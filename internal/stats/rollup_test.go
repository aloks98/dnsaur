package stats

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// fakeStatsStore records Rollup calls for testing.
type fakeStatsStore struct {
	lastID      int64
	returnError error
	calls       []int64 // afterID values passed to Rollup
}

func (f *fakeStatsStore) Rollup(ctx context.Context, afterID int64) (int64, error) {
	f.calls = append(f.calls, afterID)
	if f.returnError != nil {
		return 0, f.returnError
	}
	return f.lastID, nil
}

func (f *fakeStatsStore) Counter(ctx context.Context, bucketFromSec int64, metric string) (map[string]int64, error) {
	return nil, nil
}

func (f *fakeStatsStore) Timeline(ctx context.Context, fromSec int64) (map[int64]map[string]int64, error) {
	return nil, nil
}

// fakeSettingsStore holds in-memory settings and records SetInternal calls.
type fakeSettingsStore struct {
	values           map[string]string
	setInternalErr   error
	setInternalCalls []struct {
		key   string
		value string
	}
}

func newFakeSettingsStore() *fakeSettingsStore {
	return &fakeSettingsStore{values: make(map[string]string)}
}

func (f *fakeSettingsStore) Get(ctx context.Context, key string) (string, bool, error) {
	v, ok := f.values[key]
	return v, ok, nil
}

func (f *fakeSettingsStore) Set(ctx context.Context, key, value string) error {
	f.values[key] = value
	return nil
}

func (f *fakeSettingsStore) SetInternal(ctx context.Context, key, value string) error {
	f.setInternalCalls = append(f.setInternalCalls, struct {
		key   string
		value string
	}{key, value})
	return f.setInternalErr
}

func (f *fakeSettingsStore) GetInt(ctx context.Context, key string) (int64, error) {
	return 0, nil
}

func (f *fakeSettingsStore) ConfigVersion(ctx context.Context) (int64, error) {
	return 0, nil
}

func (f *fakeSettingsStore) Changes() <-chan int64 {
	return make(<-chan int64)
}

func (f *fakeSettingsStore) SeedDefaults(ctx context.Context, defaults map[string]string) error {
	return nil
}

func (f *fakeSettingsStore) All(ctx context.Context) (map[string]string, error) {
	return f.values, nil
}

func TestRunnerHappyPath(t *testing.T) {
	// Watermark 0 → Rollup(0) → watermark written with returned lastID
	ctx := context.Background()
	ss := &fakeStatsStore{lastID: 42}
	settings := newFakeSettingsStore()

	runner := NewRunner(ss, settings, 100*time.Millisecond)
	runner.once(ctx)

	if len(ss.calls) != 1 || ss.calls[0] != 0 {
		t.Fatalf("rollup not called with 0: %v", ss.calls)
	}
	if len(settings.setInternalCalls) != 1 {
		t.Fatalf("setInternal not called: %d calls", len(settings.setInternalCalls))
	}
	if settings.setInternalCalls[0].key != "stats.watermark" || settings.setInternalCalls[0].value != "42" {
		t.Fatalf("watermark not set correctly: %v", settings.setInternalCalls[0])
	}
}

func TestRunnerNoOp(t *testing.T) {
	// Rollup returns afterID → SetInternal NOT called
	ctx := context.Background()
	settings := newFakeSettingsStore()
	settings.values["stats.watermark"] = "42"
	ss := &fakeStatsStore{lastID: 42} // same as watermark

	runner := NewRunner(ss, settings, 100*time.Millisecond)
	runner.once(ctx)

	if len(ss.calls) != 1 || ss.calls[0] != 42 {
		t.Fatalf("rollup not called with 42: %v", ss.calls)
	}
	if len(settings.setInternalCalls) != 0 {
		t.Fatalf("setInternal should not be called when no new data: %d calls", len(settings.setInternalCalls))
	}
}

func TestRunnerCorruptWatermark(t *testing.T) {
	// Key exists with "garbage" → Rollup NOT called
	ctx := context.Background()
	settings := newFakeSettingsStore()
	settings.values["stats.watermark"] = "garbage"
	ss := &fakeStatsStore{}

	runner := NewRunner(ss, settings, 100*time.Millisecond)
	runner.once(ctx)

	if len(ss.calls) != 0 {
		t.Fatalf("rollup should not be called with corrupt watermark: %v", ss.calls)
	}
}

func TestRunnerSetInternalFailure(t *testing.T) {
	// Rollup called, but SetInternal fails → warning logged, no panic
	ctx := context.Background()
	ss := &fakeStatsStore{lastID: 100}
	settings := newFakeSettingsStore()
	settings.setInternalErr = fmt.Errorf("disk full")

	runner := NewRunner(ss, settings, 100*time.Millisecond)
	// Should not panic
	runner.once(ctx)

	if len(ss.calls) != 1 || ss.calls[0] != 0 {
		t.Fatalf("rollup should be called: %v", ss.calls)
	}
	if len(settings.setInternalCalls) != 1 {
		t.Fatalf("setInternal should be attempted: %d calls", len(settings.setInternalCalls))
	}
}
