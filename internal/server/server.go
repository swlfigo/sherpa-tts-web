// Package server is the web service: a page and the JSON API it calls.
package server

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/swlfigo/sherpa-tts-web/internal/tts"
)

// Config configures the service.
type Config struct {
	// Password, when set, is asked for with HTTP basic authentication; the
	// user name is not checked.
	Password string
	MaxChars int // longest text accepted, in characters
	Version  string
	Logger   *slog.Logger
}

// EngineInfo reports the model's state; *Engine is one.
type EngineInfo interface {
	Status() EngineStatus
}

type service struct {
	cfg      Config
	engine   EngineInfo
	queue    *Queue
	previews *previewCache
	static   fs.FS
}

// New returns the service's handler. static holds the page.
func New(cfg Config, engine EngineInfo, queue *Queue, static fs.FS) http.Handler {
	if cfg.MaxChars <= 0 {
		cfg.MaxChars = 5000
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	s := &service{cfg: cfg, engine: engine, queue: queue, previews: newPreviewCache(64), static: static}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok\n")) })
	mux.HandleFunc("GET /api/status", s.status)
	mux.HandleFunc("GET /api/voices", s.voices)
	mux.HandleFunc("POST /api/jobs", s.createJob)
	mux.HandleFunc("GET /api/jobs/{id}", s.jobStatus)
	mux.HandleFunc("GET /api/jobs/{id}/audio", s.jobAudio)
	mux.HandleFunc("POST /api/jobs/{id}/cancel", s.cancelJob)
	mux.HandleFunc("POST /api/preview", s.preview)
	files := http.FileServerFS(static)
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		files.ServeHTTP(w, r)
	})
	return s.logRequests(securityHeaders(s.authenticate(mux)))
}

func (s *service) status(w http.ResponseWriter, r *http.Request) {
	pending, running := s.queue.Info()
	writeJSON(w, http.StatusOK, map[string]any{
		"model":    s.engine.Status(),
		"queue":    map[string]any{"pending": pending, "running": running},
		"maxChars": s.cfg.MaxChars,
		"version":  s.cfg.Version,
	})
}

func (s *service) voices(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"default": tts.DefaultVoice, "voices": tts.Voices})
}

func (s *service) createJob(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Text  string  `json:"text"`
		Voice string  `json:"voice"`
		Speed float64 `json:"speed"`
	}
	if !s.decode(w, r, &in) {
		return
	}
	text := strings.TrimSpace(in.Text)
	if text == "" {
		writeError(w, http.StatusBadRequest, "请输入文字")
		return
	}
	if n := utf8.RuneCountInString(text); n > s.cfg.MaxChars {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("文字太长：%d 字，最多 %d 字", n, s.cfg.MaxChars))
		return
	}
	voice, speed, msg := parseVoice(in.Voice, in.Speed)
	if msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	id, err := s.queue.Submit(Request{Text: text, Voice: voice, Speed: speed}, false)
	if err != nil {
		s.submitError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"id": id})
}

func (s *service) jobStatus(w http.ResponseWriter, r *http.Request) {
	st, ok := s.queue.Status(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "任务不存在或已过期")
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// jobAudio sends a job's audio once: the server keeps no copy after.
func (s *service) jobAudio(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	wav, name, ok := s.queue.Result(id)
	if !ok {
		writeError(w, http.StatusNotFound, "音频不存在或已过期")
		return
	}
	if writeWAV(w, wav, name) == nil {
		s.queue.Remove(id)
	}
}

func (s *service) cancelJob(w http.ResponseWriter, r *http.Request) {
	if !s.queue.Cancel(r.PathValue("id")) {
		writeError(w, http.StatusNotFound, "任务不存在或已过期")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// preview says a sentence in a voice. Previews jump the queue and are
// cached in memory, as there are few of them.
func (s *service) preview(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Voice string  `json:"voice"`
		Speed float64 `json:"speed"`
	}
	if !s.decode(w, r, &in) {
		return
	}
	voice, speed, msg := parseVoice(in.Voice, in.Speed)
	if msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	key := fmt.Sprintf("%s@%.1f", voice.Name, speed)
	if wav, ok := s.previews.get(key); ok {
		writeWAV(w, wav, "preview_"+voice.Name+".wav")
		return
	}
	id, err := s.queue.Submit(Request{Text: tts.PreviewText(voice), Voice: voice, Speed: speed}, true)
	if err != nil {
		s.submitError(w, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	st, err := s.queue.Wait(ctx, id)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "服务器正忙，请稍后再试")
		return
	}
	wav, _, ok := s.queue.Result(id)
	s.queue.Remove(id)
	if st.State != JobDone || !ok {
		writeError(w, http.StatusInternalServerError, "试听失败："+st.Error)
		return
	}
	s.previews.put(key, wav)
	writeWAV(w, wav, "preview_"+voice.Name+".wav")
}

func (s *service) decode(w http.ResponseWriter, r *http.Request, v any) bool {
	// Characters take up to 4 bytes in UTF-8, and JSON escapes more.
	r.Body = http.MaxBytesReader(w, r.Body, int64(s.cfg.MaxChars)*8+4096)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeError(w, http.StatusRequestEntityTooLarge, "文字太长")
		} else {
			writeError(w, http.StatusBadRequest, "请求格式不正确")
		}
		return false
	}
	return true
}

func (s *service) submitError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrNotReady):
		writeError(w, http.StatusServiceUnavailable, "语音模型还没准备好，请稍后再试")
	case errors.Is(err, ErrQueueFull):
		w.Header().Set("Retry-After", "30")
		writeError(w, http.StatusServiceUnavailable, "排队的任务太多，请稍后再试")
	default:
		writeError(w, http.StatusInternalServerError, err.Error())
	}
}

func parseVoice(name string, speed float64) (tts.Voice, float32, string) {
	if name == "" {
		name = tts.DefaultVoice
	}
	v, ok := tts.VoiceByName(name)
	if !ok {
		return tts.Voice{}, 0, "不支持的音色"
	}
	if speed == 0 {
		speed = 1
	}
	if math.IsNaN(speed) || speed < 0.5 || speed > 2 {
		return tts.Voice{}, 0, "语速应在 0.5 到 2 之间"
	}
	return v, float32(math.Round(speed*10) / 10), ""
}

func writeWAV(w http.ResponseWriter, wav []byte, name string) error {
	h := w.Header()
	h.Set("Content-Type", "audio/wav")
	h.Set("Content-Length", strconv.Itoa(len(wav)))
	h.Set("Cache-Control", "no-store")
	h.Set("Content-Disposition", `attachment; filename="speech.wav"; filename*=UTF-8''`+url.PathEscape(name))
	_, err := w.Write(wav)
	return err
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

// authenticate asks for the password, when one is set, on every path but
// the health check.
func (s *service) authenticate(next http.Handler) http.Handler {
	if s.cfg.Password == "" {
		return next
	}
	want := sha256.Sum256([]byte(s.cfg.Password))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			_, pass, ok := r.BasicAuth()
			got := sha256.Sum256([]byte(pass))
			if !ok || subtle.ConstantTimeCompare(got[:], want[:]) != 1 {
				w.Header().Set("WWW-Authenticate", `Basic realm="TTS", charset="UTF-8"`)
				writeError(w, http.StatusUnauthorized, "需要密码")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; "+
			"img-src 'self' data:; media-src 'self' blob:; connect-src 'self'; "+
			"base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
		next.ServeHTTP(w, r)
	})
}

// logRequests logs changes and errors, never the text spoken.
func (s *service) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, code: http.StatusOK}
		next.ServeHTTP(rec, r)
		if r.Method != http.MethodGet || rec.code >= 400 {
			s.cfg.Logger.Info("request", "method", r.Method, "path", r.URL.Path,
				"status", rec.code, "took", time.Since(start).Round(time.Millisecond))
		}
	})
}

type statusRecorder struct {
	http.ResponseWriter
	code int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.code = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// previewCache keeps the latest previews.
type previewCache struct {
	mu    sync.Mutex
	max   int
	items map[string][]byte
	order []string
}

func newPreviewCache(max int) *previewCache {
	return &previewCache{max: max, items: map[string][]byte{}}
}

func (c *previewCache) get(key string) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	b, ok := c.items[key]
	return b, ok
}

func (c *previewCache) put(key string, b []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.items[key]; ok {
		return
	}
	if len(c.order) >= c.max {
		delete(c.items, c.order[0])
		c.order = c.order[1:]
	}
	c.items[key] = b
	c.order = append(c.order, key)
}
