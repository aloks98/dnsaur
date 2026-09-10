package zones

// A direct, white-box test of watchStalledWrite, for the same reason
// batch_internal_test.go is one (see its header): an unexported helper with
// no seam a black-box test could drive. The property here is also not
// observable from outside at all — "no Close is still in flight once the
// stand-down has returned" is about the moment serve is free to return, and
// nothing in the package's API exposes that moment.

import (
	"context"
	"testing"
	"time"

	"github.com/miekg/dns"
)

// blockingCloser holds the watchdog inside Close until the test lets it out,
// which is what makes "still in flight" something a test can point at rather
// than race for. The embedded interface is nil: Close is the only method
// under test, and a call to any other would be the failure, loudly.
type blockingCloser struct {
	dns.ResponseWriter
	entered chan struct{}
	release chan struct{}
}

func (b *blockingCloser) Close() error {
	close(b.entered)
	<-b.release
	return nil
}

// The stand-down must not return while the watchdog can still close the
// connection.
//
// It used to only signal, over a channel a goroutine parked in a select was
// reading. When the context ended at the same moment the stream did, that
// select had both cases ready and picked one at random — so half the time it
// closed the connection after serve had already returned, and miekg reads the
// response writer after the handler returns (serveTCPConn). CI caught it as a
// write to dns.(*response).Close against that read.
func TestStalledWriteStandDownJoinsTheClose(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	w := &blockingCloser{entered: make(chan struct{}), release: make(chan struct{})}
	standDown := watchStalledWrite(ctx, w)

	// Put the watchdog inside Close: the state the stand-down has to wait out.
	cancel()
	<-w.entered

	returned := make(chan struct{})
	go func() {
		standDown()
		close(returned)
	}()
	// A deliberate window, not a poll: the assertion is that nothing happens
	// in it. Anything shorter risks passing on scheduling luck.
	select {
	case <-returned:
		t.Fatal("the stand-down returned with a Close still running: serve is free to return, and miekg reads the writer the moment it does")
	case <-time.After(100 * time.Millisecond):
	}

	close(w.release)
	select {
	case <-returned:
	case <-time.After(2 * time.Second):
		t.Fatal("the stand-down never returned once the Close had finished")
	}
}

// The ordinary path, and the one that must not deadlock now that standing
// down can wait: the stream finished first, so there is nothing to join and
// the connection is left open for the server to go on using.
func TestStalledWriteStandDownLeavesALiveConnectionOpen(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := &recordingCloser{}

	done := make(chan struct{})
	go func() {
		watchStalledWrite(ctx, w)()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("standing down a watch that never fired blocked")
	}

	if w.closes != 0 {
		t.Fatalf("the watchdog closed a live connection %d time(s); the stream had ended, so the connection is the server's to keep", w.closes)
	}
}

// recordingCloser counts closes. No lock: the only writer is the watchdog,
// and the read happens after standDown has joined it.
type recordingCloser struct {
	dns.ResponseWriter
	closes int
}

func (r *recordingCloser) Close() error {
	r.closes++
	return nil
}
