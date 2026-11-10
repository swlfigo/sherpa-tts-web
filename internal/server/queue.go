package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"slices"
	"sync"
	"time"

	"github.com/swlfigo/sherpa-tts-web/internal/tts"
)

// JobState is where a job is.
type JobState string

const (
	JobQueued   JobState = "queued"
	JobRunning  JobState = "running"
	JobDone     JobState = "done"
	JobFailed   JobState = "failed"
	JobCanceled JobState = "canceled"
)

func (s JobState) finished() bool { return s == JobDone || s == JobFailed || s == JobCanceled }

// Request is what to say, and how.
type Request struct {
	Text  string
	Voice tts.Voice
	Speed float32
}

// JobStatus is what a client sees of a job.
type JobStatus struct {
	ID       string   `json:"id"`
	State    JobState `json:"state"`
	Ahead    int      `json:"ahead"` // jobs that run before it, while queued
	Progress float64  `json:"progress"`
	Duration float64  `json:"duration,omitempty"`
	Filename string   `json:"filename,omitempty"`
	Error    string   `json:"error,omitempty"`
}

// QueueConfig bounds the work and memory of the queue. Zero fields take
// their defaults.
type QueueConfig struct {
	MaxPending     int           // queued jobs beyond which Submit fails; 20
	ResultTTL      time.Duration // finished jobs are dropped this long after; 10m
	AbandonAfter   time.Duration // unfinished jobs nobody asks about are canceled; 2m
	MaxResultBytes int           // results kept in memory, oldest dropped first; 256 MiB
	SweepEvery     time.Duration // how often the limits above are applied; 15s
}

func (c *QueueConfig) defaults() {
	if c.MaxPending <= 0 {
		c.MaxPending = 20
	}
	if c.ResultTTL <= 0 {
		c.ResultTTL = 10 * time.Minute
	}
	if c.AbandonAfter <= 0 {
		c.AbandonAfter = 2 * time.Minute
	}
	if c.MaxResultBytes <= 0 {
		c.MaxResultBytes = 256 << 20
	}
	if c.SweepEvery <= 0 {
		c.SweepEvery = 15 * time.Second
	}
}

var (
	ErrQueueFull = errors.New("queue is full")
	ErrNotReady  = errors.New("model is not ready")
	ErrNoJob     = errors.New("no such job")
)

type job struct {
	id       string
	req      Request
	priority bool // previews run before the other jobs
	state    JobState
	progress float64
	err      string
	wav      []byte
	duration float64
	filename string
	finished time.Time
	lastSeen time.Time
	ctx      context.Context
	cancel   context.CancelFunc
	done     chan struct{} // closed as the job finishes
}

// Queue runs speech jobs one at a time: each uses every thread the engine
// has. Results stay in memory, never on disk, until the client takes them
// or they expire.
type Queue struct {
	cfg   QueueConfig
	synth func() tts.Synthesizer
	now   func() time.Time

	mu          sync.Mutex
	jobs        map[string]*job
	pending     []*job
	running     *job
	resultBytes int
	wake        chan struct{}
}

// NewQueue returns a queue whose jobs use the synthesizer synth returns,
// nil while the model is not ready.
func NewQueue(cfg QueueConfig, synth func() tts.Synthesizer) *Queue {
	cfg.defaults()
	return &Queue{
		cfg:   cfg,
		synth: synth,
		now:   time.Now,
		jobs:  map[string]*job{},
		wake:  make(chan struct{}, 1),
	}
}

// Submit queues a job. A priority job, a preview, runs before the others.
func (q *Queue) Submit(req Request, priority bool) (string, error) {
	if q.synth() == nil {
		return "", ErrNotReady
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.pending) >= q.cfg.MaxPending {
		return "", ErrQueueFull
	}
	ctx, cancel := context.WithCancel(context.Background())
	j := &job{
		id:       newID(),
		req:      req,
		priority: priority,
		state:    JobQueued,
		lastSeen: q.now(),
		ctx:      ctx,
		cancel:   cancel,
		done:     make(chan struct{}),
	}
	q.jobs[j.id] = j
	q.pending = append(q.pending, j)
	select {
	case q.wake <- struct{}{}:
	default:
	}
	return j.id, nil
}

// Status returns a job's status, and notes that its client is still there.
func (q *Queue) Status(id string) (JobStatus, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	j := q.jobs[id]
	if j == nil {
		return JobStatus{}, false
	}
	j.lastSeen = q.now()
	return q.statusLocked(j), true
}

// Info reports the jobs waiting and whether one is running.
func (q *Queue) Info() (pending int, running bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.pending), q.running != nil
}

// Cancel stops a job: at once while queued, at its next sentence while
// running.
func (q *Queue) Cancel(id string) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	j := q.jobs[id]
	if j == nil {
		return false
	}
	q.cancelLocked(j)
	return true
}

// Result returns the audio of a finished job.
func (q *Queue) Result(id string) (wav []byte, filename string, ok bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	j := q.jobs[id]
	if j == nil || j.state != JobDone || j.wav == nil {
		return nil, "", false
	}
	return j.wav, j.filename, true
}

// Remove drops a finished job and its audio, or cancels an unfinished one.
func (q *Queue) Remove(id string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	j := q.jobs[id]
	if j == nil {
		return
	}
	if !j.state.finished() {
		q.cancelLocked(j)
		return
	}
	q.dropLocked(j)
}

// Wait waits for a job to finish, cancelling it if ctx ends first.
func (q *Queue) Wait(ctx context.Context, id string) (JobStatus, error) {
	q.mu.Lock()
	j := q.jobs[id]
	q.mu.Unlock()
	if j == nil {
		return JobStatus{}, ErrNoJob
	}
	select {
	case <-j.done:
	case <-ctx.Done():
		q.Cancel(id)
		return JobStatus{}, ctx.Err()
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.statusLocked(j), nil
}

// Run runs the jobs until ctx ends, then cancels those left.
func (q *Queue) Run(ctx context.Context) {
	stop := context.AfterFunc(ctx, q.cancelAll)
	defer stop()
	go func() {
		t := time.NewTicker(q.cfg.SweepEvery)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				q.sweep()
			case <-ctx.Done():
				return
			}
		}
	}()
	for {
		j := q.next(ctx)
		if j == nil {
			return
		}
		q.process(j)
	}
}

func (q *Queue) next(ctx context.Context) *job {
	for {
		q.mu.Lock()
		if len(q.pending) > 0 {
			i := slices.IndexFunc(q.pending, func(j *job) bool { return j.priority })
			if i < 0 {
				i = 0
			}
			j := q.pending[i]
			q.pending = slices.Delete(q.pending, i, i+1)
			j.state = JobRunning
			q.running = j
			q.mu.Unlock()
			return j
		}
		q.mu.Unlock()
		select {
		case <-q.wake:
		case <-ctx.Done():
			return nil
		}
	}
}

func (q *Queue) process(j *job) {
	var (
		audio *tts.Audio
		err   error
	)
	if s := q.synth(); s == nil {
		err = ErrNotReady
	} else {
		audio, err = s.Generate(j.ctx, tts.Normalize(j.req.Text), j.req.Voice.SID, j.req.Speed, func(p float64) {
			q.mu.Lock()
			j.progress = p
			q.mu.Unlock()
		})
	}
	var wav []byte
	if err == nil && j.ctx.Err() == nil {
		wav = tts.EncodeWAV(audio)
	}

	q.mu.Lock()
	defer q.mu.Unlock()
	q.running = nil
	switch {
	case j.ctx.Err() != nil:
		j.state = JobCanceled
	case err != nil:
		j.state, j.err = JobFailed, err.Error()
	default:
		j.state, j.progress, j.wav = JobDone, 1, wav
		j.duration = audio.Duration()
		j.filename = tts.FileName(j.req.Text, j.req.Voice.Name, q.now())
		q.resultBytes += len(wav)
	}
	q.finishLocked(j)
	if j.state == JobDone {
		q.evictLocked(j)
	}
}

// sweep cancels jobs whose clients went away and drops expired results.
func (q *Queue) sweep() {
	q.mu.Lock()
	defer q.mu.Unlock()
	now := q.now()
	for _, j := range q.jobs {
		switch {
		case j.state.finished():
			if now.Sub(j.finished) >= q.cfg.ResultTTL {
				q.dropLocked(j)
			}
		case !j.priority && now.Sub(j.lastSeen) >= q.cfg.AbandonAfter:
			// Previews are waited on by their request, which cancels them.
			q.cancelLocked(j)
		}
	}
}

// evictLocked drops the oldest results while they take more memory than
// allowed, keeping the newest.
func (q *Queue) evictLocked(keep *job) {
	for q.resultBytes > q.cfg.MaxResultBytes {
		var oldest *job
		for _, j := range q.jobs {
			if j != keep && j.wav != nil && (oldest == nil || j.finished.Before(oldest.finished)) {
				oldest = j
			}
		}
		if oldest == nil {
			return
		}
		q.dropLocked(oldest)
	}
}

func (q *Queue) cancelLocked(j *job) {
	switch j.state {
	case JobQueued:
		q.pending = slices.DeleteFunc(q.pending, func(p *job) bool { return p == j })
		j.state = JobCanceled
		q.finishLocked(j)
	case JobRunning:
		j.cancel() // process marks it canceled when the engine stops
	}
}

func (q *Queue) cancelAll() {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, j := range q.jobs {
		q.cancelLocked(j)
	}
}

func (q *Queue) finishLocked(j *job) {
	j.finished = q.now()
	j.cancel() // release the context
	close(j.done)
}

func (q *Queue) dropLocked(j *job) {
	q.resultBytes -= len(j.wav)
	j.wav = nil
	delete(q.jobs, j.id)
}

func (q *Queue) statusLocked(j *job) JobStatus {
	st := JobStatus{
		ID:       j.id,
		State:    j.state,
		Progress: j.progress,
		Duration: j.duration,
		Filename: j.filename,
		Error:    j.err,
	}
	if j.state == JobQueued {
		if q.running != nil {
			st.Ahead++
		}
		for _, p := range q.pending {
			if p == j {
				break
			}
			if p.priority || !j.priority {
				st.Ahead++
			}
		}
		if !j.priority {
			// Priority jobs queued after it still run first.
			i := slices.Index(q.pending, j)
			for _, p := range q.pending[i+1:] {
				if p.priority {
					st.Ahead++
				}
			}
		}
	}
	return st
}

func newID() string {
	b := make([]byte, 12)
	rand.Read(b)
	return hex.EncodeToString(b)
}
