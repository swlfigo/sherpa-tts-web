//go:build cgo

package tts

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	sherpa "github.com/k2-fsa/sherpa-onnx-go/sherpa_onnx"
)

type sherpaEngine struct {
	mu  sync.Mutex // OfflineTts is not safe for concurrent use
	tts *sherpa.OfflineTts
}

// Load loads the Kokoro multi-lingual model in dir, running inference on
// threads CPU threads.
func Load(dir string, threads int) (Synthesizer, error) {
	p := func(name string) string { return filepath.Join(dir, name) }
	if _, err := os.Stat(p("model.onnx")); err != nil {
		return nil, fmt.Errorf("model not found: %w", err)
	}

	cfg := sherpa.OfflineTtsConfig{}
	k := &cfg.Model.Kokoro
	k.Model = p("model.onnx")
	k.Voices = p("voices.bin")
	k.Tokens = p("tokens.txt")
	k.DataDir = p("espeak-ng-data")
	k.DictDir = p("dict")
	k.Lexicon = p("lexicon-us-en.txt") + "," + p("lexicon-zh.txt")
	k.LengthScale = 1.0
	cfg.RuleFsts = strings.Join([]string{p("date-zh.fst"), p("number-zh.fst"), p("phone-zh.fst")}, ",")
	cfg.Model.NumThreads = max(1, threads)
	cfg.Model.Provider = "cpu"
	cfg.MaxNumSentences = 1

	tts := sherpa.NewOfflineTts(&cfg)
	if tts == nil {
		return nil, fmt.Errorf("failed to load the model from %s", dir)
	}
	return &sherpaEngine{tts: tts}, nil
}

func (e *sherpaEngine) Generate(ctx context.Context, text string, sid int, speed float32, progress func(float64)) (*Audio, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.tts == nil {
		return nil, fmt.Errorf("engine closed")
	}
	if sid < 0 || sid >= e.tts.NumSpeakers() {
		return nil, fmt.Errorf("speaker %d out of range", sid)
	}
	out := e.tts.GenerateWithProgressCallback(text, sid, speed, func(_ []float32, p float32) bool {
		if progress != nil {
			progress(float64(p))
		}
		return ctx.Err() == nil // false stops the generation
	})
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if out == nil || len(out.Samples) == 0 {
		return nil, fmt.Errorf("no audio was generated")
	}
	return &Audio{Samples: out.Samples, SampleRate: out.SampleRate}, nil
}

func (e *sherpaEngine) Close() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.tts != nil {
		sherpa.DeleteOfflineTts(e.tts)
		e.tts = nil
	}
}
