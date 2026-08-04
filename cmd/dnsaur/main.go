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

var Version = "dev"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := run(ctx); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	cfgPath := flag.String("config", "dnsaur.yaml", "path to bootstrap config")
	flag.Parse()
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	var lvl slog.Level
	_ = lvl.UnmarshalText([]byte(cfg.LogLevel))
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: lvl})))
	slog.Info("dnsaur starting", "version", Version)

	a, err := app.New(ctx, cfg)
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
