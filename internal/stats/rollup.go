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

// once rolls up whatever query_log holds beyond the stored watermark. The
// watermark is only read here: the store records the new one inside the
// rollup transaction (see store.StatsStore.Rollup), so counting a batch and
// remembering it cannot come apart.
//
// A watermark that cannot be read is not a watermark of zero. Rolling up
// from zero re-adds every row still within query-log retention onto counters
// that are additive, doubling every figure the dashboard shows, and the tick
// that did it records a fresh watermark, so it never corrects itself. A read
// failure therefore skips the tick, exactly as an unparseable value does —
// stats lag by a minute, which is the cheap half of that trade.
func (r *Runner) once(ctx context.Context) {
	var after int64
	v, ok, err := r.settings.Get(ctx, store.StatsWatermarkKey)
	if err != nil {
		slog.Warn("stats watermark unreadable, skipping rollup tick", "err", err)
		return
	}
	if ok {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			slog.Warn("stats watermark corrupt, skipping rollup tick", "value", v, "err", err)
			return
		}
		after = n
	}
	if _, err := r.ss.Rollup(ctx, after); err != nil {
		slog.Warn("stats rollup failed", "err", err)
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
