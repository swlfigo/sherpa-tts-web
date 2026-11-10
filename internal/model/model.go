// Package model describes the speech model's files and downloads them:
// resumably, from the first source that has them, each checked against its
// SHA-256 before the model is moved into place.
package model

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// manifest.json lists the model's files with their sizes and SHA-256, at a
// pinned revision. tools/genmanifest writes it.
//
//go:embed manifest.json
var manifestJSON []byte

// Manifest is a model: its files and where to download them.
type Manifest struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Revision string   `json:"revision"`
	Sources  []Source `json:"sources"`
	Files    []File   `json:"files"`
}

// Source is a base URL, ending in "/", to which a file's path is appended.
type Source struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

// File is one file of the model.
type File struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// Embedded returns the manifest built into the program.
func Embedded() (*Manifest, error) {
	var m Manifest
	if err := json.Unmarshal(manifestJSON, &m); err != nil {
		return nil, fmt.Errorf("manifest: %w", err)
	}
	return &m, nil
}

// TotalSize is the size of all the files.
func (m *Manifest) TotalSize() int64 {
	var n int64
	for _, f := range m.Files {
		n += f.Size
	}
	return n
}

// Dir is where Download installs the model under root.
func (m *Manifest) Dir(root string) string {
	return filepath.Join(root, "models", m.ID, m.Revision)
}

// Check reports the first file of the model that dir lacks, or holds with
// another size. It does not read the files, so it stays instant.
func (m *Manifest) Check(dir string) error {
	for _, f := range m.Files {
		st, err := os.Stat(filepath.Join(dir, filepath.FromSlash(f.Path)))
		if err != nil {
			return fmt.Errorf("missing %s", f.Path)
		}
		if st.Size() != f.Size {
			return fmt.Errorf("%s has %d bytes, want %d", f.Path, st.Size(), f.Size)
		}
	}
	return nil
}

const completeMarker = ".complete"

// Installed reports whether a finished download of the model is in root.
// Files were verified as they were downloaded.
func (m *Manifest) Installed(root string) bool {
	dir := m.Dir(root)
	if _, err := os.Stat(filepath.Join(dir, completeMarker)); err != nil {
		return false
	}
	return m.Check(dir) == nil
}

// Progress is a snapshot of a download.
type Progress struct {
	Done, Total int64
	BytesPerSec float64
}

// UserAgent is sent with downloads.
var UserAgent = "sherpa-tts-web"

// retryDelay is the wait before the second attempt at a file, doubled for
// the third.
var retryDelay = 2 * time.Second

// ErrChecksum means a downloaded file does not match its SHA-256.
var ErrChecksum = errors.New("checksum mismatch")

type statusError int

func (e statusError) Error() string {
	return fmt.Sprintf("HTTP %d %s", int(e), http.StatusText(int(e)))
}

// Download fetches the model into root. sources are tried in order for each
// file; partial files are resumed with range requests. progress is called
// from another goroutine about four times a second, and once at the end.
func (m *Manifest) Download(ctx context.Context, root string, sources []Source, progress func(Progress)) error {
	if len(sources) == 0 {
		return errors.New("no download source")
	}
	final := m.Dir(root)
	staging := final + ".partial"
	if err := os.MkdirAll(staging, 0o755); err != nil {
		return err
	}

	total := m.TotalSize()
	var done atomic.Int64
	for _, f := range m.Files {
		if st, err := os.Stat(filepath.Join(staging, filepath.FromSlash(f.Path))); err == nil {
			done.Add(min(st.Size(), f.Size))
		}
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	stopReport := make(chan struct{})
	reported := make(chan struct{})
	go func() {
		defer close(reported)
		t := time.NewTicker(250 * time.Millisecond)
		defer t.Stop()
		last, lastAt := done.Load(), time.Now()
		var speed float64
		for {
			select {
			case <-stopReport:
				progress(Progress{Done: done.Load(), Total: total, BytesPerSec: speed})
				return
			case now := <-t.C:
				d := done.Load()
				if dt := now.Sub(lastAt).Seconds(); dt > 0 {
					speed = 0.7*speed + 0.3*float64(d-last)/dt
				}
				last, lastAt = d, now
				progress(Progress{Done: d, Total: total, BytesPerSec: speed})
			}
		}
	}()
	defer func() { close(stopReport); <-reported }()

	// Largest first, so the long download starts at once and the small
	// files fill the other workers around it.
	files := append([]File(nil), m.Files...)
	sort.Slice(files, func(i, j int) bool { return files[i].Size > files[j].Size })

	jobs := make(chan File)
	var (
		wg       sync.WaitGroup
		errOnce  sync.Once
		firstErr error
	)
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for f := range jobs {
				if err := fetchFile(ctx, staging, f, sources, &done); err != nil {
					errOnce.Do(func() { firstErr = err; cancel() })
				}
			}
		}()
	}
feed:
	for _, f := range files {
		select {
		case jobs <- f:
		case <-ctx.Done():
			break feed
		}
	}
	close(jobs)
	wg.Wait()

	if firstErr != nil {
		return firstErr
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(staging, completeMarker), []byte(m.Revision+"\n"), 0o644); err != nil {
		return err
	}
	if err := os.RemoveAll(final); err != nil {
		return err
	}
	return os.Rename(staging, final)
}

var httpClient = &http.Client{
	Transport: &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		ResponseHeaderTimeout: 30 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		IdleConnTimeout:       60 * time.Second,
	},
}

// fetchFile downloads one file into dir, trying each source a few times.
func fetchFile(ctx context.Context, dir string, f File, sources []Source, done *atomic.Int64) error {
	dest := filepath.Join(dir, filepath.FromSlash(f.Path))
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	var lastErr error
	for _, src := range sources {
		for attempt := range 3 {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if attempt > 0 {
				select {
				case <-time.After(time.Duration(attempt) * retryDelay):
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			err := fetchOnce(ctx, src.URL+escapePath(f.Path), dest, f, done)
			if err == nil {
				return nil
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			lastErr = fmt.Errorf("%s (%s): %w", f.Path, src.Name, err)
			var se statusError
			if errors.As(err, &se) && se >= 400 && se < 500 {
				break // the source does not have it: try the next one
			}
		}
	}
	return lastErr
}

func fetchOnce(ctx context.Context, rawURL, dest string, f File, done *atomic.Int64) error {
	var have int64
	if st, err := os.Stat(dest); err == nil {
		have = st.Size()
	}
	if have > f.Size {
		done.Add(-f.Size) // Download counted at most the file's size
		os.Remove(dest)
		have = 0
	}
	if have == f.Size {
		if err := verify(dest, f.SHA256); err == nil {
			return nil
		}
		done.Add(-have)
		os.Remove(dest)
		have = 0
	}

	// Give up on a connection that sends nothing for a minute.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	idle := time.AfterFunc(60*time.Second, cancel)
	defer idle.Stop()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", UserAgent)
	if have > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", have))
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	flags := os.O_CREATE | os.O_WRONLY
	switch {
	case have > 0 && resp.StatusCode == http.StatusPartialContent:
		flags |= os.O_APPEND
	case resp.StatusCode == http.StatusOK:
		flags |= os.O_TRUNC // the server ignored the range: start over
		done.Add(-have)
		have = 0
	default:
		return statusError(resp.StatusCode)
	}

	out, err := os.OpenFile(dest, flags, 0o644)
	if err != nil {
		return err
	}
	buf := make([]byte, 256<<10)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			idle.Reset(60 * time.Second)
			if _, werr := out.Write(buf[:n]); werr != nil {
				out.Close()
				return werr
			}
			done.Add(int64(n))
			have += int64(n)
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			out.Close()
			if ctx.Err() != nil && have < f.Size {
				return fmt.Errorf("connection stalled: %w", rerr)
			}
			return rerr
		}
	}
	if err := out.Close(); err != nil {
		return err
	}
	if have != f.Size {
		return fmt.Errorf("got %d of %d bytes", have, f.Size)
	}
	if err := verify(dest, f.SHA256); err != nil {
		done.Add(-have)
		os.Remove(dest)
		return err
	}
	return nil
}

func verify(path, want string) error {
	fh, err := os.Open(path)
	if err != nil {
		return err
	}
	defer fh.Close()
	h := sha256.New()
	if _, err := io.Copy(h, fh); err != nil {
		return err
	}
	if hex.EncodeToString(h.Sum(nil)) != want {
		return ErrChecksum
	}
	return nil
}

func escapePath(p string) string {
	parts := strings.Split(p, "/")
	for i, s := range parts {
		parts[i] = url.PathEscape(s)
	}
	return strings.Join(parts, "/")
}
