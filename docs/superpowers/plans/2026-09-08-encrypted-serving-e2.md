# Milestone E2 — Encrypted serving (DoT/DoH) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Clients can reach dnsaur over DNS-over-TLS and DNS-over-HTTPS, with dnsaur terminating TLS itself, so the last plaintext hop on the query path is closed.

**Architecture:** Two new listeners in front of the existing handler pipeline. DoT is `dns.Server` over a `tls.Listener` — a TCP-only mode added to `internal/dnssrv.Server`. DoH is a small `http.Server` in the same package, which is what lets it build a complete `Request` (the `tsig` field is unexported). Both are started and stopped by a reconciler driven from settings. Nothing above the socket changes: the pipeline never knew which transport carried a query.

**Tech Stack:** Go 1.26, `miekg/dns` v1.1.72, `crypto/tls`, `net/http` (HTTP/2), React 19 + Vite + TanStack Query + `@e412/rnui-react`.

**Spec:** `docs/superpowers/specs/2026-09-08-encrypted-serving-design.md`. Read it first — especially §2 (why the client-identity subsystem is out of scope) and §6 (the reconciler's two failure classes).

## Global Constraints

- **`internal/dnssrv` tests are internal** (`package dnssrv`) — see `internal/dnssrv/server_test.go:1`. `internal/upstream` tests are internal too. `internal/api` tests use `newTestServer(t)` returning **one** value and `ts.do(t, method, path, body string)` with the body as a **JSON string**.
- **`Request.tsig` is unexported** (`internal/dnssrv/pipeline.go:38`) and filled by `Server.serve`. Any transport that builds a `Request` must therefore live in `package dnssrv`. This is why the DoH handler goes there and not in `internal/api`.
- **Client identity must survive.** `Request.ClientIP` comes from `w.RemoteAddr()` and drives `clients.Registry.Lookup`. Every new transport must populate it with the *real* client address. This is the property the whole milestone's shape was chosen for; Task 3 pins it.
- **No PROXY protocol, no `X-Forwarded-For`, no trusted-proxy ACL.** Spec §2. Nothing sits between client and dnsaur on the DNS path. If a task seems to need them, stop and report — the premise has changed.
- **Run every command in the FOREGROUND.** Three tasks in E1 stalled by launching a background test run and then waiting on a notification that never came.
- **A mutation that fails to COMPILE proves nothing.** The test never ran, and a typo produces the same result. Four E1 probes hit this. If a probe produces a build error, adjust it so the code still compiles (keep the variable used) and re-run for real runtime evidence. Report what actually happened; never describe a compile error as evidence a test is load-bearing.
- **Back up before mutating an uncommitted file.** `git checkout` reverts the whole file, destroying the task's work. E1 lost work to this once and nearly again. Back up, restore from the backup, verify byte-identity with `cmp` or `sha256sum`.
- **Check identifiers for redeclaration before writing.** E1 lost a task to defining `answerA` in a second file of the same package. `grep` the package first.
- **Do not re-fixture a protected suite.** If a shared test helper's behaviour changes (a TTL, a default), every existing test using it changes silently while its assertions still read the same. Prefer a new helper to editing one in use.
- `staticcheck ST1008`: `error` is the last return value.
- Gates on the **committed** tree, `git status --porcelain` empty: `go test -race ./...`, `~/go/bin/golangci-lint run ./...`, `gofmt -l internal cmd`, and for web tasks `cd web && pnpm lint && pnpm format:check && pnpm typecheck && pnpm test && pnpm build`.
- **Conventional commits**, including the PR title.
- **Docs are part of the work** (Task 11), not a follow-up.

## File Structure

- `internal/dnssrv/padding.go` — `Pad`/`StripPadding`, moved from `internal/upstream`
- `internal/certtest/certtest.go` — self-signed certificate minting, importable by both packages' tests
- `internal/dnssrv/tls.go` — the TCP-only TLS mode on `Server`
- `internal/dnssrv/certs.go` — the mtime-watching `GetCertificate` provider
- `internal/dnssrv/doh.go` — the RFC 8484 handler and its listener
- `internal/app/serve.go` — the listener reconciler
- `internal/api/settings_handlers.go` — the six settings, per-key and cross-field validation
- `internal/api/server.go` — `ResolverStatus` extended with serving state
- `web/src/components/protocols-field.tsx`, `web/src/pages/settings.tsx` — the Protocols group
- `docs/configuration.md`, `README.md` — the bootstrap requirement, DNS-01, the key-permissions hook

---

### Task 1: Move padding into `internal/dnssrv`

**Files:**
- Create: `internal/dnssrv/padding.go`, `internal/dnssrv/padding_test.go`
- Delete: `internal/upstream/padding.go`, `internal/upstream/padding_test.go`
- Modify: `internal/upstream/dot.go:76,83,110`, `internal/upstream/doh.go:69,110`

**Interfaces:**
- Produces:
  ```go
  package dnssrv

  // PaddingBlockQuery is RFC 8467 §4.1's client profile.
  const PaddingBlockQuery = 128
  // PaddingBlockResponse is RFC 8467 §4.2's server profile.
  const PaddingBlockResponse = 468

  func Pad(m *dns.Msg, block int) error
  func StripPadding(m *dns.Msg)
  ```

**Why here.** `internal/upstream` already imports `internal/dnssrv` (`forwarder.go`), so the dependency direction is right and no new package is needed. The serving side needs the same arithmetic at a different block size, and E1 proved the off-by-four trap in it — two copies would drift.

**This is a pure move, and the existing tests are its proof.** `padQuery` becomes `Pad`, `stripPadding` becomes `StripPadding`, `paddingBlock` becomes `PaddingBlockQuery`. The bodies do not change. `internal/upstream`'s callers change only in the name they call.

- [ ] **Step 1: Record the baseline**

Run: `go test ./internal/upstream/ -run 'TestPad|TestStrip|TestDoT|TestDoH' -v 2>&1 | tail -20`
Expected: PASS. Record the count — the move must not move it.

- [ ] **Step 2: Move the files**

`git mv internal/upstream/padding.go internal/dnssrv/padding.go` and the same for the test. Change `package upstream` to `package dnssrv` in both. Rename the three identifiers as above and export them. Add `PaddingBlockResponse = 468` beside `PaddingBlockQuery`, with this comment:

```go
// PaddingBlockResponse is RFC 8467 §4.2's server profile. A response is
// padded to a multiple of this only when the query that prompted it carried
// a Padding option, and only on an encrypted transport — padding a
// plaintext reply hides nothing and costs bytes.
const PaddingBlockResponse = 468
```

The test file's `query(name string) *dns.Msg` helper moves with it. **Check first whether `internal/dnssrv`'s tests already define something by that name** — if so, rename the incoming one rather than the existing one.

- [ ] **Step 3: Update the callers**

In `internal/upstream/dot.go` and `doh.go`, `padQuery(m, paddingBlock)` becomes `dnssrv.Pad(m, dnssrv.PaddingBlockQuery)` and `stripPadding(r)` becomes `dnssrv.StripPadding(r)`. `dnssrv` is already imported by the package; confirm it is imported in these two files specifically.

- [ ] **Step 4: Run both packages**

Run: `go test -race ./internal/upstream/ ./internal/dnssrv/ 2>&1 | tail -10`
Expected: PASS, with the padding tests now reported under `internal/dnssrv` and the count from Step 1 preserved across the two packages.

- [ ] **Step 5: Confirm nothing else referenced the old names**

Run: `grep -rn "padQuery\|stripPadding\|paddingBlock" internal/ || echo "clean"`
Expected: `clean`. A leftover reference means the move was partial.

- [ ] **Step 6: Commit**

```bash
gofmt -l internal && go test -race ./internal/... > /dev/null && git add -A internal && git commit -m "refactor(dnssrv): move EDNS padding where both sides can reach it"
```

---

### Task 2: `internal/certtest`

**Files:**
- Create: `internal/certtest/certtest.go`
- Modify: `internal/upstream/tlstest_test.go` — drop its own `testCertFor`, call the package

**Interfaces:**
- Produces:
  ```go
  package certtest

  // For mints a self-signed certificate valid for name, and a pool trusting
  // it. Returned separately so a test can hand the pool to one client and
  // deliberately not to another.
  func For(t *testing.T, name string) (tls.Certificate, *x509.CertPool)

  // ForWithExpiry is For with an explicit NotAfter, so a test can mint a
  // certificate that is already close to expiry.
  func ForWithExpiry(t *testing.T, name string, notAfter time.Time) (tls.Certificate, *x509.CertPool)
  ```

**Why a normal package, not a `_test.go` helper.** Helpers in `_test.go` files cannot cross package boundaries, and `internal/dnssrv` needs the same minting `internal/upstream` already has. Duplicating it is how the two copies diverge.

`ForWithExpiry` exists because Task 9's expiry warning needs a certificate that expires soon, and a test that waits 76 days is not a test.

- [ ] **Step 1: Extract**

Move the body of `testCertFor` from `internal/upstream/tlstest_test.go` into `internal/certtest/certtest.go` as `For`, unchanged except for the name and package. Add `ForWithExpiry` by parameterising `NotAfter`; `For` calls it with `time.Now().Add(time.Hour)`, which is what it uses today.

The package imports `testing`, which is unusual for a non-test package and is correct here — it exists only for tests, and the `t *testing.T` parameter is what lets it call `t.Fatalf` on a minting failure rather than returning an error every caller would ignore.

- [ ] **Step 2: Switch `internal/upstream` to it**

Delete `testCertFor` from `tlstest_test.go`; replace its call sites with `certtest.For(t, …)`. **The certificate's properties must not change** — same key type, same SANs, same validity window — or E1's DoT and DoH tests are being re-fixtured under their own assertions.

- [ ] **Step 3: Run E1's suite unmodified**

Run: `go test -race ./internal/upstream/ -run 'TestDoT|TestDoH|TestForwarderUsesDoT|TestEncryptedUpstream' -v 2>&1 | tail -20`
Expected: PASS. Only the call sites changed; no assertion in those tests may have been touched. Confirm with `git diff --stat internal/upstream/`.

- [ ] **Step 4: Commit**

```bash
gofmt -l internal && go test -race ./internal/... > /dev/null && git add -A internal && git commit -m "refactor(certtest): share self-signed certificate minting across packages"
```

---

### Task 3: The DoT listener, and the identity it must preserve

**Files:**
- Create: `internal/dnssrv/tls.go`, `internal/dnssrv/tls_test.go`
- Modify: `internal/dnssrv/server.go` — `Server` gains a `tlsConfig` field; `Start` branches on it

**Interfaces:**
- Consumes: `certtest.For` (Task 2)
- Produces:
  ```go
  // WithTLS makes Start bind a single TCP listener wrapped in TLS, and no
  // UDP socket. DNS-over-TLS has no datagram sibling (RFC 7858 §3).
  func WithTLS(cfg *tls.Config) Option
  ```

**Why `Start` must branch rather than always binding both.** Today `Start` binds a `PacketConn` and a `Listener` on the same port, retrying together when the port is ephemeral. DoT has no UDP side; binding one would occupy port 853/udp for nothing and make the retry loop's pairing meaningless.

- [ ] **Step 1: Write the failing test**

Create `internal/dnssrv/tls_test.go`. **This is the milestone's load-bearing test** — it is the only thing that would notice if client identity stopped surviving the new transport, which is the property the entire design was chosen for.

```go
package dnssrv

import (
	"context"
	"crypto/tls"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/aloks98/dnsaur/internal/certtest"
	"github.com/miekg/dns"
)

// A DoT client's real address reaches the handler.
//
// Everything about per-client filtering hangs off Request.ClientIP:
// clients.Registry.Lookup maps it to a group, and the group decides which
// blocklists and rules apply. A transport that lost it would still answer
// every query correctly-looking — with the default group's rules and the
// wrong attribution in the query log — so nothing else in the suite would
// fail. Hence this test.
func TestDoTPreservesClientIdentity(t *testing.T) {
	cert, pool := certtest.For(t, "dot.test")

	got := make(chan netip.Addr, 1)
	h := HandlerFunc(func(_ context.Context, req *Request) (*Response, error) {
		got <- req.ClientIP
		r := new(dns.Msg)
		r.SetReply(req.Msg)
		return &Response{Msg: r}, nil
	})

	srv := NewServer("127.0.0.1:0", h, WithTLS(&tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}))
	if err := srv.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })

	// The client dials from a source address distinct from the server's, so
	// the assertion below can tell the two apart. All of 127.0.0.0/8 is on
	// `lo` under Linux; where it is not, bind fails and the test skips
	// rather than falling back to an assertion that cannot discriminate.
	const clientSrc = "127.0.0.2"
	c := &dns.Client{
		Net:       "tcp-tls",
		Timeout:   5 * time.Second,
		TLSConfig: &tls.Config{ServerName: "dot.test", RootCAs: pool, MinVersion: tls.VersionTLS12},
		Dialer:    &net.Dialer{LocalAddr: &net.TCPAddr{IP: net.ParseIP(clientSrc)}},
	}
	m := new(dns.Msg)
	m.SetQuestion("example.com.", dns.TypeA)
	if _, _, err := c.Exchange(m, srv.Addr()); err != nil {
		t.Fatalf("Exchange: %v", err)
	}

	select {
	case ip := <-got:
		if !ip.IsValid() {
			t.Fatal("the handler saw no client address at all")
		}
		// Compared against the *specific* source address the client dialled
		// from, not merely "is it loopback". The server binds 127.0.0.1 and
		// the client dials from 127.0.0.2 for exactly this reason: a bug
		// that reads the server's own address off the connection instead of
		// the peer's — w.LocalAddr() where w.RemoteAddr() was meant — is
		// the most likely failure in a transport rewrite, and against a
		// loopback-to-loopback test with an IsLoopback() assertion it
		// passes cleanly. That is not hypothetical; an earlier draft of
		// this test had it, and the review caught it by making that exact
		// swap and watching the test stay green.
		if ip.String() != clientSrc {
			t.Errorf("handler saw ClientIP %v, want %s — the address the client actually dialled from", ip, clientSrc)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the handler never ran")
	}
}

// A TLS server binds TCP only. Binding UDP as well would occupy 853/udp for
// nothing, and DNS-over-TLS has no datagram form.
func TestDoTBindsNoUDPSocket(t *testing.T) {
	cert, _ := certtest.For(t, "dot.test")
	srv := NewServer("127.0.0.1:0", HandlerFunc(func(context.Context, *Request) (*Response, error) {
		return nil, nil
	}), WithTLS(&tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}))
	if err := srv.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })

	// The same port must still be free on UDP.
	pc, err := net.ListenPacket("udp", srv.Addr())
	if err != nil {
		t.Fatalf("UDP port %s is occupied, so the TLS server bound a datagram socket it should not have: %v", srv.Addr(), err)
	}
	_ = pc.Close()
}
```

- [ ] **Step 2: Run it to watch it fail**

Run: `go test ./internal/dnssrv/ -run TestDoT -v 2>&1 | head -20`
Expected: FAIL to compile — `undefined: WithTLS`.

- [ ] **Step 3: Implement**

Create `internal/dnssrv/tls.go`:

```go
package dnssrv

import "crypto/tls"

// WithTLS makes Start bind a single TCP listener wrapped in TLS, and no UDP
// socket.
//
// DNS-over-TLS (RFC 7858) is a stream protocol with no datagram sibling, so
// the paired UDP/TCP bind Start does for plaintext would occupy a datagram
// port for nothing. Everything above the socket is identical — the same
// handler, the same TSIG provider, the same transfer and NOTIFY intercepts:
// none of them ever knew what carried the bytes.
func WithTLS(cfg *tls.Config) Option {
	return func(s *Server) { s.tlsConfig = cfg }
}
```

In `server.go`, add `tlsConfig *tls.Config` to `Server`, and branch at the top of `Start`:

```go
	if s.tlsConfig != nil {
		ln, err := tls.Listen("tcp", s.addr, s.tlsConfig)
		if err != nil {
			return err
		}
		s.bound = ln.Addr().String()
		// Net is deliberately unset: ActivateAndServe uses the Listener it
		// is given, and this one already speaks TLS.
		s.tcp = &dns.Server{Listener: ln, Handler: dns.HandlerFunc(s.serve), TsigProvider: s.tsig}
		go func() {
			if err := s.tcp.ActivateAndServe(); err != nil {
				slog.Error("dot server error", "err", err)
			}
		}()
		return nil
	}
```

`Shutdown` already iterates `[]*dns.Server{s.udp, s.tcp}` skipping nils, so it needs no change — confirm that by reading it rather than assuming.

- [ ] **Step 4: Run the tests**

Run: `go test -race ./internal/dnssrv/ -run TestDoT -v 2>&1 | tail -15`
Expected: PASS, both.

- [ ] **Step 5: Prove the identity test discriminates**

The mutation must *lose the client address*, not break the server — a test that fails when the transport stops working proves nothing about identity:

```bash
cp internal/dnssrv/server.go /tmp/server.go.bak
python3 - <<'EOF'
p = "internal/dnssrv/server.go"
s = open(p).read()
old = "\treq := &Request{Msg: m, ClientIP: ip.Unmap()}"
assert old in s, "MUTATION DID NOT APPLY — anchor not found"
s = s.replace(old, "\treq := &Request{Msg: m} // MUTANT: identity dropped", 1)
open(p, "w").write(s)
print("mutation applied")
EOF
go test ./internal/dnssrv/ -run TestDoTPreservesClientIdentity 2>&1 | tail -5
cp /tmp/server.go.bak internal/dnssrv/server.go && cmp internal/dnssrv/server.go /tmp/server.go.bak && echo "restored byte-identical"
```

Expected: FAIL with "the handler saw no client address at all". If it instead fails to compile, adjust so `ip` stays used and re-run.

- [ ] **Step 6: Commit**

```bash
gofmt -l internal && go test -race ./internal/... > /dev/null && git add -A internal && git commit -m "feat(dnssrv): serve DNS-over-TLS on a TCP-only listener"
```

---

### Task 4: Certificate loading with renewal reload

**Files:**
- Create: `internal/dnssrv/certs.go`, `internal/dnssrv/certs_test.go`

**Interfaces:**
- Consumes: `certtest.For`, `certtest.ForWithExpiry` (Task 2)
- Produces:
  ```go
  // CertProvider serves a keypair from disk, re-reading it when either file
  // changes on disk.
  type CertProvider struct{ ... }

  func NewCertProvider(certPath, keyPath string) *CertProvider

  // GetCertificate satisfies tls.Config.GetCertificate.
  func (p *CertProvider) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error)

  // Leaf returns the currently loaded certificate for inspection — the
  // subject name and NotAfter the Settings screen shows, and the expiry the
  // warning in Task 9 checks. Returns nil before the first successful load.
  func (p *CertProvider) Leaf() *x509.Certificate
  ```

**Why mtime rather than a watcher or a timer.** certbot replaces the files; the next handshake is when it matters. Re-reading on every handshake would be a syscall per connection; a `fsnotify` watcher is a dependency and a goroutine for a file that changes four times a year. Comparing mtime on each handshake and reloading only on a change costs one `Stat` and is correct.

- [ ] **Step 1: Write the failing test**

Create `internal/dnssrv/certs_test.go`. The load-bearing case is the reload — a keypair read once at startup silently breaks 90 days later, which is the failure mode nobody notices until DNS stops:

```go
package dnssrv

import (
	"crypto/tls"
	"os"
	"path/filepath"
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
```

`certtest.EncodeCert` and `certtest.EncodeKey` do not exist yet — add them to `internal/certtest` as part of this task, since the provider is the first thing that needs a keypair *on disk* rather than in memory:

```go
// EncodeCert returns the PEM encoding of cert's leaf.
func EncodeCert(cert tls.Certificate) []byte

// EncodeKey returns the PEM encoding of cert's private key.
func EncodeKey(t *testing.T, cert tls.Certificate) []byte
```

- [ ] **Step 2: Run it to watch it fail**

Run: `go test ./internal/dnssrv/ -run TestCertProvider -v 2>&1 | head -20`
Expected: FAIL to compile — `undefined: NewCertProvider`.

- [ ] **Step 3: Implement**

Write `internal/dnssrv/certs.go`. The provider holds the two paths, the last-seen modification times, the parsed keypair, and a mutex. `GetCertificate` stats both files; if neither mtime has moved and a keypair is cached, it returns the cache; otherwise it reloads with `tls.LoadX509KeyPair`, parses the leaf with `x509.ParseCertificate` so `Leaf()` has something to return, and caches.

Two rules the code must follow, both learned in E1:

- **Never hold the mutex across file I/O.** `internal/upstream/dot.go`'s pool had exactly this bug and it was an Important finding: a slow or hung filesystem would stall every concurrent handshake. Stat and read outside the lock; take it only to swap the cached value in.
- **A failed reload must not discard a working certificate.** If the files are mid-write — certbot is not atomic on every platform — returning an error is right, but throwing away the cached keypair would turn a transient read into an outage. Keep the last good one and report the error for *this* handshake only.

- [ ] **Step 4: Run the tests**

Run: `go test -race ./internal/dnssrv/ -run TestCertProvider -v 2>&1 | tail -15`
Expected: PASS, all four.

- [ ] **Step 5: Prove the reload test discriminates**

This plan cannot give you a `sed` anchor, because you have just written the
code and only you know what the staleness check looks like. So **edit it by
hand**, and prove the edit landed:

```bash
cp internal/dnssrv/certs.go /tmp/certs.go.bak
```

Now change the "have the files changed?" decision to always answer **no**,
so the first-loaded keypair is cached forever. Keep every variable used —
if the mtime values become unused the file will not compile, and a build
error is not evidence (Global Constraints).

```bash
diff /tmp/certs.go.bak internal/dnssrv/certs.go   # MUST show your edit
go test ./internal/dnssrv/ -run TestCertProviderPicksUpARenewal 2>&1 | tail -5
cp /tmp/certs.go.bak internal/dnssrv/certs.go && cmp internal/dnssrv/certs.go /tmp/certs.go.bak && echo "restored byte-identical"
```

Expected: the `diff` shows a non-empty change, then FAIL with "still
serving \"old.test\" after the files changed". **Quote both the diff and
the failure in your report** — a green run against a mutation that never
applied is indistinguishable from a green run against a test that cannot
catch anything.

- [ ] **Step 6: Commit**

```bash
gofmt -l internal && go test -race ./internal/... > /dev/null && git add -A internal && git commit -m "feat(dnssrv): load the TLS keypair from disk and pick up renewals"
```

---

### Task 5: The DoH handler and listener

**Files:**
- Create: `internal/dnssrv/doh.go`, `internal/dnssrv/doh_test.go`

**Interfaces:**
- Consumes: `CertProvider` (Task 4), `certtest.For` (Task 2)
- Produces:
  ```go
  // DoHServer serves RFC 8484 DNS-over-HTTPS at /dns-query.
  type DoHServer struct{ ... }

  func NewDoHServer(addr string, h Handler, tlsCfg *tls.Config) *DoHServer
  func (s *DoHServer) Start() error
  func (s *DoHServer) Addr() string
  func (s *DoHServer) Shutdown(ctx context.Context) error
  ```

The three methods deliberately mirror `dnssrv.Server`'s, so Task 8's reconciler can hold both behind one interface.

**Why this lives in `package dnssrv` and not in `internal/api`.** `Request.tsig` is unexported (`pipeline.go:38`), and a handler that cannot set it cannot build a complete `Request`. Putting DoH here is what lets `RequireTSIG` keep its guarantee.

**Four rules this handler must follow:**

1. **TSIG is *not* verified over DoH, and must say so.** `Server.serve` relies on miekg verifying during message parsing; there is no miekg parse on this path.

   **The zero value already fails closed**, by deliberate design: `Request.RequireTSIG` (`internal/dnssrv/tsig.go:226-234`) returns `ErrTSIGUnavailable` when `r.tsig == nil`, and its comment says so — a hand-built `Request` reports "nobody verified" rather than an empty key with no error. So leaving `req.tsig` nil is *safe*.

   What is **not** safe is setting `&tsigState{}` with a zero `err`, which would let a handler read "checked and fine" from a message nobody checked. Set `&tsigState{err: ErrTSIGUnavailable}` for explicitness, or leave it nil and rely on the documented fail-closed behaviour — but **pin whichever you choose with a test** asserting `req.RequireTSIG()` returns `ErrTSIGUnavailable` on a DoH request. That assertion is the point; the field's value is an implementation detail.
2. **Transfers and NOTIFY are refused.** `Server.serve` intercepts them and streams over a `dns.ResponseWriter`, which does not exist here. RFC 8484 does not contemplate AXFR. Answer `REFUSED` rather than dropping them into the ordinary pipeline, which is what E1's `internal/app` NOTIFY bug did before D4 caught it.
3. **The body is bounded at `dns.MaxMsgSize`.** It comes from a client; an unbounded read is a denial of service.
4. **HTTP/2 must be configured explicitly.** Go wires h2 automatically only through `ListenAndServeTLS`/`ServeTLS`. A hand-built `tls.Listener` handed to `Serve` negotiates HTTP/1.1 unless `NextProtos` contains `h2`. The failure is silent — everything works, one connection per query. Use `srv.ServeTLS` or set `NextProtos: []string{"h2", "http/1.1"}` on the config.

- [ ] **Step 1: Write the failing tests**

Create `internal/dnssrv/doh_test.go`. Write the bodies against this package's existing conventions; the behaviours to pin are:

1. **`POST /dns-query`** with `Content-Type: application/dns-message` and a wire-format body returns a wire-format answer with `Content-Type: application/dns-message`.
2. **`GET /dns-query?dns=<base64url>`**, unpadded encoding, returns the same answer as the equivalent POST.
3. **Client identity survives** — the handler sees the real client address in `Request.ClientIP`, exactly as Task 3 pins for DoT. Assert it is the loopback address the test client dialled from, not the zero value.
4. **HTTP/2 is negotiated** — assert `r.Proto == "HTTP/2.0"` inside the handler. E1's client-side DoH test does the same, and for the same reason: nothing else notices.
5. **A wrong method is 405**; a body that is not a DNS message is 400; a `?dns=` that is not valid base64url is 400.
6. **An AXFR query is REFUSED**, not forwarded into the pipeline.
7. **A body larger than `dns.MaxMsgSize` is rejected** rather than read.
8. **DoH answers identically to plain DNS** — send the same question to a
   plain listener and a DoH listener sharing one handler, and assert the
   answers match. This is spec §10's row, and it is what catches a
   transport wired *beside* the pipeline rather than into it: every other
   test here would still pass if DoH answered from somewhere else entirely.

- [ ] **Step 2: Run them to watch them fail**

Run: `go test ./internal/dnssrv/ -run TestDoH -v 2>&1 | head -20`
Expected: FAIL to compile — `undefined: NewDoHServer`.

- [ ] **Step 3: Implement**

Create `internal/dnssrv/doh.go`. The handler's core:

```go
const dohContentType = "application/dns-message"

func (s *DoHServer) handle(w http.ResponseWriter, r *http.Request) {
	wire, code, err := readQuery(r)
	if err != nil {
		http.Error(w, err.Error(), code)
		return
	}
	m := new(dns.Msg)
	if err := m.Unpack(wire); err != nil {
		http.Error(w, "malformed DNS message", http.StatusBadRequest)
		return
	}
	// A transfer or a NOTIFY has no meaning here: both are answered by
	// Server.serve through a dns.ResponseWriter that streams, and RFC 8484
	// does not contemplate either. Refusing is honest; letting them into the
	// ordinary pipeline is how a NOTIFY once got forwarded upstream.
	if isTransferQuery(m) || isNotify(m) {
		// There is no `refused` helper in this package — build the reply
		// with SetRcode. Checked: only Servfail exists (pipeline.go:82).
		r := new(dns.Msg)
		r.SetRcode(m, dns.RcodeRefused)
		s.write(w, r)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	req := &Request{Msg: m, ClientIP: clientAddr(r)}
	// Nobody verified a signature here — there is no miekg parse on this
	// path. RequireTSIG must report unavailable, never "checked and fine".
	req.tsig = unverifiedTSIG()

	resp, err := s.handler.ServeDNS(ctx, req)
	if err != nil || resp == nil || resp.Msg == nil {
		resp = Servfail(req)
	}
	if m.IsEdns0() != nil && resp.Msg.IsEdns0() == nil {
		resp.Msg.SetEdns0(1232, false)
	}
	s.write(w, resp.Msg)
}
```

`readQuery` handles both methods and the size bound; `clientAddr` parses `r.RemoteAddr` into a `netip.Addr`; `unverifiedTSIG` returns the state from rule 1 — name it whatever `tsig.go`'s shape makes natural.

**RFC 8484 §4.1 note:** unlike the *client* side in E1, a server does not rewrite the message ID — it echoes whatever arrived, which `SetReply` already does.

- [ ] **Step 4: Run the tests**

Run: `go test -race ./internal/dnssrv/ -run TestDoH -v 2>&1 | tail -20`
Expected: PASS, all seven.

- [ ] **Step 5: Prove the identity and h2 tests discriminate**

Two mutations, because they are two claims. Back up the file first; it is uncommitted.

```bash
cp internal/dnssrv/doh.go /tmp/doh.go.bak
sed -i 's/ClientIP: clientAddr(r)/ClientIP: netip.Addr{} \/\/ MUTANT/' internal/dnssrv/doh.go
git diff --stat internal/dnssrv/doh.go 2>/dev/null || echo "(untracked — check by reading the line)"
go test ./internal/dnssrv/ -run TestDoH 2>&1 | tail -5
cp /tmp/doh.go.bak internal/dnssrv/doh.go && cmp internal/dnssrv/doh.go /tmp/doh.go.bak && echo restored
```
Expected: FAIL on the identity test. If it fails to compile because `clientAddr` becomes unused, keep a `_ = clientAddr(r)` and re-run.

Then remove `h2` from `NextProtos` (or switch `ServeTLS` for `Serve`) and confirm the HTTP/2 test fails with `HTTP/1.1`. Restore and verify byte-identity.

- [ ] **Step 6: Commit**

```bash
gofmt -l internal && go test -race ./internal/... > /dev/null && git add -A internal && git commit -m "feat(dnssrv): serve DNS-over-HTTPS (RFC 8484)"
```

---

### Task 6: Response padding on the encrypted serving paths

**Files:**
- Modify: `internal/dnssrv/server.go` — `serve` pads when the transport is encrypted
- Modify: `internal/dnssrv/tls.go` — `WithTLS` records that the transport is encrypted
- Modify: `internal/dnssrv/doh.go` — the same in `handle`
- Test: `internal/dnssrv/padding_serve_test.go`

**Interfaces:**
- Consumes: `Pad`, `PaddingBlockResponse` (Task 1); `WithTLS` (Task 3); `DoHServer` (Task 5)

**Where the padding call goes, and the real reason.** In `Server.serve` the order is: handler → EDNS echo → `ReplyTSIG` → `FitUDPReply` (UDP only) → append the signature → write. **Put padding between the EDNS echo and `ReplyTSIG`.**

Be careful about *why*, because the obvious reason is wrong and an earlier draft of this plan asserted it. `ReplyTSIG` does **not** compute a MAC — it builds an unsigned stub. The signing happens lazily inside `miekg/dns`'s `(*response).WriteMsg`, over whatever the message holds at that moment. So the only hard constraint is **"before `w.WriteMsg`"**, and padding placed anywhere earlier is equally safe as far as TSIG is concerned. (Task 6's review established this by mutation: moving `Pad` past `ReplyTSIG` and even past `FitUDPReply` left the signed-reply test passing.)

**And be careful about the second reason too, because an earlier draft of this correction got that wrong as well.** The obvious replacement — "it sits upstream of `FitUDPReply`, which trims to the client's UDP budget" — does not hold either: `WithTLS` binds a TCP-only listener, so `s.encrypted` and the `isUDP` guard on `FitUDPReply` are *structurally* mutually exclusive. Reordering the two has no observable effect today.

So say what is actually true, and stop there: the only hard constraint is **before `w.WriteMsg`**, where miekg computes the TSIG MAC over the message as it then stands; and the position among the response-shaping steps is **conventional, not forced** — `FitUDPReply` runs only for a UDP reply and `s.encrypted` is only ever true for a TCP-only TLS listener, so the two never compete for the same reply.

Do not reach for a third reason. A draft of this correction once named DNS-over-QUIC as the future condition that would make the ordering matter — also wrong (RFC 9250 maps DoQ onto QUIC streams, TCP-shaped, with no datagram budget of its own) — and it is not replaced with another guess. If some future transport ever does create the conflict, whoever adds it can say so then.

**Pad only when the query was padded** (RFC 8467 §4.2), and only on an encrypted transport. A plaintext reply gains nothing and loses bytes.

- [ ] **Step 1: Write the failing test**

Pin four behaviours in `internal/dnssrv/padding_serve_test.go`:

1. A **padded query over DoT** gets a response whose wire length is a multiple of `PaddingBlockResponse`.
2. An **unpadded query over DoT** gets an unpadded response — the presence of the client's option is what triggers it.
3. A **padded query over plain TCP** gets an **unpadded** response. This is the test that a refactor hoisting `Pad` too high would fail, and without it nothing distinguishes "pads correctly" from "pads everything".
4. A **signed (TSIG) padded query over DoT still verifies** — proof that padding and TSIG signing coexist, so a padded reply to a signed query is still accepted. Note what this does *not* prove: it is not an ordering test, because padding anywhere before `w.WriteMsg` verifies fine. Its value is catching padding that corrupts the OPT record in a way that breaks signing.

- [ ] **Step 2: Run it to watch it fail**

Run: `go test ./internal/dnssrv/ -run TestServePadding -v 2>&1 | head -20`
Expected: FAIL — responses are not padded.

- [ ] **Step 3: Implement**

Add an `encrypted bool` field to `Server`, set by `WithTLS`. In `serve`, immediately after the EDNS echo block and before the `var sig *dns.TSIG` line:

```go
	// RFC 8467 §4.2. Only when the client padded, and only on an encrypted
	// transport: a plaintext reply gains nothing from padding and costs
	// bytes. Before ReplyTSIG on purpose — the signature covers the message
	// as it goes on the wire, so padding added after signing would
	// invalidate it.
	if s.encrypted && hasPadding(m) {
		if err := Pad(resp.Msg, PaddingBlockResponse); err != nil {
			slog.Error("padding the reply", "err", err)
		}
	}
```

Add the same in `DoHServer.handle` after its EDNS echo. `hasPadding(m *dns.Msg) bool` reports whether the query's OPT carries an `*dns.EDNS0_PADDING`; put it in `padding.go` beside `Pad`.

- [ ] **Step 4: Run the tests**

Run: `go test -race ./internal/dnssrv/ 2>&1 | tail -10`
Expected: PASS, including every pre-existing test in the package.

- [ ] **Step 5: Prove the plaintext case discriminates**

```bash
cp internal/dnssrv/server.go /tmp/server.go.bak
sed -i 's/if s.encrypted \&\& hasPadding(m) {/if hasPadding(m) {/' internal/dnssrv/server.go
go test ./internal/dnssrv/ -run TestServePadding 2>&1 | tail -5
cp /tmp/server.go.bak internal/dnssrv/server.go && cmp internal/dnssrv/server.go /tmp/server.go.bak && echo restored
```
Expected: FAIL on the plaintext case — a plain reply came back padded.

- [ ] **Step 6: Commit**

```bash
gofmt -l internal && go test -race ./internal/... > /dev/null && git add -A internal && git commit -m "feat(dnssrv): pad encrypted responses per RFC 8467"
```

---

### Task 7: Settings and validation

**Files:**
- Modify: `internal/api/settings_handlers.go` — six keys, per-key validators, the cross-field check
- Modify: `internal/app/app.go` — `defaultSettings()` gains the six
- Test: `internal/api/settings_handlers_test.go`

**Interfaces:**
- Produces: the six keys of spec §7, and
  ```go
  // validateCrossField runs after the per-key validator, with the value that
  // is about to be written and the store's current values for everything
  // else. It exists because "enable DoT" is only valid against the state of
  // serve.tls.cert and serve.tls.key, which a per-key validator cannot see.
  func validateCrossField(key, value string, current map[string]string) error
  ```

**The shape change, and why it is needed.** `editableSettings` is `map[string]func(string) error` — per key. Enabling a protocol is only valid if a certificate and key are already set and load as a pair. So `handleSettingsPut` reads the current settings and calls `validateCrossField` before writing.

**Order matters, and the message must say so.** Certificate paths have to be saved before a protocol is enabled. `"set serve.tls.cert and serve.tls.key first"` is actionable; `"invalid value"` is not.

- [ ] **Step 1: Write the failing test**

Add to `internal/api/settings_handlers_test.go`, following the file's existing harness (`newTestServer(t)`, `ts.do(t, method, path, body)`, and decoding the `error` field rather than substring-matching the raw body — E1 learned that `errJSON` escapes quotes):

- Enabling `serve.dot.enabled` with no certificate configured → **400**, and the message names both `serve.tls.cert` and `serve.tls.key`.
- Setting `serve.tls.cert` to a path that does not exist → **400** naming the path.
- Setting a valid certificate pair, **then** enabling → **204** for both.
- `serve.dot.listen` of `"not-an-address"` → **400**; of `":853"` → **204**.
- Setting a certificate and a key that are **not a pair** → 400 saying so.
- Disabling a protocol never requires a certificate → **204** even with none configured.

Use `internal/certtest` to write a real keypair to `t.TempDir()` for the valid cases.

- [ ] **Step 2: Run it to watch it fail**

Run: `go test ./internal/api/ -run TestSettingsServe -v 2>&1 | tail -20`
Expected: FAIL — the keys are not in `editableSettings`, so every PUT is `400 setting not editable`.

- [ ] **Step 3: Implement**

Add the six entries with per-key validators (`boolean`, `listenAddr`, `absPathOrEmpty`), then the cross-field function and its call site in `handleSettingsPut`. Keep the existing `"invalid value for " + key + ": "` prefix — other tests match on it.

Add the six to `defaultSettings()` in `internal/app/app.go` with the spec §7 defaults, so a fresh install has them and `GET /settings` returns them.

- [ ] **Step 4: Run the tests**

Run: `go test -race ./internal/api/ ./internal/app/ 2>&1 | tail -10`
Expected: PASS.

- [ ] **Step 5: Prove the cross-field check is what rejects**

Neutralise `validateCrossField` to `return nil`, confirm the "enable with no certificate" case fails, restore, and verify byte-identity.

- [ ] **Step 6: Commit**

```bash
gofmt -l internal && go test -race ./internal/... > /dev/null && git add -A internal && git commit -m "feat(api): settings for the encrypted serving protocols"
```

---

### Task 8: The listener reconciler

**Files:**
- Create: `internal/app/serve.go`, `internal/app/serve_test.go`
- Modify: `internal/app/app.go` — `applySettings` calls the reconciler; `Shutdown` stops it

**Interfaces:**
- Consumes: `dnssrv.NewServer` + `WithTLS` (Task 3), `dnssrv.NewDoHServer` (Task 5), `dnssrv.NewCertProvider` (Task 4), the settings (Task 7)
- Produces:
  ```go
  // servingState is what the reconciler achieved, read by the status
  // endpoint in Task 9.
  type servingState struct {
      DoT protocolState
      DoH protocolState
  }
  type protocolState struct {
      Enabled   bool
      Listening bool
      Addr      string
      Err       string // the bind error, when Enabled && !Listening
  }
  ```

**The rules, from spec §6:**

- Diff desired against actual: stop what is gone, start what is new, **leave unchanged listeners strictly alone.** Toggling DoH must not drop live DoT connections.
- An **address change is stop-then-start** — the new listener cannot bind while the old holds the address. If the start fails, **report and leave the protocol down**; do not revert to the old address, because a listener serving an address the settings no longer name is a worse lie than one that is honestly off.
- **Plain `:53` stays out of it**, built from `dns_listen` in config as it is today.
- Bind failures are recorded in `servingState`, not just logged. Ports 853 and 443 are privileged; this will happen.
- **"Enabled but no usable certificate" is a live state, not an impossible one.** Task 7's review established that it is reachable through the API: enable with a valid pair, then blank one half. Blocking that write was rejected deliberately, because it would leave an operator unable to repair a broken certificate while a protocol is enabled. And the reconciler must cope regardless — a file can be deleted, replaced badly, or lose read permission at any moment after a perfectly valid save. So the reconciler **must not assume a validated setting implies a loadable keypair**: treat the failure as an ordinary bind-time failure, reported through status (§8), and keep any listener that is already running rather than tearing it down on a certificate it cannot re-read.

**Lock discipline, carried from E1.** `applySettings` documents the order `routeMu → swappable.mu`, and E1's Important finding was a `Close` under a mutex. Do not hold a lock across `Start`, `Shutdown`, or any socket operation. Take it only to swap state.

- [ ] **Step 1: Write the failing test**

`internal/app/serve_test.go`. Two of these are load-bearing:

1. **Enabling DoT starts a listener** that answers a real DoT query.
2. **Toggling DoH leaves a live DoT connection working** — open a DoT connection, keep it open, enable DoH, then send a query on the *same* connection and assert it still answers. This is the test a rebuild-everything reconciler fails, and nothing else would catch it.
3. **Disabling stops the listener** — the port becomes bindable again.
4. **A bind failure is recorded** — occupy the port first, enable, assert `servingState.DoT.Listening` is false and `Err` is non-empty.
5. **An address change moves the listener** and leaves nothing on the old address.
6. **A settings write unrelated to serving disturbs neither listener.**

- [ ] **Step 2: Run it to watch it fail**

Run: `go test ./internal/app/ -run TestServing -v 2>&1 | head -20`
Expected: FAIL to compile.

- [ ] **Step 3: Implement**

Write the reconciler. Hold the active listeners in a small struct keyed by protocol, each holding its address and a handle with `Shutdown(ctx) error`. Since `dnssrv.Server` and `dnssrv.DoHServer` both expose `Start`, `Addr` and `Shutdown`, declare a tiny local interface rather than special-casing each.

Call it from `applySettings` **after** the forwarder swap, so a settings write that changes both does the DNS-path work first.

- [ ] **Step 4: Run the tests**

Run: `go test -race ./internal/app/ 2>&1 | tail -10`
Expected: PASS.

- [ ] **Step 5: Prove the leave-alone rule holds**

Make the reconciler stop and restart every listener on each reconcile — the natural wrong implementation — and confirm test 2 fails because the held DoT connection died. Restore and verify byte-identity.

- [ ] **Step 6: Commit**

```bash
gofmt -l internal && go test -race ./internal/... > /dev/null && git add -A internal && git commit -m "feat(app): start and stop encrypted listeners from settings"
```

---

### Task 9: Status and warnings

**Files:**
- Modify: `internal/api/server.go` — extend `ResolverStatus`
- Modify: `internal/api/settings_handlers.go` — `handleResolverStatus` reports the new facts
- Modify: `internal/api/openapi.yaml` — the extended response
- Modify: `internal/app/app.go` — implement the new methods
- Test: `internal/api/settings_handlers_test.go`, `internal/app/serve_test.go`

**Interfaces:**
- Consumes: `servingState` (Task 8), `CertProvider.Leaf()` (Task 4)
- Produces: `ResolverStatus` gains
  ```go
  // ProtocolStatus is declared in internal/api, because ResolverStatus is
  // an api interface and internal/app cannot be imported from here (it
  // imports this package). internal/app converts its own unexported
  // protocolState into this on the way out.
  type ProtocolStatus struct {
      Enabled   bool   `json:"enabled"`
      Listening bool   `json:"listening"`
      Addr      string `json:"addr"`
      Err       string `json:"error,omitempty"`
  }

  // Serving reports each encrypted protocol's intent and reality.
  Serving() (dot, doh ProtocolStatus)
  // CertExpiry reports the loaded certificate's expiry, and whether it is
  // within the warning threshold. ok is false when no certificate is loaded.
  CertExpiry() (notAfter time.Time, expiringSoon, ok bool)
  ```
  and the JSON response gains `serving` and `certificate` objects beside the existing `encryption_downgraded`.

**Extend the existing interface, do not add a second one.** E1 built `ResolverStatus` for exactly this class of fact — server state the UI must show that is not a setting — and its doc comment says "today, exactly one thing", which is an invitation, not a boundary.

**The expiry threshold is a constant, 14 days.** Not a setting: it exists to catch a delivery mechanism that stopped working, and an operator who can tune it can tune it to never fire.

- [ ] **Step 1: Write the failing test**

- `GET /api/v1/resolver/status` reports, per protocol, enabled and listening, and the bind error when it failed. Authenticated; **401 unauthenticated** — reuse the existing route-auth assertions.
- A certificate expiring inside 14 days sets `expiring_soon`; one expiring in a year does not. Use `certtest.ForWithExpiry`.
- `GET /api/v1/settings` still returns a **pure settings map** with no serving state leaked into it — E1 has this test; extend it rather than writing a second.

- [ ] **Step 2: Run it to watch it fail, then implement, then re-run**

Run: `go test ./internal/api/ -run 'TestResolverStatus|TestSettings' -v 2>&1 | tail -20`

Implement the interface methods on `App`, reading `servingState` and `CertProvider.Leaf()`. Update `openapi.yaml` — `internal/api/openapi_test.go` asserts the spec against the route table in **both** directions, so an undocumented field is caught but an undocumented *route* is a hard failure.

- [ ] **Step 3: Prove the expiry threshold fires**

Mint a certificate expiring in 13 days and one in 15; assert the first warns and the second does not. A threshold test that only checks the warning case cannot tell a working threshold from one that always fires.

- [ ] **Step 4: Commit**

```bash
gofmt -l internal && go test -race ./internal/... > /dev/null && git add -A internal && git commit -m "feat(api): report encrypted-serving state and certificate expiry"
```

---

### Task 10: The Protocols group

**Files:**
- Create: `web/src/components/protocols-field.tsx`, `web/src/components/protocols-field.test.tsx`
- Modify: `web/src/pages/settings.tsx` — the Protocols group
- Modify: `web/src/hooks/use-settings.ts` — the extended `ResolverStatus` type
- Modify: `web/src/components/encryption-downgrade-banner.tsx` or a sibling — the two new banners

**Interfaces:**
- Consumes: the extended `/resolver/status` response (Task 9), `WarningStrip` and the shell mount (both shipped in E1)

**The artboard is authoritative and is being updated in parallel.** Do not start this task until it is on disk; the controller will supply the path. Where the artboard disagrees with anything below, the artboard wins.

**The shape, from spec §8:** each protocol row shows **intent** (a checkbox) and **reality** (whether the socket bound) — `● listening on :853`, `● not listening — bind :853: address already in use`, or `○ off`. A checked box alone is the same lie the encryption-downgrade banner exists to prevent.

The certificate block is two path inputs plus a verification line: the subject name and expiry when valid, the reason when unreadable. That line is what turns "did I type the right path" into a question the screen answers.

Two shell banners, reusing `WarningStrip`: a protocol enabled but not listening, and a certificate expiring within 14 days.

**Copy rule: state the fact and stop.** No line explaining what encrypted serving does or does not protect — that is `docs/configuration.md`, written in Task 11.

- [ ] **Step 1: Write the failing tests**

Pin: a disabled protocol shows `off`; an enabled-and-listening one shows its address; an enabled-but-failed one shows the bind error and is visually distinct; the certificate line shows subject and expiry when valid and the reason when not; and **the two banners appear on a non-Settings route**, since they are shell-mounted (E1's `app-shell.test.tsx` has the pattern).

- [ ] **Step 2: Run, implement, re-run**

Run: `cd web && pnpm vitest run src/components/protocols-field.test.tsx`

- [ ] **Step 3: Look at it**

```bash
cd web && pnpm build && cd .. && go build -o /tmp/dnsaur ./cmd/dnsaur
```

Run it bound to the host's LAN address (`0.0.0.0` and the host IP, **not** `127.0.0.1`) and check each state against the artboard. **Build the binary from the repository root** — running `go build ./cmd/dnsaur` from `web/` fails, and a stale binary serving old embedded assets looks identical to a working one.

- [ ] **Step 4: Commit**

```bash
cd web && pnpm lint && pnpm format:check && pnpm typecheck && pnpm test && pnpm build && cd .. && git add web && git commit -m "feat(web): the Protocols settings group"
```

---

### Task 11: Documentation

**Files:**
- Modify: `docs/configuration.md` — the six settings and a new section
- Modify: `README.md` — the feature line, which currently says encrypted serving is planned
- Modify: `internal/api/openapi.yaml` — if Task 9 left anything undescribed

- [ ] **Step 1: Write the configuration section**

Cover, taking every example from the code rather than inventing them:

- The six settings and their defaults.
- **The bootstrap requirement** — a DoT client is configured with a hostname, resolves it using the resolver the network handed it (dnsaur, in plaintext), and then connects. So **the DoT/DoH hostname must resolve to the listener's address for internal clients**, and dnsaur is what makes that true. Without this line the first deployment fails confusingly.
- **DNS-01 is the only usable ACME challenge** when the hostname resolves to a private address, because Let's Encrypt cannot reach it for HTTP-01 or TLS-ALPN-01.
- **The key-permissions hook.** certbot writes `privkey.pem` as `0600 root:root`; dnsaur binding 53 and 853 may be running with `CAP_NET_BIND_SERVICE` rather than as root, and then cannot read it. Give the `--deploy-hook` that fixes it.
- That certificate **delivery is out of scope** — certbot, rsync or a manual copy all work, because the reload watches the file rather than the process.
- What this protects and what it does not: other devices on the network stop seeing queries; dnsaur still sees all of them.

- [ ] **Step 2: Update the README**

E1 left the feature list saying encrypted serving was planned. It is now shipped — update that line rather than adding a second.

- [ ] **Step 3: Check every link**

```bash
grep -rn "](.*\.md" README.md docs/ | grep -v http | while IFS= read -r line; do
  f=$(echo "$line" | sed 's/.*](\([^)#]*\).*/\1/')
  d=$(dirname "$(echo "$line" | cut -d: -f1)")
  [ -e "$d/$f" ] || echo "BROKEN: $line"
done
```
Expected: no output.

- [ ] **Step 4: Commit**

```bash
git add README.md docs internal/api/openapi.yaml && git commit -m "docs: encrypted serving (DoT/DoH)"
```

---

## Done when

- `go test -race ./...` passes, `~/go/bin/golangci-lint run ./...` is clean, `gofmt -l internal cmd` is empty.
- `cd web && pnpm lint && pnpm format:check && pnpm typecheck && pnpm test && pnpm build && pnpm test:e2e` passes.
- A DoT client's real address reaches `Request.ClientIP` — the property the whole design was chosen for, pinned by Task 3.
- Toggling one protocol does not disturb the other's live connections.
- A protocol enabled but not listening says so on screen, and a certificate within 14 days of expiry warns.
- `git status --porcelain` is empty and every mutation used as evidence has been reverted.
