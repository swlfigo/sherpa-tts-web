// Command genmanifest writes the model manifest the app embeds: every file
// the runtime needs, with its size and SHA-256. It reads a local copy of the
// model and checks each file against the pinned HuggingFace revision (git
// blob SHA-1, or the LFS SHA-256 for large files), so the hashes it records
// are those the download will see.
//
//	go run ./tools/genmanifest -dir /path/to/kokoro-multi-lang-v1_0 -o internal/model/manifest.json
package main

import (
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
)

type hfModel struct {
	SHA      string `json:"sha"`
	Siblings []struct {
		Name   string `json:"rfilename"`
		Size   int64  `json:"size"`
		BlobID string `json:"blobId"`
		LFS    *struct {
			SHA256 string `json:"sha256"`
		} `json:"lfs"`
	} `json:"siblings"`
}

type manifest struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Revision string   `json:"revision"`
	Sources  []source `json:"sources"`
	Files    []file   `json:"files"`
}

type source struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

type file struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// Files the app does not need at run time.
var skip = map[string]bool{".gitattributes": true, "README.md": true, "lexicon-gb-en.txt": true}

func main() {
	dir := flag.String("dir", "", "local copy of the model")
	repo := flag.String("repo", "csukuangfj/kokoro-multi-lang-v1_0", "HuggingFace repository")
	rev := flag.String("rev", "f7b96bb6bef5c5da4d3aa4f4e0498fbbf62dc78b", "pinned revision")
	out := flag.String("o", "internal/model/manifest.json", "output file")
	flag.Parse()
	if *dir == "" {
		log.Fatal("-dir is required")
	}

	api := fmt.Sprintf("https://huggingface.co/api/models/%s/revision/%s?blobs=true", *repo, *rev)
	resp, err := http.Get(api)
	if err != nil {
		log.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		log.Fatalf("%s: %s", api, resp.Status)
	}
	var hf hfModel
	if err := json.NewDecoder(resp.Body).Decode(&hf); err != nil {
		log.Fatal(err)
	}
	if hf.SHA != *rev {
		log.Fatalf("API returned revision %s, want %s", hf.SHA, *rev)
	}

	m := manifest{
		ID:       "kokoro-multi-lang-v1_0",
		Name:     "Kokoro multi-lang v1.0 (zh/en)",
		Revision: *rev,
		Sources: []source{
			{Name: "hf-mirror.com", URL: fmt.Sprintf("https://hf-mirror.com/%s/resolve/%s/", *repo, *rev)},
			{Name: "huggingface.co", URL: fmt.Sprintf("https://huggingface.co/%s/resolve/%s/", *repo, *rev)},
		},
	}
	for _, s := range hf.Siblings {
		if skip[s.Name] {
			continue
		}
		local := filepath.Join(*dir, filepath.FromSlash(s.Name))
		data, err := os.ReadFile(local)
		if err != nil {
			log.Fatalf("%s: %v", s.Name, err)
		}
		if int64(len(data)) != s.Size {
			log.Fatalf("%s: local size %d, remote %d", s.Name, len(data), s.Size)
		}
		sum := sha256.Sum256(data)
		if s.LFS != nil {
			if hex.EncodeToString(sum[:]) != s.LFS.SHA256 {
				log.Fatalf("%s: LFS sha256 mismatch", s.Name)
			}
		} else if gitBlobID(data) != s.BlobID {
			log.Fatalf("%s: git blob id mismatch", s.Name)
		}
		m.Files = append(m.Files, file{Path: s.Name, Size: s.Size, SHA256: hex.EncodeToString(sum[:])})
	}
	sort.Slice(m.Files, func(i, j int) bool { return m.Files[i].Path < m.Files[j].Path })

	var total int64
	for _, f := range m.Files {
		total += f.Size
	}
	b, _ := json.MarshalIndent(m, "", "  ")
	if err := os.WriteFile(*out, append(b, '\n'), 0o644); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("wrote %s: %d files, %.1f MB\n", *out, len(m.Files), float64(total)/1e6)
}

func gitBlobID(data []byte) string {
	h := sha1.New()
	io.WriteString(h, "blob "+fmt.Sprint(len(data))+"\x00")
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}
