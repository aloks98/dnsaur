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

func (f *fakeStatsStore) PruneBefore(ctx context.Context, bucketBeforeSec int64) (int64, error) {
	return 0, nil
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
	getErr           error
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
	if f.getErr != nil {
		return "", false, f.getErr
	}
	v, ok := f.values[key]
	return v, ok, nil
}

func (f *fakeSettingsStore) Set(ctx context.Context, key, value string) error {
	f.values[key] = value
	return nil
}

func (f *fakeSettingsStore) SetMany(ctx context.Context, values map[string]string) error {
	for k, v := range values {
		f.values[k] = v
	}
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
	// No watermark stored → Rollup(0). The watermark itself is written by
	// the store, inside the rollup transaction — see
	// TestRunnerLeavesTheWatermarkToTheStore.
	ctx := context.Background()
	ss := &fakeStatsStore{lastID: 42}
	settings := newFakeSettingsStore()

	runner := NewRunner(ss, settings, 100*time.Millisecond)
	runner.once(ctx)

	if len(ss.calls) != 1 || ss.calls[0] != 0 {
		t.Fatalf("rollup not called with 0: %v", ss.calls)
	}
}

func TestRunnerNoOp(t *testing.T) {
	// A stored watermark is where the next rollup starts.
	ctx := context.Background()
	settings := newFakeSettingsStore()
	settings.values["stats.watermark"] = "42"
	ss := &fakeStatsStore{lastID: 42} // same as watermark

	runner := NewRunner(ss, settings, 100*time.Millisecond)
	runner.once(ctx)

	if len(ss.calls) != 1 || ss.calls[0] != 42 {
		t.Fatalf("rollup not called with 42: %v", ss.calls)
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

// TestRunnerLeavesTheWatermarkToTheStore: the counts and the record of how
// far they got are one fact and must be written once. When the runner wrote
// the watermark itself, a failed write after a committed rollup left the
// batch counted and the progress unrecorded, and the next tick added the
// same rows on top — permanently, since the hourly counters are additive.
// The store now writes it inside the rollup transaction, so there is no
// second write left to fail: a SetInternal that would error is never called.
func TestRunnerLeavesTheWatermarkToTheStore(t *testing.T) {
	ctx := context.Background()
	ss := &fakeStatsStore{lastID: 100}
	settings := newFakeSettingsStore()
	settings.setInternalErr = fmt.Errorf("disk full")

	runner := NewRunner(ss, settings, 100*time.Millisecond)
	runner.once(ctx)

	if len(ss.calls) != 1 || ss.calls[0] != 0 {
		t.Fatalf("rollup should be called: %v", ss.calls)
	}
	if len(settings.setInternalCalls) != 0 {
		t.Fatalf("runner wrote the watermark outside the rollup transaction: %v", settings.setInternalCalls)
	}
}

// TestRunnerWatermarkReadFailure: a watermark the store cannot read is not a
// watermark of 0. Rolling up from 0 re-adds every query_log row still in
// retention onto the hourly counters, which are additive, so one failed read
// doubles every dashboard number for as long as those rows are kept — and
// the same tick would then record the new watermark, so nothing ever
// corrects it. The tick has to be skipped, exactly as a corrupt watermark
// skips it.
func TestRunnerWatermarkReadFailure(t *testing.T) {
	ctx := context.Background()
	settings := newFakeSettingsStore()
	settings.values["stats.watermark"] = "42"
	settings.getErr = fmt.Errorf("database is locked")
	ss := &fakeStatsStore{lastID: 100}

	runner := NewRunner(ss, settings, 100*time.Millisecond)
	runner.once(ctx)

	if len(ss.calls) != 0 {
		t.Fatalf("rollup should not be called when the watermark cannot be read: %v", ss.calls)
	}
	if len(settings.setInternalCalls) != 0 {
		t.Fatalf("watermark written after a failed read: %v", settings.setInternalCalls)
	}
}

// TestRunnerRollupFailureKeepsWatermark: a rollup that failed counted
// nothing, so the watermark must stay where it was — advancing it would skip
// the rows the failed tick was supposed to count.
func TestRunnerRollupFailureKeepsWatermark(t *testing.T) {
	ctx := context.Background()
	settings := newFakeSettingsStore()
	settings.values["stats.watermark"] = "42"
	ss := &fakeStatsStore{lastID: 100, returnError: fmt.Errorf("disk full")}

	runner := NewRunner(ss, settings, 100*time.Millisecond)
	runner.once(ctx)

	if len(ss.calls) != 1 || ss.calls[0] != 42 {
		t.Fatalf("rollup not called from the stored watermark: %v", ss.calls)
	}
	if settings.values["stats.watermark"] != "42" {
		t.Fatalf("watermark moved to %q", settings.values["stats.watermark"])
	}
}
