package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"testing"
	"time"

	"github.com/swlfigo/sherpa-tts-web/internal/tts"
)

func sha(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

var testVoice, _ = tts.VoiceByName(tts.DefaultVoice)

func req(text string) Request { return Request{Text: text, Voice: testVoice, Speed: 1} }

func waitState(t *testing.T, q *Queue, id string, want JobState) JobStatus {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		st, ok := q.Status(id)
		if ok && st.State == want {
			return st
		}
		if time.Now().After(deadline) {
			t.Fatalf("job %s: %+v (found %v), want %s", id, st, ok, want)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func TestQueueOrderPriorityAndCancel(t *testing.T) {
	synth := &fakeSynth{block: make(chan struct{})}
	q := NewQueue(QueueConfig{}, func() tts.Synthesizer { return synth })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go q.Run(ctx)

	first, _ := q.Submit(req("one"), false)
	waitState(t, q, first, JobRunning)
	second, _ := q.Submit(req("two"), false)
	third, _ := q.Submit(req("three"), false)
	preview, _ := q.Submit(req("preview"), true)

	if st, _ := q.Status(third); st.Ahead != 3 { // running, second, and the preview
		t.Errorf("third: ahead %d", st.Ahead)
	}
	if st, _ := q.Status(preview); st.Ahead != 1 {
		t.Errorf("preview: ahead %d", st.Ahead)
	}

	// Cancelling a queued job takes it out at once.
	q.Cancel(second)
	waitState(t, q, second, JobCanceled)

	// Cancelling the running job stops it; the preview runs next.
	q.Cancel(first)
	waitState(t, q, first, JobCanceled)
	waitState(t, q, preview, JobRunning)
	close(synth.block)
	waitState(t, q, preview, JobDone)
	waitState(t, q, third, JobDone)
	if wav, name, ok := q.Result(third); !ok || len(wav) != 44+2*24000 || name == "" {
		t.Errorf("result: %v %q %d bytes", ok, name, len(wav))
	}
}

func TestQueueFull(t *testing.T) {
	synth := &fakeSynth{block: make(chan struct{})}
	defer close(synth.block)
	q := NewQueue(QueueConfig{MaxPending: 2}, func() tts.Synthesizer { return synth })
	for i := range 2 {
		if _, err := q.Submit(req("x"), false); err != nil {
			t.Fatalf("submit %d: %v", i, err)
		}
	}
	if _, err := q.Submit(req("x"), false); err != ErrQueueFull {
		t.Errorf("err = %v", err)
	}
}

func TestSweepAbandonedAndExpired(t *testing.T) {
	synth := &fakeSynth{}
	q := NewQueue(QueueConfig{ResultTTL: time.Minute, AbandonAfter: 30 * time.Second}, func() tts.Synthesizer { return synth })
	now := time.Unix(1_000_000, 0)
	q.now = func() time.Time { return now }

	// Not running: jobs stay queued, so only the sweep acts on them.
	abandoned, _ := q.Submit(req("nobody asks"), false)
	watched, _ := q.Submit(req("polled"), false)
	now = now.Add(20 * time.Second)
	q.Status(watched)
	now = now.Add(15 * time.Second)
	q.sweep()
	if st, _ := q.Status(abandoned); st.State != JobCanceled {
		t.Errorf("abandoned job %s", st.State)
	}
	if st, _ := q.Status(watched); st.State != JobQueued {
		t.Errorf("watched job %s", st.State)
	}

	// Finished jobs go after the TTL, results and all.
	j := q.jobs[watched]
	q.mu.Lock()
	q.pending = nil
	j.state = JobRunning
	q.mu.Unlock()
	q.process(j)
	q.mu.Lock()
	held := q.resultBytes
	q.mu.Unlock()
	if held == 0 {
		t.Fatal("no result held")
	}
	now = now.Add(2 * time.Minute)
	q.sweep()
	if _, ok := q.Status(watched); ok {
		t.Error("expired job still there")
	}
	if q.resultBytes != 0 || len(q.jobs) != 0 {
		t.Errorf("left %d jobs, %d bytes", len(q.jobs), q.resultBytes)
	}
}

func TestResultMemoryCap(t *testing.T) {
	one := 44 + 2*24000
	q := NewQueue(QueueConfig{MaxResultBytes: 2*one + 10}, func() tts.Synthesizer { return &fakeSynth{} })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go q.Run(ctx)
	var ids []string
	for range 3 {
		id, _ := q.Submit(req("x"), false)
		waitState(t, q, id, JobDone)
		ids = append(ids, id)
	}
	if _, _, ok := q.Result(ids[0]); ok {
		t.Error("oldest result kept past the cap")
	}
	for _, id := range ids[1:] {
		if _, _, ok := q.Result(id); !ok {
			t.Errorf("result %s dropped", id)
		}
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.resultBytes != 2*one {
		t.Errorf("resultBytes %d", q.resultBytes)
	}
}
