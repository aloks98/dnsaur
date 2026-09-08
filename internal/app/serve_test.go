package app

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/aloks98/dnsaur/internal/api"
	"github.com/aloks98/dnsaur/internal/certtest"
	"github.com/miekg/dns"
)

// testCertName is the subject/DNS name every certificate minted in this
// file carries, and the ServerName every test client verifies against.
const testCertName = "dot.test"

// writeCert puts a keypair on disk in certbot's layout (a PEM certificate,
// a PEM key) and returns the two paths — what CertProvider, and so the
// reconciler, actually reads.
func writeCert(t *testing.T, dir, name string, cert tls.Certificate) (certPath, keyPath string) {
	t.Helper()
	certPath = filepath.Join(dir, name+".crt")
	keyPath = filepath.Join(dir, name+".key")
	if err := os.WriteFile(certPath, certtest.EncodeCert(cert), 0o600); err != nil {
		t.Fatalf("writing the certificate: %v", err)
	}
	if err := os.WriteFile(keyPath, certtest.EncodeKey(t, cert), 0o600); err != nil {
		t.Fatalf("writing the key: %v", err)
	}
	return certPath, keyPath
}

// freeTCPAddr finds a currently-unused loopback TCP address by binding to
// port 0 and immediately releasing it, so a test can name a real,
// deterministic address before anything is listening on it — needed for
// the address-change and occupied-port tests, where ":0" itself would
// defeat the point (the reconciler has to be told a real address to move
// to or fail to bind).
func freeTCPAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("finding a free port: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("closing the probe listener: %v", err)
	}
	return addr
}

// newServingTestApp builds an App with a real (mock) default upstream and a
// self-signed keypair on disk for testCertName, ready to be pointed at by
// serve.tls.cert/key.
func newServingTestApp(t *testing.T) (a *App, pool *x509.CertPool, certPath, keyPath string) {
	t.Helper()
	pub := mockDNS(t, answerA("5.6.7.8"))
	a = newTestApp(t, withUpstreams(pub))
	cert, pool := certtest.For(t, testCertName)
	certPath, keyPath = writeCert(t, t.TempDir(), "c", cert)
	return a, pool, certPath, keyPath
}

// setInternal is Settings().SetInternal with the test's Fatal wired in. It
// (deliberately, like main_test.go's withSetting/SetInternal calls) does
// not go through the Changes() notification the live settings watcher
// reacts to — every test here calls a.applySettings itself, synchronously,
// so an assertion runs against a reconcile pass that has actually
// returned rather than one merely believed to have happened by now.
func setInternal(t *testing.T, a *App, key, value string) {
	t.Helper()
	if err := a.Store().Settings().SetInternal(context.Background(), key, value); err != nil {
		t.Fatalf("SetInternal(%s, %s): %v", key, value, err)
	}
}

// enableDoT points serve.tls.cert/key at certPath/keyPath, sets
// serve.dot.listen to addr, flips serve.dot.enabled to true, and reconciles.
func enableDoT(t *testing.T, a *App, certPath, keyPath, addr string) {
	t.Helper()
	setInternal(t, a, "serve.tls.cert", certPath)
	setInternal(t, a, "serve.tls.key", keyPath)
	setInternal(t, a, "serve.dot.listen", addr)
	setInternal(t, a, "serve.dot.enabled", "true")
	a.applySettings(context.Background())
}

// enableDoH is enableDoT's DoH counterpart.
func enableDoH(t *testing.T, a *App, certPath, keyPath, addr string) {
	t.Helper()
	setInternal(t, a, "serve.tls.cert", certPath)
	setInternal(t, a, "serve.tls.key", keyPath)
	setInternal(t, a, "serve.doh.listen", addr)
	setInternal(t, a, "serve.doh.enabled", "true")
	a.applySettings(context.Background())
}

// dotClient builds a dns.Client speaking DNS-over-TLS against a server
// presenting a certificate for testCertName, trusted via pool.
func dotClient(pool *x509.CertPool) *dns.Client {
	return &dns.Client{
		Net:       "tcp-tls",
		Timeout:   5 * time.Second,
		TLSConfig: &tls.Config{ServerName: testCertName, RootCAs: pool, MinVersion: tls.VersionTLS12},
	}
}

// askOverDoT sends one query for name to addr over a fresh DoT connection
// and fails the test unless it gets a real answer back.
func askOverDoT(t *testing.T, pool *x509.CertPool, addr, name string) {
	t.Helper()
	m := new(dns.Msg)
	m.SetQuestion(dns.Fqdn(name), dns.TypeA)
	r, _, err := dotClient(pool).Exchange(m, addr)
	if err != nil {
		t.Fatalf("Exchange over DoT (%s): %v", name, err)
	}
	if len(r.Answer) == 0 {
		t.Fatalf("no answer for %s over DoT, rcode %s", name, dns.RcodeToString[r.Rcode])
	}
}

// Test 1 (load-bearing): enabling DoT actually starts a listener that
// answers a real query over a real TLS connection — not merely a
// servingState that claims it did.
func TestServingEnablingDoTStartsAListener(t *testing.T) {
	a, pool, certPath, keyPath := newServingTestApp(t)
	addr := freeTCPAddr(t)

	enableDoT(t, a, certPath, keyPath, addr)

	status := a.ServingStatus().DoT
	if !status.Enabled || !status.Listening {
		t.Fatalf("DoT state = %+v, want enabled and listening", status)
	}
	if status.Err != "" {
		t.Errorf("DoT.Err = %q, want empty", status.Err)
	}
	if status.Addr == "" {
		t.Fatal("DoT.Addr is empty while Listening is true")
	}

	askOverDoT(t, pool, status.Addr, "example.com")
}

// Test 2 (load-bearing): toggling DoH must not drop a live DoT connection.
// dns.Client keeps no connection of its own, so the persistent connection
// is dialled explicitly and reused via ExchangeWithConnContext — the same
// pattern internal/upstream/dot.go's pool uses. This is the test a
// reconciler that stops and restarts every listener on every reconcile
// fails, and nothing else in this file would catch it.
func TestServingTogglingDoHLeavesALiveDoTConnectionWorking(t *testing.T) {
	ctx := context.Background()
	a, pool, certPath, keyPath := newServingTestApp(t)
	dotAddr := freeTCPAddr(t)

	enableDoT(t, a, certPath, keyPath, dotAddr)
	before := a.ServingStatus().DoT
	if !before.Listening {
		t.Fatalf("precondition: DoT not listening: %+v", before)
	}

	client := dotClient(pool)
	conn, err := client.DialContext(ctx, before.Addr)
	if err != nil {
		t.Fatalf("DialContext: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	askOnConn := func(name string) {
		t.Helper()
		m := new(dns.Msg)
		m.SetQuestion(dns.Fqdn(name), dns.TypeA)
		r, _, err := client.ExchangeWithConnContext(ctx, m, conn)
		if err != nil {
			t.Fatalf("ExchangeWithConnContext(%s): %v", name, err)
		}
		if len(r.Answer) == 0 {
			t.Fatalf("no answer for %s, rcode %s", name, dns.RcodeToString[r.Rcode])
		}
	}
	// Proves the connection is genuinely usable before DoH ever enters the
	// picture, so a failure below is attributable to what happens next.
	askOnConn("before.example")

	dohAddr := freeTCPAddr(t)
	enableDoH(t, a, certPath, keyPath, dohAddr)

	dohStatus := a.ServingStatus().DoH
	if !dohStatus.Listening || dohStatus.Err != "" {
		t.Fatalf("DoH did not come up: %+v", dohStatus)
	}
	dotStatus := a.ServingStatus().DoT
	if !dotStatus.Listening || dotStatus.Addr != before.Addr {
		t.Fatalf("DoT state changed by enabling DoH: before %+v, after %+v", before, dotStatus)
	}

	// The load-bearing assertion: the connection opened before DoH existed
	// still answers on the far side of that settings change.
	askOnConn("after.example")
}

// Test 3: disabling actually stops the listener — the port becomes
// bindable again by something else, not merely "the setting now reads
// false".
func TestServingDisablingStopsTheListener(t *testing.T) {
	ctx := context.Background()
	a, _, certPath, keyPath := newServingTestApp(t)
	addr := freeTCPAddr(t)

	enableDoT(t, a, certPath, keyPath, addr)
	if !a.ServingStatus().DoT.Listening {
		t.Fatal("precondition: DoT never started")
	}

	setInternal(t, a, "serve.dot.enabled", "false")
	a.applySettings(ctx)

	status := a.ServingStatus().DoT
	if status.Enabled || status.Listening {
		t.Fatalf("DoT state after disabling = %+v, want disabled and not listening", status)
	}

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("port %s is still occupied after disabling DoT: %v", addr, err)
	}
	_ = ln.Close()
}

// Test 4: a bind failure — the ordinary "the port is already in use" kind —
// is recorded in servingState rather than merely logged. Ports 853 and 443
// are privileged, so this is not a corner case in production.
func TestServingBindFailureIsRecorded(t *testing.T) {
	a, _, certPath, keyPath := newServingTestApp(t)
	addr := freeTCPAddr(t)

	occupied, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("occupying %s: %v", addr, err)
	}
	t.Cleanup(func() { _ = occupied.Close() })

	enableDoT(t, a, certPath, keyPath, addr)

	status := a.ServingStatus().DoT
	if status.Listening {
		t.Fatal("DoT reports Listening against an address something else already occupies")
	}
	if status.Err == "" {
		t.Error("DoT.Err is empty; a bind failure must be recorded, not just logged")
	}
	if !status.Enabled {
		t.Error("DoT.Enabled is false; the setting still says true even though the bind failed — intent and reality must both be visible")
	}
	// Fix round 1: the address that failed to bind is the useful half of a
	// privileged-port error ("bind :853: address already in use") — Task
	// 9's status UI can only name *that* something failed without it.
	if status.Addr != addr {
		t.Errorf("DoT.Addr = %q after a failed bind, want %q — the address is what makes the error actionable", status.Addr, addr)
	}
}

// Test 5: an address change is a real move. The new address serves, and
// nothing is left listening on the old one — a stop-then-start, not a
// second listener alongside the first.
func TestServingAddressChangeMovesTheListener(t *testing.T) {
	ctx := context.Background()
	a, pool, certPath, keyPath := newServingTestApp(t)
	addr1 := freeTCPAddr(t)

	enableDoT(t, a, certPath, keyPath, addr1)
	if got := a.ServingStatus().DoT; !got.Listening || got.Addr != addr1 {
		t.Fatalf("precondition: DoT = %+v, want listening on %s", got, addr1)
	}

	addr2 := freeTCPAddr(t)
	setInternal(t, a, "serve.dot.listen", addr2)
	a.applySettings(ctx)

	status := a.ServingStatus().DoT
	if !status.Listening || status.Addr != addr2 {
		t.Fatalf("after the address change, DoT = %+v, want listening on %s", status, addr2)
	}

	ln, err := net.Listen("tcp", addr1)
	if err != nil {
		t.Fatalf("the old address %s is still occupied after the move: %v", addr1, err)
	}
	_ = ln.Close()

	askOverDoT(t, pool, addr2, "example.com")
}

// Test 6: a settings write with nothing to do with serving must disturb
// neither listener. Mechanically this exercises the same "leave alone"
// branch Test 2 does, reached through a different trigger — an unrelated
// key rather than the other protocol — so a reconciler keyed narrowly on
// "did a serve.* key change" rather than "does the desired state differ"
// could pass one and fail the other.
func TestServingUnrelatedSettingsWriteDisturbsNeitherListener(t *testing.T) {
	ctx := context.Background()
	a, pool, certPath, keyPath := newServingTestApp(t)

	dotAddr := freeTCPAddr(t)
	enableDoT(t, a, certPath, keyPath, dotAddr)
	dohAddr := freeTCPAddr(t)
	enableDoH(t, a, certPath, keyPath, dohAddr)

	before := a.ServingStatus()
	if !before.DoT.Listening || !before.DoH.Listening {
		t.Fatalf("precondition: DoT/DoH = %+v / %+v, want both listening", before.DoT, before.DoH)
	}

	client := dotClient(pool)
	conn, err := client.DialContext(ctx, before.DoT.Addr)
	if err != nil {
		t.Fatalf("DialContext: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	// A setting with nothing to do with serving at all.
	setInternal(t, a, "blocking.mode", "nxdomain")
	a.applySettings(ctx)

	after := a.ServingStatus()
	if after.DoT != before.DoT {
		t.Errorf("DoT state changed from an unrelated settings write: before %+v, after %+v", before.DoT, after.DoT)
	}
	if after.DoH != before.DoH {
		t.Errorf("DoH state changed from an unrelated settings write: before %+v, after %+v", before.DoH, after.DoH)
	}

	m := new(dns.Msg)
	m.SetQuestion(dns.Fqdn("still-alive.example"), dns.TypeA)
	if _, _, err := client.ExchangeWithConnContext(ctx, m, conn); err != nil {
		t.Fatalf("the DoT connection opened before the unrelated write no longer answers: %v", err)
	}
}

// --- Rule 3: "enabled but no usable certificate" is a live state ----------
//
// Fix round 1 added coverage for both directions the brief's rule 3
// describes: a certificate that never loads in the first place, and one
// that stops being loadable after a listener is already up and using it.
// Neither was in the original six-test list.

// Rule 3, direction 1: a certificate that cannot be loaded at all is an
// ordinary bind-time failure — reported through servingState, with the
// port left free — not a listener that silently comes up unable to serve
// a single handshake.
func TestServingUnreadableCertificateReportsAndLeavesPortFree(t *testing.T) {
	a, _, _, _ := newServingTestApp(t)
	addr := freeTCPAddr(t)
	dir := t.TempDir()

	// Absolute paths — satisfying the API's own grammar for serve.tls.* —
	// naming files that simply do not exist. Task 7's review established
	// enabling against a certificate that never loads is a reachable
	// stored state; this is the simplest way to reach it directly, without
	// going through a save that first had to succeed with a valid pair.
	setInternal(t, a, "serve.tls.cert", filepath.Join(dir, "missing.crt"))
	setInternal(t, a, "serve.tls.key", filepath.Join(dir, "missing.key"))
	setInternal(t, a, "serve.dot.listen", addr)
	setInternal(t, a, "serve.dot.enabled", "true")
	a.applySettings(context.Background())

	status := a.ServingStatus().DoT
	if status.Listening {
		t.Fatal("DoT reports Listening with no certificate files on disk at all")
	}
	if status.Err == "" {
		t.Error("DoT.Err is empty; a certificate that never loads must be reported like any other bind-time failure")
	}
	if !status.Enabled {
		t.Error("DoT.Enabled is false; the setting still says true even though the certificate could not be loaded")
	}

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("port %s is occupied even though the certificate never loaded and the socket should never have been bound: %v", addr, err)
	}
	_ = ln.Close()
}

// Rule 3, direction 2: a certificate deleted out from under an
// already-running listener must not take the listener down. Its
// CertProvider already has a successfully-loaded keypair cached (Task 4),
// and nothing here may re-probe the filesystem for a listener the diff
// says is unchanged.
func TestServingCertificateDeletedUnderARunningListenerLeavesItServing(t *testing.T) {
	a, pool, certPath, keyPath := newServingTestApp(t)
	addr := freeTCPAddr(t)

	enableDoT(t, a, certPath, keyPath, addr)
	if !a.ServingStatus().DoT.Listening {
		t.Fatal("precondition: DoT never started")
	}

	if err := os.Remove(certPath); err != nil {
		t.Fatalf("removing the certificate: %v", err)
	}
	if err := os.Remove(keyPath); err != nil {
		t.Fatalf("removing the key: %v", err)
	}

	// A reconcile triggered by something that has nothing to do with DoT —
	// the same shape as any unrelated settings write reaching
	// applySettings while the certificate happens to be gone.
	setInternal(t, a, "blocking.mode", "nxdomain")
	a.applySettings(context.Background())

	status := a.ServingStatus().DoT
	if !status.Listening || status.Err != "" {
		t.Fatalf("DoT = %+v after its certificate files were deleted; want it left running, untouched", status)
	}

	// The load-bearing assertion: the listener's own CertProvider is still
	// serving the keypair it loaded before the files vanished, proven end
	// to end with a real handshake rather than by inspecting state.
	askOverDoT(t, pool, status.Addr, "still-alive.example")
}

// --- Fix round 1: cert-path change detection and settings-read safety -----

// Repointing serve.tls.cert/key at a *different, valid* keypair while a
// listener is up is a save the API accepts (Task 7) and must actually take
// effect: the listener has to restart and serve the new certificate, not
// silently keep the old one running under a status that still claims
// success.
func TestServingCertificatePathChangeRestartsTheListener(t *testing.T) {
	a, pool1, certPath1, keyPath1 := newServingTestApp(t)
	addr := freeTCPAddr(t)

	enableDoT(t, a, certPath1, keyPath1, addr)
	before := a.ServingStatus().DoT
	if !before.Listening {
		t.Fatal("precondition: DoT never started")
	}
	askOverDoT(t, pool1, before.Addr, "example.com")

	const secondCertName = "dot2.test"
	cert2, pool2 := certtest.For(t, secondCertName)
	certPath2, keyPath2 := writeCert(t, t.TempDir(), "c2", cert2)

	setInternal(t, a, "serve.tls.cert", certPath2)
	setInternal(t, a, "serve.tls.key", keyPath2)
	a.applySettings(context.Background())

	after := a.ServingStatus().DoT
	if !after.Listening {
		t.Fatalf("DoT = %+v after a certificate path change, want still listening", after)
	}
	if after.Addr != before.Addr {
		t.Fatalf("DoT moved address (%s -> %s) from a certificate-only change; only the certificate changed", before.Addr, after.Addr)
	}

	// The load-bearing assertion: a client trusting the NEW certificate
	// completes a handshake and gets an answer — proof the listener
	// actually restarted with the new keypair rather than going on serving
	// the old one, unnoticed, from a diff that only ever compared addresses.
	client := &dns.Client{
		Net: "tcp-tls", Timeout: 5 * time.Second,
		TLSConfig: &tls.Config{ServerName: secondCertName, RootCAs: pool2, MinVersion: tls.VersionTLS12},
	}
	m := new(dns.Msg)
	m.SetQuestion(dns.Fqdn("example.com"), dns.TypeA)
	if _, _, err := client.Exchange(m, after.Addr); err != nil {
		t.Fatalf("Exchange against the new certificate: %v", err)
	}
}

// A settings-store read failure must not read as "the operator disabled
// encrypted DNS". Simulated with an already-canceled context: verified
// separately that Settings().Get returns a real error ("context canceled")
// against one, which is what actually distinguishes this from a merely
// absent key (("", false, nil), not an error) — the distinction
// reconcileServing has to preserve for this test to mean anything.
func TestServingSettingsReadFailureLeavesListenersUntouched(t *testing.T) {
	a, pool, certPath, keyPath := newServingTestApp(t)
	addr := freeTCPAddr(t)

	enableDoT(t, a, certPath, keyPath, addr)
	before := a.ServingStatus()
	if !before.DoT.Listening {
		t.Fatal("precondition: DoT never started")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	a.reconcileServing(ctx)

	after := a.ServingStatus()
	if after != before {
		t.Errorf("servingState changed from a failed settings read: before %+v, after %+v", before, after)
	}
	if !after.DoT.Listening {
		t.Fatal("DoT was torn down by a settings read failure")
	}

	// The listener itself must still be genuinely alive, not merely
	// unreported-on.
	askOverDoT(t, pool, before.DoT.Addr, "still-alive.example")
}

// DoT and DoH read the same serve.tls.cert/key settings, so they must be
// built from the exact same CertProvider instance rather than two
// independent ones each caching the on-disk keypair's mtime on their own —
// two such caches can disagree briefly across a renewal. This pins the
// sharing primitive itself: identical paths return the identical instance,
// and different paths do not.
func TestServingCertProviderForSharesOneInstancePerPathPair(t *testing.T) {
	a, _, certPath, keyPath := newServingTestApp(t)

	p1 := a.serving.certProviderFor(certPath, keyPath)
	p2 := a.serving.certProviderFor(certPath, keyPath)
	if p1 != p2 {
		t.Error("certProviderFor built a second instance for identical paths — DoT and DoH would each cache the on-disk keypair independently")
	}

	cert2, _ := certtest.For(t, "other.test")
	certPath2, keyPath2 := writeCert(t, t.TempDir(), "other", cert2)
	p3 := a.serving.certProviderFor(certPath2, keyPath2)
	if p3 == p1 {
		t.Error("certProviderFor returned the same instance for two different path pairs")
	}
}

// --- Task 9: status and certificate-expiry reporting -----------------------

// Serving satisfies api.ResolverStatus by converting the reconciler's own
// protocolState into api.ProtocolStatus. This proves the conversion is
// faithful for both a working listener and one that failed to bind — DoT
// and DoH fail independently, so the API has to be able to tell them apart
// rather than reporting one aggregated boolean.
func TestServingReportsThroughTheAPIInterface(t *testing.T) {
	a, _, certPath, keyPath := newServingTestApp(t)
	dotAddr := freeTCPAddr(t)
	enableDoT(t, a, certPath, keyPath, dotAddr)

	dohAddr := freeTCPAddr(t)
	occupied, err := net.Listen("tcp", dohAddr)
	if err != nil {
		t.Fatalf("occupying %s: %v", dohAddr, err)
	}
	t.Cleanup(func() { _ = occupied.Close() })
	enableDoH(t, a, certPath, keyPath, dohAddr)

	want := a.ServingStatus()
	gotDot, gotDoh := a.Serving()

	if gotDot != (api.ProtocolStatus{Enabled: want.DoT.Enabled, Listening: want.DoT.Listening, Addr: want.DoT.Addr, Err: want.DoT.Err}) {
		t.Errorf("Serving() dot = %+v, want it converted from %+v", gotDot, want.DoT)
	}
	if !gotDot.Listening {
		t.Fatal("precondition: DoT should be listening")
	}

	if gotDoh != (api.ProtocolStatus{Enabled: want.DoH.Enabled, Listening: want.DoH.Listening, Addr: want.DoH.Addr, Err: want.DoH.Err}) {
		t.Errorf("Serving() doh = %+v, want it converted from %+v", gotDoh, want.DoH)
	}
	if gotDoh.Listening {
		t.Fatal("precondition: DoH should have failed to bind against an address already in use")
	}
	if gotDoh.Err == "" {
		t.Error("Serving() doh.Err is empty despite a failed bind — the API loses the one actionable detail")
	}
}

// A fresh install, or one where neither encrypted protocol has ever been
// enabled, has no certificate loaded at all — CertExpiry must report that
// as ok=false rather than as a zero-value NotAfter that would look like a
// certificate that expired long ago.
func TestCertExpiryWithNothingLoadedReportsNotOk(t *testing.T) {
	a, _, _, _ := newServingTestApp(t)
	if _, _, ok := a.CertExpiry(); ok {
		t.Fatal("CertExpiry reports ok=true before any certificate has ever loaded")
	}
}

// newServingTestAppWithCertExpiry builds an App and enables DoT against a
// freshly minted certificate expiring dur from now — enough to force the
// shared CertProvider to actually load it (buildTLSConfig's eager load), so
// Leaf(), and so CertExpiry, has something to report.
func newServingTestAppWithCertExpiry(t *testing.T, dur time.Duration) *App {
	t.Helper()
	pub := mockDNS(t, answerA("5.6.7.8"))
	a := newTestApp(t, withUpstreams(pub))
	cert, _ := certtest.ForWithExpiry(t, "expiry.test", time.Now().Add(dur))
	certPath, keyPath := writeCert(t, t.TempDir(), "expiry", cert)
	enableDoT(t, a, certPath, keyPath, freeTCPAddr(t))
	if !a.ServingStatus().DoT.Listening {
		t.Fatal("precondition: DoT never started, so the certificate never loaded")
	}
	return a
}

// The 14-day threshold (certExpiryWarningWindow) has to be tested on both
// sides: a certificate that only checked the warning case could not tell a
// working threshold from one that always fires.
func TestCertExpiryWarnsInsideTheThresholdNotOutsideIt(t *testing.T) {
	t.Run("13 days out warns", func(t *testing.T) {
		a := newServingTestAppWithCertExpiry(t, 13*24*time.Hour)
		notAfter, expiringSoon, ok := a.CertExpiry()
		if !ok {
			t.Fatal("CertExpiry reports ok=false after a successful load")
		}
		if !expiringSoon {
			t.Errorf("13 days out: expiringSoon = false, want true (notAfter %v)", notAfter)
		}
	})

	t.Run("15 days out does not warn", func(t *testing.T) {
		a := newServingTestAppWithCertExpiry(t, 15*24*time.Hour)
		notAfter, expiringSoon, ok := a.CertExpiry()
		if !ok {
			t.Fatal("CertExpiry reports ok=false after a successful load")
		}
		if expiringSoon {
			t.Errorf("15 days out: expiringSoon = true, want false (notAfter %v) — a threshold that always fires is indistinguishable from a working one", notAfter)
		}
	})
}

// dohClient builds an http.Client speaking DNS-over-HTTPS against a server
// presenting a certificate for testCertName, trusted via pool. HTTP/2 is
// forced on: setting TLSClientConfig disables net/http's own automatic
// attempt, and the DoH listener negotiates h2 by default.
func dohClient(pool *x509.CertPool) *http.Client {
	return &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			ForceAttemptHTTP2: true,
			TLSClientConfig: &tls.Config{
				ServerName: testCertName,
				RootCAs:    pool,
				MinVersion: tls.VersionTLS12,
			},
		},
	}
}

// askOverDoH sends one RFC 8484 POST for name to addr and fails the test
// unless a real answer comes back.
func askOverDoH(t *testing.T, pool *x509.CertPool, addr, name string) {
	t.Helper()
	m := new(dns.Msg)
	m.SetQuestion(dns.Fqdn(name), dns.TypeA)
	wire, err := m.Pack()
	if err != nil {
		t.Fatalf("packing the query: %v", err)
	}
	resp, err := dohClient(pool).Post(
		"https://"+addr+"/dns-query", "application/dns-message", bytes.NewReader(wire))
	if err != nil {
		t.Fatalf("POST over DoH (%s): %v", name, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading the DoH response body: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("DoH POST for %s returned %d: %s", name, resp.StatusCode, body)
	}
	r := new(dns.Msg)
	if err := r.Unpack(body); err != nil {
		t.Fatalf("unpacking the DoH response: %v", err)
	}
	if len(r.Answer) == 0 {
		t.Fatalf("no answer for %s over DoH, rcode %s", name, dns.RcodeToString[r.Rcode])
	}
}

// Enabling DoH actually starts an endpoint that answers a real query
// through the real pipeline — the DoH half of Test 1, which was declared
// load-bearing on the argument that a servingState claiming success is not
// proof of anything.
//
// That argument was never applied to DoH: enableDoH appears three times
// above and nothing ever dialled the address it produced, so every DoH
// assertion in this file stopped at "the socket bound". A DoH listener
// wired to a nil handler, or to a mux with the wrong path, or holding a
// tls.Config whose certificate never loads for a real handshake, would have
// satisfied all of them.
func TestServingEnablingDoHStartsAnEndpointThatAnswers(t *testing.T) {
	a, pool, certPath, keyPath := newServingTestApp(t)
	addr := freeTCPAddr(t)

	enableDoH(t, a, certPath, keyPath, addr)

	status := a.ServingStatus().DoH
	if !status.Enabled || !status.Listening {
		t.Fatalf("DoH state = %+v, want enabled and listening", status)
	}
	if status.Err != "" {
		t.Errorf("DoH.Err = %q, want empty", status.Err)
	}

	askOverDoH(t, pool, status.Addr, "example.com")
}

// A bind failure whose cause the operator has since cleared is retried, and
// the state corrects itself with no settings write at all.
//
// Spec §8 promises the warning clears "on its own … when a later reconcile
// binds successfully". Until this existed nothing *caused* a later
// reconcile: reconcileServing ran only from applySettings, so an operator
// who stopped whatever was holding the port watched the banner stay up
// until some unrelated setting happened to be saved.
func TestServingRetriesAFailedBindOnItsOwn(t *testing.T) {
	// Shortened for the test only; restored before anything else runs.
	prev := servingRetryEvery
	servingRetryEvery = 20 * time.Millisecond
	t.Cleanup(func() { servingRetryEvery = prev })

	a, pool, certPath, keyPath := newServingTestApp(t)
	addr := freeTCPAddr(t)

	// Somebody else holds the address when DoT is enabled.
	squatter, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("occupying %s: %v", addr, err)
	}
	enableDoT(t, a, certPath, keyPath, addr)
	failed := a.ServingStatus().DoT
	if !failed.Enabled || failed.Listening {
		_ = squatter.Close()
		t.Fatalf("precondition: DoT state = %+v, want enabled and not listening", failed)
	}

	// The operator stops whatever it was. No settings write follows.
	if err := squatter.Close(); err != nil {
		t.Fatalf("releasing %s: %v", addr, err)
	}

	deadline := time.Now().Add(5 * time.Second)
	var got protocolState
	for time.Now().Before(deadline) {
		got = a.ServingStatus().DoT
		if got.Listening {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !got.Listening {
		t.Fatalf("DoT never came up after the port was freed: %+v — nothing re-runs a failed reconcile", got)
	}
	// The state saying "listening" is not the claim; answering is.
	askOverDoT(t, pool, got.Addr, "example.com")
}

// The retry only fires for a protocol that is enabled and not listening.
// Without this, the guard could be dropped and the ticker would put six
// settings reads on a clock forever, for the overwhelmingly common case
// where nothing is wrong.
func TestNeedsRetryOnlyForEnabledButNotListening(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   protocolState
		want bool
	}{
		{"off", protocolState{}, false},
		{"listening", protocolState{Enabled: true, Listening: true}, false},
		{"enabled but not listening", protocolState{Enabled: true}, true},
	} {
		if got := needsRetry(tc.in); got != tc.want {
			t.Errorf("needsRetry(%s) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// Turning both protocols off stops the certificate being reported at all.
//
// The shared CertProvider used to be built once and never cleared, so
// CertExpiry went on answering ok=true from the last keypair that loaded —
// and the "expires in N days" banner went on nagging about a certificate
// nothing serves, until the process restarted.
func TestCertExpiryStopsReportingOnceBothProtocolsAreOff(t *testing.T) {
	a, _, certPath, keyPath := newServingTestApp(t)
	enableDoT(t, a, certPath, keyPath, freeTCPAddr(t))
	if _, _, ok := a.CertExpiry(); !ok {
		t.Fatal("precondition: no certificate loaded while DoT is up")
	}

	setInternal(t, a, "serve.dot.enabled", "false")
	a.applySettings(context.Background())

	if notAfter, expiringSoon, ok := a.CertExpiry(); ok {
		t.Errorf("CertExpiry still reports a certificate (notAfter=%v expiringSoon=%v) after both protocols were turned off", notAfter, expiringSoon)
	}
}

// Blanking the certificate paths, with nothing enabled, has the same
// effect: there is no certificate to describe any more, so nothing is
// described.
func TestCertExpiryStopsReportingOnceThePathsAreBlanked(t *testing.T) {
	a, _, certPath, keyPath := newServingTestApp(t)
	enableDoT(t, a, certPath, keyPath, freeTCPAddr(t))
	if _, _, ok := a.CertExpiry(); !ok {
		t.Fatal("precondition: no certificate loaded while DoT is up")
	}

	setInternal(t, a, "serve.dot.enabled", "false")
	setInternal(t, a, "serve.tls.cert", "")
	setInternal(t, a, "serve.tls.key", "")
	a.applySettings(context.Background())

	if _, _, ok := a.CertExpiry(); ok {
		t.Error("CertExpiry still reports a certificate after the paths were cleared")
	}
}

// A renewal on disk moves the reported expiry without anyone having to
// connect first.
//
// CertProvider.Leaf only advances when a handshake forces a reload, so
// reading it alone left a quiet resolver reporting the old NotAfter — and
// the expiry warning up — indefinitely after certbot had already fixed the
// problem the warning was about.
func TestCertExpiryFollowsARenewalWithoutAHandshake(t *testing.T) {
	a, _, seedCertPath, _ := newServingTestApp(t)
	near, _ := certtest.ForWithExpiry(t, testCertName, time.Now().Add(3*24*time.Hour))
	certPath, keyPath := writeCert(t, filepath.Dir(seedCertPath), "near", near)

	enableDoT(t, a, certPath, keyPath, freeTCPAddr(t))
	_, expiringSoon, ok := a.CertExpiry()
	if !ok || !expiringSoon {
		t.Fatalf("precondition: CertExpiry(ok=%v expiringSoon=%v), want a certificate inside the warning window", ok, expiringSoon)
	}

	// certbot replaces the same two paths in place — the whole reason
	// CertProvider watches mtime rather than reading once.
	renewed, _ := certtest.ForWithExpiry(t, testCertName, time.Now().Add(90*24*time.Hour))
	if err := os.WriteFile(certPath, certtest.EncodeCert(renewed), 0o600); err != nil {
		t.Fatalf("rewriting the certificate: %v", err)
	}
	if err := os.WriteFile(keyPath, certtest.EncodeKey(t, renewed), 0o600); err != nil {
		t.Fatalf("rewriting the key: %v", err)
	}
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(certPath, future, future); err != nil {
		t.Fatalf("advancing mtime: %v", err)
	}

	// No handshake, no settings write, no reconcile: just the same read the
	// status endpoint makes.
	if _, expiringSoon, ok := a.CertExpiry(); !ok || expiringSoon {
		t.Errorf("CertExpiry(ok=%v expiringSoon=%v) after a renewal — the warning outlived what it warned about", ok, expiringSoon)
	}
}

// --- Follow-up: a shutdown and a reconcile pass cannot interleave ---------

// Once shutdownAll has run, a reconcile pass starts nothing — over a
// configuration the first half of this test proves does start a listener,
// so "nothing started" below cannot be the settings' doing.
//
// This is the pass App.Shutdown races. applySettings ends in a reconcile
// and a.wg.Wait waits for it, and a pass already past its settings read
// consults nothing further that a shutdown changes: before shutdownAll held
// passMu, such a pass went on to bind fresh listeners and commit them into
// the reconciler that had just been emptied.
func TestServingReconcilingAfterShutdownAllStartsNothing(t *testing.T) {
	ctx := context.Background()
	a, _, certPath, keyPath := newServingTestApp(t)
	addr := freeTCPAddr(t)

	enableDoT(t, a, certPath, keyPath, addr)
	if !a.ServingStatus().DoT.Listening {
		t.Fatal("precondition: DoT never started")
	}

	if err := a.serving.shutdownAll(ctx); err != nil {
		t.Fatalf("shutdownAll: %v", err)
	}

	// Settings still say enabled, the address is free again, and the diff
	// in reconcileOne would bind it: only closed stands in the way.
	a.reconcileServing(ctx)

	if dot, doh := a.serving.snapshot(); dot != nil || doh != nil {
		t.Errorf("the reconciler holds dot=%v doh=%v after a pass that ran post-shutdown", dot, doh)
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("%s is bound after a reconcile pass that ran after shutdownAll: %v", addr, err)
	}
	_ = ln.Close()
}

// reopenForTest puts the reconciler back into its just-constructed state,
// so the next iteration below races a shutdown that has not happened yet.
// Taken under both locks, so it is ordered against the retry loop the test
// App is running.
//
// Test-only, and deliberately not a method in serve.go: closed is one-way
// in production because a reopen is the only way back into the race
// shutdownAll closes.
func reopenForTest(r *servingReconciler) {
	r.passMu.Lock()
	defer r.passMu.Unlock()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = false
	r.dot, r.doh = nil, nil
	r.state = servingState{}
}

// A reconcile pass and a shutdown started together leave nothing bound,
// whichever of the two wins the lock. Only those two orderings exist —
// the pass finishes and the shutdown stops what it started, or the
// shutdown wins and the pass starts nothing — and this asserts the
// property they share, on a real socket rather than on servingState.
//
// Run under -race, it also pins closed's write and read to one lock.
func TestServingConcurrentShutdownAndReconcileLeaveNothingBound(t *testing.T) {
	ctx := context.Background()
	a, _, certPath, keyPath := newServingTestApp(t)
	addr := freeTCPAddr(t)

	// Proven to bind before anything races over it; then taken back down,
	// so the loop starts from a reconciler that has never been shut down.
	enableDoT(t, a, certPath, keyPath, addr)
	if !a.ServingStatus().DoT.Listening {
		t.Fatal("precondition: DoT never started")
	}
	if err := a.serving.shutdownAll(ctx); err != nil {
		t.Fatalf("shutdownAll: %v", err)
	}
	reopenForTest(&a.serving)

	for i := range 200 {
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			a.reconcileServing(ctx)
		}()
		go func() {
			defer wg.Done()
			<-start
			if err := a.serving.shutdownAll(ctx); err != nil {
				t.Errorf("shutdownAll: %v", err)
			}
		}()
		close(start)
		wg.Wait()

		if dot, doh := a.serving.snapshot(); dot != nil || doh != nil {
			t.Fatalf("iteration %d: the reconciler still holds dot=%v doh=%v after a shutdown", i, dot, doh)
		}
		// The claim, on the socket rather than the bookkeeping: a listener
		// the shutdown does not know about is still bound to the address.
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			t.Fatalf("iteration %d: %s is still bound after a reconcile raced a shutdown: %v", i, addr, err)
		}
		if err := ln.Close(); err != nil {
			t.Fatalf("closing the probe listener: %v", err)
		}
		reopenForTest(&a.serving)
	}
}
