package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/aloks98/dnsaur/internal/app"
	"github.com/aloks98/dnsaur/internal/config"
)

// Version is the build's declared version, reported by GET /health and
// shown on the login screen. The default is the repo's current version
// rather than "dev": an unstamped local build is still a real build, and
// "dev" told an operator nothing about which code they were running.
// Release builds overwrite it via -ldflags (see .forgejo/workflows/ci.yml,
// which stamps the short commit SHA).
var Version = "0.1.0"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := run(ctx); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	cfgPath := flag.String("config", config.DefaultPath, "path to bootstrap config")
	flag.Parse()
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	// config.Load parses the level with this same call and refuses anything
	// it cannot read, so this cannot fail — but the error is returned rather
	// than discarded, because discarding it here is what made a misspelled
	// log_level silently INFO.
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(cfg.LogLevel)); err != nil {
		return err
	}
	// config.Load refuses any other format, so json is the only value this
	// can fall through to — and it is also the one every install that
	// predates log_format was already emitting.
	opts := &slog.HandlerOptions{Level: lvl}
	var h slog.Handler = slog.NewJSONHandler(os.Stderr, opts)
	if cfg.LogFormat == "text" {
		h = slog.NewTextHandler(os.Stderr, opts)
	}
	slog.SetDefault(slog.New(h))
	slog.Info("dnsaur starting", "version", Version)
	// One line per bootstrap key, with where the value came from. An operator
	// debugging a setting that "isn't being applied" is usually looking at a
	// file the process never read, and the only way to see that from the log
	// was to notice a value that happened to differ from what they wrote.
	// Config.Effective is what decides what is safe to print.
	for _, e := range cfg.Effective() {
		slog.Info("config", "key", e.Key, "value", e.Value, "source", e.Source)
	}

	a, err := app.New(ctx, cfg, Version)
	if err != nil {
		return err
	}
	if err := a.Start(ctx); err != nil {
		return err
	}
	slog.Info("dns listening", "addr", a.DNSAddr())
	<-ctx.Done()
	slog.Info("shutting down")
	shCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return a.Shutdown(shCtx)
}
