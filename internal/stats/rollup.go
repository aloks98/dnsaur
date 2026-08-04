package stats

import (
	"context"
	"log/slog"
	"strconv"
	"time"

	"github.com/aloks98/dnsaur/internal/store"
)

type Runner struct {
	ss       store.StatsStore
	settings store.SettingsStore
	every    time.Duration
}

func NewRunner(ss store.StatsStore, settings store.SettingsStore, every time.Duration) *Runner {
	return &Runner{ss: ss, settings: settings, every: every}
}

func (r *Runner) once(ctx context.Context) {
	var after int64
	if v, ok, _ := r.settings.Get(ctx, "stats.watermark"); ok {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			slog.Warn("stats watermark corrupt, skipping rollup tick", "value", v, "err", err)
			return
		}
		after = n
	}
	last, err := r.ss.Rollup(ctx, after)
	if err != nil {
		slog.Warn("stats rollup failed", "err", err)
		return
	}
	if last != after {
		if err := r.settings.SetInternal(ctx, "stats.watermark", strconv.FormatInt(last, 10)); err != nil {
			slog.Warn("stats watermark write failed", "err", err)
		}
	}
}

func (r *Runner) Run(ctx context.Context) {
	t := time.NewTicker(r.every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.once(ctx)
		}
	}
}
