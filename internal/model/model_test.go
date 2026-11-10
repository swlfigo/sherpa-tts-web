package model

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func init() { retryDelay = 10 * time.Millisecond }

func testManifest(files map[string]string, urls ...string) *Manifest {
	m := &Manifest{ID: "test-model", Name: "Test", Revision: "r1"}
	for i, u := range urls {
		m.Sources = append(m.Sources, Source{Name: "src" + string(rune('A'+i)), URL: u})
	}
	for p, body := range files {
		sum := sha256.Sum256([]byte(body))
		m.Files = append(m.Files, File{Path: p, Size: int64(len(body)), SHA256: hex.EncodeToString(sum[:])})
	}
	return m
}

func serve(files map[string]string, ranged *atomic.Int32) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") != "" && ranged != nil {
			ranged.Add(1)
		}
		body, ok := files[strings.TrimPrefix(r.URL.Path, "/")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		http.ServeContent(w, r, "", time.Time{}, strings.NewReader(body))
	}))
}

func TestDownloadResumesFallsBackAndVerifies(t *testing.T) {
	big := strings.Repeat("0123456789", 50000)
	files := map[string]string{"model.onnx": big, "espeak-ng-data/en dict": "hello"}
	var ranged atomic.Int32
	good := serve(files, &ranged)
	defer good.Close()
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no", http.StatusBadGateway)
	}))
	defer broken.Close()

	root := t.TempDir()
	m := testManifest(files, broken.URL+"/", good.URL+"/")

	// A partial file from an earlier attempt is resumed, not restarted.
	staging := m.Dir(root) + ".partial"
	os.MkdirAll(staging, 0o755)
	os.WriteFile(filepath.Join(staging, "model.onnx"), []byte(big[:200000]), 0o644)

	var last Progress
	if err := m.Download(context.Background(), root, m.Sources, func(p Progress) { last = p }); err != nil {
		t.Fatal(err)
	}
	if !m.Installed(root) {
		t.Fatal("not installed")
	}
	if ranged.Load() == 0 {
		t.Error("partial file was not resumed with a range request")
	}
	if last.Done != m.TotalSize() {
		t.Errorf("progress %d / %d", last.Done, m.TotalSize())
	}

	bad := testManifest(map[string]string{"x.bin": "expected"}, good.URL+"/")
	files["x.bin"] = "tampered"
	if err := bad.Download(context.Background(), t.TempDir(), bad.Sources, func(Progress) {}); err == nil || !strings.Contains(err.Error(), ErrChecksum.Error()) {
		t.Errorf("err = %v, want a checksum failure", err)
	}
}

func TestCheck(t *testing.T) {
	m := testManifest(map[string]string{"a.txt": "abc", "d/b.txt": "de"})
	dir := t.TempDir()
	if err := m.Check(dir); err == nil {
		t.Error("empty dir passed")
	}
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("abc"), 0o644)
	os.MkdirAll(filepath.Join(dir, "d"), 0o755)
	os.WriteFile(filepath.Join(dir, "d", "b.txt"), []byte("x"), 0o644)
	if err := m.Check(dir); err == nil || !strings.Contains(err.Error(), "d/b.txt") {
		t.Errorf("wrong size not caught: %v", err)
	}
	os.WriteFile(filepath.Join(dir, "d", "b.txt"), []byte("de"), 0o644)
	if err := m.Check(dir); err != nil {
		t.Error(err)
	}
}

func TestEmbedded(t *testing.T) {
	m, err := Embedded()
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Files) < 300 || len(m.Sources) == 0 || m.TotalSize() < 300e6 {
		t.Errorf("manifest: %d files, %d sources, %d bytes", len(m.Files), len(m.Sources), m.TotalSize())
	}
	for _, s := range m.Sources {
		if !strings.HasSuffix(s.URL, "/") || !strings.Contains(s.URL, m.Revision) {
			t.Errorf("source %s: %s", s.Name, s.URL)
		}
	}
}
