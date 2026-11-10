// Command sherpa-tts-web serves a web page that turns text into speech,
// offline, with sherpa-onnx and the Kokoro multi-lingual model (Chinese and
// English). The model is downloaded on first start. Generated audio is kept
// in memory only, until the browser takes it.
//
// Settings come from flags or, for containers, environment variables:
//
//	TTS_ADDR       -addr       address to listen on (:8080)
//	TTS_DATA_DIR   -data-dir   where the model is downloaded to (data)
//	TTS_MODEL_DIR  -model-dir  use a model already on disk, download nothing
//	TTS_MODEL_URL  -model-url  a mirror of the model files, tried first
//	TTS_MAX_CHARS  -max-chars  longest text accepted (5000)
//	TTS_MAX_QUEUE  -max-queue  jobs that may wait (20)
//	TTS_THREADS    -threads    inference threads (CPU cores, at most 8)
//	TTS_PASSWORD               password for the page (environment only)
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/swlfigo/sherpa-tts-web/internal/model"
	"github.com/swlfigo/sherpa-tts-web/internal/server"
	"github.com/swlfigo/sherpa-tts-web/internal/tts"
	"github.com/swlfigo/sherpa-tts-web/web"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	var (
		addr        = flag.String("addr", env("TTS_ADDR", ":8080"), "address to listen on")
		dataDir     = flag.String("data-dir", env("TTS_DATA_DIR", "data"), "directory the model is downloaded to")
		modelDir    = flag.String("model-dir", env("TTS_MODEL_DIR", ""), "directory holding the model already (skips the download)")
		modelURL    = flag.String("model-url", env("TTS_MODEL_URL", ""), "base URL of a mirror of the model files, tried first")
		maxChars    = flag.Int("max-chars", envInt("TTS_MAX_CHARS", 5000), "longest text accepted, in characters")
		maxQueue    = flag.Int("max-queue", envInt("TTS_MAX_QUEUE", 20), "jobs that may wait before new ones are refused")
		threads     = flag.Int("threads", envInt("TTS_THREADS", 0), "inference threads (0: one per CPU core, at most 8)")
		showVersion = flag.Bool("version", false, "print the version and exit")
	)
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), "Usage: %s [flags]\n\nSet TTS_PASSWORD to require a password.\n\n", os.Args[0])
		flag.PrintDefaults()
	}
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	slog.SetDefault(log)

	m, err := model.Embedded()
	if err != nil {
		log.Error("manifest", "err", err)
		os.Exit(1)
	}
	model.UserAgent = "github.com/swlfigo/sherpa-tts-web/" + version
	sources := m.Sources
	if *modelURL != "" {
		u := strings.TrimSuffix(*modelURL, "/") + "/"
		sources = append([]model.Source{{Name: "custom", URL: u}}, sources...)
	}
	if *threads <= 0 {
		*threads = min(runtime.NumCPU(), 8)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	engine := server.NewEngine(server.EngineConfig{
		Manifest: m,
		ModelDir: *modelDir,
		DataDir:  *dataDir,
		Sources:  sources,
		Threads:  *threads,
		Load:     tts.Load,
		Logger:   log,
	})
	go engine.Run(ctx)

	queue := server.NewQueue(server.QueueConfig{MaxPending: *maxQueue}, engine.Synth)
	queueDone := make(chan struct{})
	go func() {
		defer close(queueDone)
		queue.Run(ctx)
	}()

	password := os.Getenv("TTS_PASSWORD")
	handler := server.New(server.Config{
		Password: password,
		MaxChars: *maxChars,
		Version:  version,
		Logger:   log,
	}, engine, queue, web.FS)

	srv := &http.Server{
		Addr:              *addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		srv.Shutdown(shutdown)
	}()

	if password == "" {
		log.Warn("TTS_PASSWORD is not set: anyone who can reach this server can use it")
	}
	log.Info("listening", "addr", *addr, "version", version)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("serve", "err", err)
		os.Exit(1)
	}
	<-queueDone
	engine.Close()
	log.Info("stopped")
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
		fmt.Fprintf(os.Stderr, "ignoring %s=%q: not a number\n", key, v)
	}
	return def
}
