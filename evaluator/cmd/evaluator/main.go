// Command evaluator is the deterministic shader evaluation service (:8081).
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/lioilsources/shaderbattle/evaluator/internal/api"
	"github.com/lioilsources/shaderbattle/evaluator/internal/glslang"
)

func main() {
	cfg := loadConfig()
	log := newLogger(cfg.LogFormat)
	slog.SetDefault(log)

	compiler := &glslang.Compiler{Bin: cfg.GlslangBin}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	ver, err := compiler.Version(ctx)
	if err != nil {
		log.Error("glslang unavailable", "bin", cfg.GlslangBin, "err", err)
		os.Exit(1)
	}

	srv := api.New(compiler, log, cfg.MaxCompiles)
	httpSrv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutdownCtx)
	}()

	log.Info("evaluator starting", "addr", cfg.Addr, "version", api.EvaluatorVersion, "glslang", ver)
	if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("server", "err", err)
		os.Exit(1)
	}
}

type config struct {
	Addr        string
	GlslangBin  string
	LogFormat   string
	MaxCompiles int
}

func loadConfig() config {
	maxCompiles, _ := strconv.Atoi(os.Getenv("EVALUATOR_MAX_COMPILES"))
	return config{
		Addr:        envOrDefault("EVALUATOR_ADDR", ":8081"),
		GlslangBin:  envOrDefault("GLSLANG_VALIDATOR", "glslangValidator"),
		LogFormat:   envOrDefault("EVALUATOR_LOG_FORMAT", "json"),
		MaxCompiles: maxCompiles,
	}
}

func newLogger(format string) *slog.Logger {
	if format == "text" {
		return slog.New(slog.NewTextHandler(os.Stdout, nil))
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, nil))
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
