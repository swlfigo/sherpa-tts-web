package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"github.com/swlfigo/sherpa-tts-web/internal/model"
	"github.com/swlfigo/sherpa-tts-web/internal/tts"
)

// fakeSynth makes a second of silence per call. When block is set, it
// waits for it to close, or for the job to be canceled.
type fakeSynth struct {
	calls atomic.Int32
	block chan struct{}
}

func (f *fakeSynth) Generate(ctx context.Context, text string, sid int, speed float32, progress func(float64)) (*tts.Audio, error) {
	f.calls.Add(1)
	if progress != nil {
		progress(0.5)
	}
	if f.block != nil {
		select {
		case <-f.block:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return &tts.Audio{Samples: make([]float32, 24000), SampleRate: 24000}, nil
}

func (f *fakeSynth) Close() {}

type readyEngine struct{}

func (readyEngine) Status() EngineStatus { return EngineStatus{State: ModelReady} }

var static = fstest.MapFS{"index.html": {Data: []byte("<!doctype html><title>t</title>")}}

func newTestServer(t *testing.T, synth tts.Synthesizer, password string) (*httptest.Server, *Queue) {
	t.Helper()
	q := NewQueue(QueueConfig{}, func() tts.Synthesizer { return synth })
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go q.Run(ctx)
	h := New(Config{Password: password, MaxChars: 50, Version: "test"}, readyEngine{}, q, static)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv, q
}

func do(t *testing.T, method, url, body, password string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(method, url, strings.NewReader(body))
	if password != "" {
		req.SetBasicAuth("any", password)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func decode[T any](t *testing.T, resp *http.Response) T {
	t.Helper()
	var v T
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestJobFlow(t *testing.T) {
	srv, q := newTestServer(t, &fakeSynth{}, "")
	resp := do(t, "POST", srv.URL+"/api/jobs", `{"text":"你好，世界","voice":"zf_xiaoxiao","speed":1.2}`, "")
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("submit: %d", resp.StatusCode)
	}
	id := decode[map[string]string](t, resp)["id"]

	var st JobStatus
	deadline := time.Now().Add(5 * time.Second)
	for st.State != JobDone {
		if time.Now().After(deadline) {
			t.Fatalf("job stuck in %q", st.State)
		}
		st = decode[JobStatus](t, do(t, "GET", srv.URL+"/api/jobs/"+id, "", ""))
		time.Sleep(10 * time.Millisecond)
	}
	if st.Duration != 1 || !strings.HasPrefix(st.Filename, "你好_世界_zf_xiaoxiao_") {
		t.Errorf("status %+v", st)
	}

	resp = do(t, "GET", srv.URL+"/api/jobs/"+id+"/audio", "", "")
	var buf bytes.Buffer
	buf.ReadFrom(resp.Body)
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "audio/wav" || buf.Len() != 44+2*24000 {
		t.Fatalf("audio: %d %s %d bytes", resp.StatusCode, resp.Header.Get("Content-Type"), buf.Len())
	}
	if cd := resp.Header.Get("Content-Disposition"); !strings.Contains(cd, "filename*=UTF-8''%E4%BD%A0") {
		t.Errorf("Content-Disposition %q", cd)
	}

	// The server keeps nothing once the audio is taken.
	if r := do(t, "GET", srv.URL+"/api/jobs/"+id+"/audio", "", ""); r.StatusCode != http.StatusNotFound {
		t.Errorf("second fetch: %d", r.StatusCode)
	}
	q.mu.Lock()
	n, held := len(q.jobs), q.resultBytes
	q.mu.Unlock()
	if n != 0 || held != 0 {
		t.Errorf("queue still holds %d jobs, %d bytes", n, held)
	}
}

func TestValidation(t *testing.T) {
	srv, _ := newTestServer(t, &fakeSynth{}, "")
	cases := map[string]string{
		`{"text":"   "}`: "请输入文字",
		`{"text":"` + strings.Repeat("字", 51) + `"}`: "文字太长",
		`{"text":"hi","voice":"nobody"}`:             "不支持的音色",
		`{"text":"hi","speed":3}`:                    "语速",
		`not json`:                                   "请求格式不正确",
	}
	for body, want := range cases {
		resp := do(t, "POST", srv.URL+"/api/jobs", body, "")
		got := decode[map[string]string](t, resp)["error"]
		if resp.StatusCode != http.StatusBadRequest || !strings.Contains(got, want) {
			t.Errorf("%s: %d %q, want %q", body, resp.StatusCode, got, want)
		}
	}
}

func TestPassword(t *testing.T) {
	srv, _ := newTestServer(t, &fakeSynth{}, "s3cret")
	if r := do(t, "GET", srv.URL+"/", "", ""); r.StatusCode != http.StatusUnauthorized || r.Header.Get("WWW-Authenticate") == "" {
		t.Errorf("no password: %d", r.StatusCode)
	}
	if r := do(t, "GET", srv.URL+"/api/status", "", "wrong"); r.StatusCode != http.StatusUnauthorized {
		t.Errorf("wrong password: %d", r.StatusCode)
	}
	if r := do(t, "GET", srv.URL+"/", "", "s3cret"); r.StatusCode != http.StatusOK {
		t.Errorf("right password: %d", r.StatusCode)
	}
	if r := do(t, "GET", srv.URL+"/healthz", "", ""); r.StatusCode != http.StatusOK {
		t.Errorf("healthz: %d", r.StatusCode)
	}
}

func TestPreviewIsCached(t *testing.T) {
	synth := &fakeSynth{}
	srv, _ := newTestServer(t, synth, "")
	for range 2 {
		r := do(t, "POST", srv.URL+"/api/preview", `{"voice":"bf_emma","speed":1.04}`, "")
		if r.StatusCode != 200 || r.Header.Get("Content-Type") != "audio/wav" {
			t.Fatalf("preview: %d", r.StatusCode)
		}
	}
	if n := synth.calls.Load(); n != 1 {
		t.Errorf("synthesized %d times", n)
	}
}

func TestStatusAndStatic(t *testing.T) {
	srv, _ := newTestServer(t, &fakeSynth{}, "")
	st := decode[struct {
		Model    EngineStatus
		MaxChars int
		Version  string
	}](t, do(t, "GET", srv.URL+"/api/status", "", ""))
	if st.Model.State != ModelReady || st.MaxChars != 50 || st.Version != "test" {
		t.Errorf("status %+v", st)
	}
	r := do(t, "GET", srv.URL+"/", "", "")
	if r.StatusCode != 200 || r.Header.Get("Content-Security-Policy") == "" {
		t.Errorf("index: %d", r.StatusCode)
	}
	voices := decode[struct {
		Default string
		Voices  []tts.Voice
	}](t, do(t, "GET", srv.URL+"/api/voices", "", ""))
	if voices.Default != tts.DefaultVoice || len(voices.Voices) != len(tts.Voices) {
		t.Errorf("voices %+v", voices)
	}
}

func TestNotReady(t *testing.T) {
	q := NewQueue(QueueConfig{}, func() tts.Synthesizer { return nil })
	srv := httptest.NewServer(New(Config{}, readyEngine{}, q, static))
	defer srv.Close()
	r := do(t, "POST", srv.URL+"/api/jobs", `{"text":"hi"}`, "")
	if r.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status %d", r.StatusCode)
	}
}

func TestEngineDownloadsThenLoads(t *testing.T) {
	files := map[string]string{"model.onnx": strings.Repeat("m", 5000), "dict/a.txt": "abc"}
	src := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := files[strings.TrimPrefix(r.URL.Path, "/m/")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(body))
	}))
	defer src.Close()

	m, _ := model.Embedded()
	m.Files = nil
	for p, body := range files {
		m.Files = append(m.Files, model.File{Path: p, Size: int64(len(body)), SHA256: sha(body)})
	}
	data := t.TempDir()
	var loadedFrom string
	e := NewEngine(EngineConfig{
		Manifest: m,
		DataDir:  data,
		Sources:  []model.Source{{Name: "test", URL: src.URL + "/m/"}},
		Load: func(dir string, threads int) (tts.Synthesizer, error) {
			loadedFrom = dir
			return &fakeSynth{}, nil
		},
	})
	e.Run(context.Background())
	if st := e.Status(); st.State != ModelReady || e.Synth() == nil {
		t.Fatalf("status %+v", st)
	}
	if loadedFrom != m.Dir(data) {
		t.Errorf("loaded from %s", loadedFrom)
	}

	// A model directory that lacks files is reported, not downloaded into.
	bad := NewEngine(EngineConfig{Manifest: m, ModelDir: t.TempDir(), Load: e.cfg.Load})
	bad.Run(context.Background())
	if st := bad.Status(); st.State != ModelFailed || !strings.Contains(st.Error, "missing") {
		t.Errorf("status %+v", st)
	}
	os.RemoveAll(filepath.Join(data, "models"))
}
