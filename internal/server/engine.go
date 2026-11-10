package server

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/swlfigo/sherpa-tts-web/internal/model"
	"github.com/swlfigo/sherpa-tts-web/internal/tts"
)

// ModelState is where the speech model is in getting ready.
type ModelState string

const (
	ModelDownloading ModelState = "downloading"
	ModelLoading     ModelState = "loading"
	ModelReady       ModelState = "ready"
	ModelFailed      ModelState = "failed"
)

// EngineStatus is what the page shows of the model.
type EngineStatus struct {
	Name        string     `json:"name"`
	State       ModelState `json:"state"`
	Done        int64      `json:"done"`
	Total       int64      `json:"total"`
	BytesPerSec float64    `json:"bytesPerSec"`
	Error       string     `json:"error,omitempty"`
}

// EngineConfig says where the model is, or where to download it.
type EngineConfig struct {
	Manifest *model.Manifest
	// ModelDir, when set, holds the model already: it is used as it is.
	ModelDir string
	// DataDir is where the model is downloaded to when ModelDir is empty.
	DataDir string
	Sources []model.Source
	Threads int
	// Load loads the model of a directory; tts.Load in the program.
	Load       func(dir string, threads int) (tts.Synthesizer, error)
	RetryDelay time.Duration // between failed downloads, a minute by default
	Logger     *slog.Logger
}

// Engine gets the model ready, downloading it first if needed, and holds
// the synthesizer once it is.
type Engine struct {
	cfg EngineConfig

	mu    sync.Mutex
	st    EngineStatus
	synth tts.Synthesizer
}

// NewEngine returns an engine that Run gets ready.
func NewEngine(cfg EngineConfig) *Engine {
	if cfg.RetryDelay <= 0 {
		cfg.RetryDelay = time.Minute
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Engine{cfg: cfg, st: EngineStatus{
		Name:  cfg.Manifest.Name,
		State: ModelLoading,
		Total: cfg.Manifest.TotalSize(),
	}}
}

// Status returns a snapshot of the model's state.
func (e *Engine) Status() EngineStatus {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.st
}

// Synth returns the synthesizer, or nil while the model is not ready.
func (e *Engine) Synth() tts.Synthesizer {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.synth
}

// Run makes the model ready: it downloads it when needed, retrying until
// ctx ends, then loads it.
func (e *Engine) Run(ctx context.Context) {
	log := e.cfg.Logger
	dir, err := e.prepare(ctx)
	if err != nil {
		if ctx.Err() == nil {
			log.Error("model unavailable", "err", err)
			e.update(func(s *EngineStatus) { s.State, s.Error = ModelFailed, err.Error() })
		}
		return
	}
	e.update(func(s *EngineStatus) { s.State, s.Done, s.BytesPerSec, s.Error = ModelLoading, s.Total, 0, "" })
	log.Info("loading model", "dir", dir, "threads", e.cfg.Threads)
	start := time.Now()
	synth, err := e.cfg.Load(dir, e.cfg.Threads)
	if err != nil {
		log.Error("model failed to load", "err", err)
		e.update(func(s *EngineStatus) { s.State, s.Error = ModelFailed, err.Error() })
		return
	}
	e.mu.Lock()
	e.synth, e.st.State = synth, ModelReady
	e.mu.Unlock()
	log.Info("model ready", "took", time.Since(start).Round(time.Millisecond))
}

func (e *Engine) prepare(ctx context.Context) (string, error) {
	m := e.cfg.Manifest
	if e.cfg.ModelDir != "" {
		if err := m.Check(e.cfg.ModelDir); err != nil {
			return "", fmt.Errorf("model directory %s: %w", e.cfg.ModelDir, err)
		}
		return e.cfg.ModelDir, nil
	}
	if m.Installed(e.cfg.DataDir) {
		return m.Dir(e.cfg.DataDir), nil
	}
	log := e.cfg.Logger
	for {
		e.update(func(s *EngineStatus) { s.State = ModelDownloading })
		names := make([]string, len(e.cfg.Sources))
		for i, s := range e.cfg.Sources {
			names[i] = s.Name
		}
		log.Info("downloading model", "size_mb", m.TotalSize()/1e6, "sources", names)
		err := m.Download(ctx, e.cfg.DataDir, e.cfg.Sources, func(p model.Progress) {
			e.update(func(s *EngineStatus) { s.Done, s.Total, s.BytesPerSec = p.Done, p.Total, p.BytesPerSec })
		})
		if err == nil {
			return m.Dir(e.cfg.DataDir), nil
		}
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		delay := e.cfg.RetryDelay
		log.Warn("model download failed", "err", err, "retry_in", delay)
		e.update(func(s *EngineStatus) {
			s.BytesPerSec = 0
			s.Error = fmt.Sprintf("download failed, retrying in %s: %v", delay, err)
		})
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
}

func (e *Engine) update(fn func(*EngineStatus)) {
	e.mu.Lock()
	fn(&e.st)
	e.mu.Unlock()
}

// Close frees the model.
func (e *Engine) Close() {
	e.mu.Lock()
	s := e.synth
	e.synth = nil
	e.mu.Unlock()
	if s != nil {
		s.Close()
	}
}
