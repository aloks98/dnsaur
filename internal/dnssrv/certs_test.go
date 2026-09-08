package dnssrv

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/aloks98/dnsaur/internal/certtest"
)

// write puts a keypair on disk and returns the two paths.
func write(t *testing.T, dir, name string, cert tls.Certificate) (string, string) {
	t.Helper()
	certPath := filepath.Join(dir, name+".crt")
	keyPath := filepath.Join(dir, name+".key")
	if err := os.WriteFile(certPath, certtest.EncodeCert(cert), 0o600); err != nil {
		t.Fatalf("writing the certificate: %v", err)
	}
	if err := os.WriteFile(keyPath, certtest.EncodeKey(t, cert), 0o600); err != nil {
		t.Fatalf("writing the key: %v", err)
	}
	return certPath, keyPath
}

func TestCertProviderServesTheKeypair(t *testing.T) {
	dir := t.TempDir()
	cert, _ := certtest.For(t, "first.test")
	certPath, keyPath := write(t, dir, "c", cert)

	p := NewCertProvider(certPath, keyPath)
	got, err := p.GetCertificate(&tls.ClientHelloInfo{})
	if err != nil {
		t.Fatalf("GetCertificate: %v", err)
	}
	if got.Leaf.Subject.CommonName != "first.test" {
		t.Errorf("served a certificate for %q, want first.test", got.Leaf.Subject.CommonName)
	}
}

// A renewal replaces the files; the next handshake must serve the new
// certificate. Without this, renewal silently stops working and DNS fails
// three months later for a reason that looks nothing like a certificate.
func TestCertProviderPicksUpARenewal(t *testing.T) {
	dir := t.TempDir()
	old, _ := certtest.For(t, "old.test")
	certPath, keyPath := write(t, dir, "c", old)

	p := NewCertProvider(certPath, keyPath)
	if _, err := p.GetCertificate(&tls.ClientHelloInfo{}); err != nil {
		t.Fatalf("first load: %v", err)
	}

	// Replace with a certificate for a different name, and make sure the
	// mtime actually moves — a same-second write on a coarse filesystem
	// would make this test pass for the wrong reason.
	renewed, _ := certtest.For(t, "renewed.test")
	future := time.Now().Add(2 * time.Second)
	if err := os.WriteFile(certPath, certtest.EncodeCert(renewed), 0o600); err != nil {
		t.Fatalf("rewriting the certificate: %v", err)
	}
	if err := os.WriteFile(keyPath, certtest.EncodeKey(t, renewed), 0o600); err != nil {
		t.Fatalf("rewriting the key: %v", err)
	}
	if err := os.Chtimes(certPath, future, future); err != nil {
		t.Fatalf("advancing mtime: %v", err)
	}

	got, err := p.GetCertificate(&tls.ClientHelloInfo{})
	if err != nil {
		t.Fatalf("second load: %v", err)
	}
	if got.Leaf.Subject.CommonName != "renewed.test" {
		t.Errorf("still serving %q after the files changed — the keypair was read once and cached forever", got.Leaf.Subject.CommonName)
	}
}

// An unreadable key is an error, not a panic and not a silently absent
// certificate. This is the permissions case from spec §4: certbot writes
// privkey.pem as 0600 root:root and dnsaur may not be root.
func TestCertProviderReportsAnUnreadableKey(t *testing.T) {
	dir := t.TempDir()
	cert, _ := certtest.For(t, "x.test")
	certPath, keyPath := write(t, dir, "c", cert)
	if err := os.Chmod(keyPath, 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(keyPath, 0o600) })
	if os.Geteuid() == 0 {
		t.Skip("running as root, which can read a 0000 file")
	}

	p := NewCertProvider(certPath, keyPath)
	if _, err := p.GetCertificate(&tls.ClientHelloInfo{}); err == nil {
		t.Fatal("GetCertificate succeeded against an unreadable key")
	}
}

func TestCertProviderLeafIsNilBeforeAnyLoad(t *testing.T) {
	p := NewCertProvider("/nonexistent/c.crt", "/nonexistent/c.key")
	if p.Leaf() != nil {
		t.Error("Leaf returned a certificate before anything was loaded")
	}
}

// Concurrent handshakes across a live renewal see one certificate or the
// other, never a torn mixture, and the race detector sees no race.
//
// GetCertificate runs on every single handshake, so its locking is on the
// hottest path this package has — and the rule it follows (take p.mu to
// swap fields, never across the Stat/LoadX509KeyPair I/O) is the exact rule
// E1's dot.go broke, as that milestone's one Important finding. Every other
// test here is sequential and single-goroutine, so -race has no concurrent
// access to observe in them at all: the suite's green -race runs said
// nothing whatsoever about this until now.
//
// The assertion is deliberately two-sided. The detector catches an
// unsynchronised field write; the name check catches a synchronised but
// *torn* one — a keypair stored with the previous load's mtimes, or a Leaf
// belonging to a different certificate than the one returned — which is
// invisible to -race because every access would still be under the lock.
func TestCertProviderIsSafeUnderConcurrentHandshakes(t *testing.T) {
	const goroutines = 20
	const reps = 5

	for rep := range reps {
		dir := t.TempDir()
		before, _ := certtest.For(t, "before.test")
		certPath, keyPath := write(t, dir, "c", before)
		p := NewCertProvider(certPath, keyPath)

		var wg sync.WaitGroup
		errs := make(chan error, goroutines)
		start := make(chan struct{})
		for range goroutines {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				for range 50 {
					got, err := p.GetCertificate(&tls.ClientHelloInfo{})
					if err != nil {
						// A mid-write read is allowed to fail — the
						// replacement below is two non-atomic WriteFiles —
						// so this is not an assertion, only a bound on what
						// the failure may be.
						continue
					}
					name := got.Leaf.Subject.CommonName
					if name != "before.test" && name != "after.test" {
						errs <- fmt.Errorf("served a certificate for %q, which is neither the old nor the new one", name)
						return
					}
					// The Leaf must describe the keypair actually being
					// served: a swap that stored one and cached the other
					// would show up here and nowhere else.
					leaf, err := x509.ParseCertificate(got.Certificate[0])
					if err != nil {
						errs <- fmt.Errorf("parsing the served DER: %w", err)
						return
					}
					if leaf.Subject.CommonName != name {
						errs <- fmt.Errorf("Leaf says %q but the served DER is %q — the cached tuple is torn", name, leaf.Subject.CommonName)
						return
					}
				}
			}()
		}

		close(start)
		// The renewal lands while the readers above are already running.
		after, _ := certtest.For(t, "after.test")
		future := time.Now().Add(2 * time.Second)
		if err := os.WriteFile(certPath, certtest.EncodeCert(after), 0o600); err != nil {
			t.Fatalf("rep %d: rewriting the certificate: %v", rep, err)
		}
		if err := os.WriteFile(keyPath, certtest.EncodeKey(t, after), 0o600); err != nil {
			t.Fatalf("rep %d: rewriting the key: %v", rep, err)
		}
		if err := os.Chtimes(certPath, future, future); err != nil {
			t.Fatalf("rep %d: advancing mtime: %v", rep, err)
		}

		wg.Wait()
		close(errs)
		for err := range errs {
			t.Errorf("rep %d: %v", rep, err)
		}
	}
}

// A reload that fails after a successful one keeps serving the certificate
// that did load.
//
// True by construction — `cached` only ever reads p.cert, and no error path
// nils or overwrites it — but nothing pinned it, and the guarantee is the
// whole reason certbot's non-atomic replacement is survivable: a handshake
// landing between the two WriteFiles must not take the listener down. The
// existing unreadable-key test fails from a cold start, so it never reaches
// this path at all.
func TestCertProviderKeepsTheCachedKeypairWhenAReloadFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root, which can read a 0000 file")
	}
	dir := t.TempDir()
	good, _ := certtest.For(t, "good.test")
	certPath, keyPath := write(t, dir, "c", good)

	p := NewCertProvider(certPath, keyPath)
	if _, err := p.GetCertificate(&tls.ClientHelloInfo{}); err != nil {
		t.Fatalf("first load: %v", err)
	}

	// Corrupt the certificate half and move its mtime, so the next call is
	// forced to attempt a reload and that reload cannot succeed.
	future := time.Now().Add(2 * time.Second)
	if err := os.WriteFile(certPath, []byte("not a certificate"), 0o600); err != nil {
		t.Fatalf("corrupting the certificate: %v", err)
	}
	if err := os.Chtimes(certPath, future, future); err != nil {
		t.Fatalf("advancing mtime: %v", err)
	}

	got, err := p.GetCertificate(&tls.ClientHelloInfo{})
	if err != nil {
		t.Fatalf("a failed reload was reported as an error even though a good keypair was cached: %v", err)
	}
	if got.Leaf.Subject.CommonName != "good.test" {
		t.Errorf("served %q after a failed reload, want the cached good.test", got.Leaf.Subject.CommonName)
	}
	if p.Leaf() == nil || p.Leaf().Subject.CommonName != "good.test" {
		t.Error("Leaf lost the cached certificate on a failed reload")
	}
}

// A renewal reaches a *live* TLS listener, over a real handshake.
//
// Every other test here calls GetCertificate directly, so nothing pinned
// that the seam actually in use — tls.Config.GetCertificate, consulted by
// crypto/tls during a ClientHello — reaches the provider at all. A
// tls.Config built with Certificates instead of GetCertificate, or a
// listener holding a config built once from a snapshot, would serve the
// boot-time keypair forever and pass every direct-call test in this file.
//
// InsecureSkipVerify because the point is *which* certificate the server
// presents, not whether the client trusts it; the assertion reads the
// subject off the presented chain, which a wrong certificate cannot satisfy.
func TestCertProviderRenewalReachesALiveTLSListener(t *testing.T) {
	dir := t.TempDir()
	old, _ := certtest.For(t, "old.test")
	certPath, keyPath := write(t, dir, "c", old)
	provider := NewCertProvider(certPath, keyPath)

	srv := NewServer("127.0.0.1:0", HandlerFunc(func(context.Context, *Request) (*Response, error) {
		return nil, nil
	}), WithTLS(&tls.Config{GetCertificate: provider.GetCertificate, MinVersion: tls.VersionTLS12}))
	if err := srv.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		if err := srv.Shutdown(context.Background()); err != nil {
			t.Logf("shutdown error: %v", err)
		}
	})

	presented := func() string {
		t.Helper()
		conn, err := tls.Dial("tcp", srv.Addr(), &tls.Config{
			InsecureSkipVerify: true, //nolint:gosec // the test asserts which certificate is presented, not that it is trusted
			MinVersion:         tls.VersionTLS12,
		})
		if err != nil {
			t.Fatalf("handshake: %v", err)
		}
		defer func() { _ = conn.Close() }()
		chain := conn.ConnectionState().PeerCertificates
		if len(chain) == 0 {
			t.Fatal("the server presented no certificate")
		}
		return chain[0].Subject.CommonName
	}

	if got := presented(); got != "old.test" {
		t.Fatalf("first handshake presented %q, want old.test", got)
	}

	renewed, _ := certtest.For(t, "renewed.test")
	future := time.Now().Add(2 * time.Second)
	if err := os.WriteFile(certPath, certtest.EncodeCert(renewed), 0o600); err != nil {
		t.Fatalf("rewriting the certificate: %v", err)
	}
	if err := os.WriteFile(keyPath, certtest.EncodeKey(t, renewed), 0o600); err != nil {
		t.Fatalf("rewriting the key: %v", err)
	}
	if err := os.Chtimes(certPath, future, future); err != nil {
		t.Fatalf("advancing mtime: %v", err)
	}

	if got := presented(); got != "renewed.test" {
		t.Errorf("handshake after the renewal still presented %q — the live tls.Config is not reaching the provider", got)
	}
}

// The key's mtime is watched too, not only the certificate's.
//
// TestCertProviderPicksUpARenewal rewrites both files and advances the
// certificate's mtime, so it passes just as well against a provider that
// only ever stats the certificate. This one holds the certificate's mtime
// still and moves the key's alone, which is the only arrangement that can
// tell those two implementations apart.
func TestCertProviderNoticesAKeyMtimeChangeAlone(t *testing.T) {
	dir := t.TempDir()
	old, _ := certtest.For(t, "old.test")
	certPath, keyPath := write(t, dir, "c", old)

	p := NewCertProvider(certPath, keyPath)
	if _, err := p.GetCertificate(&tls.ClientHelloInfo{}); err != nil {
		t.Fatalf("first load: %v", err)
	}
	certInfo, err := os.Stat(certPath)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	certModAt := certInfo.ModTime()

	renewed, _ := certtest.For(t, "renewed.test")
	if err := os.WriteFile(certPath, certtest.EncodeCert(renewed), 0o600); err != nil {
		t.Fatalf("rewriting the certificate: %v", err)
	}
	if err := os.WriteFile(keyPath, certtest.EncodeKey(t, renewed), 0o600); err != nil {
		t.Fatalf("rewriting the key: %v", err)
	}
	// Put the certificate's mtime back where it was: only the key's has
	// moved, so only a key stat can notice the renewal.
	if err := os.Chtimes(certPath, certModAt, certModAt); err != nil {
		t.Fatalf("restoring the certificate mtime: %v", err)
	}
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(keyPath, future, future); err != nil {
		t.Fatalf("advancing the key mtime: %v", err)
	}

	got, err := p.GetCertificate(&tls.ClientHelloInfo{})
	if err != nil {
		t.Fatalf("second load: %v", err)
	}
	if got.Leaf.Subject.CommonName != "renewed.test" {
		t.Errorf("still serving %q — only the key's mtime moved, and it was not being watched", got.Leaf.Subject.CommonName)
	}
}
