# Milestone E1 — Encrypted upstreams (DoT/DoH) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Queries dnsaur forwards to public resolvers travel over TLS instead of plaintext UDP, so the path — the ISP first among them — stops seeing every name this network looks up.

**Architecture:** One seam. `up` currently hardcodes two `*dns.Client`s; it gains a single `exchanger` field with three implementations (plain, DoT, DoH). Everything above the seam — health tracking, the three-strike backoff, strategy selection, the RFC 9520 failure cache, `SetConditional`'s reuse-by-address — is untouched, and the existing forwarder suite passing unmodified is the proof of that. Configuration gains a scheme and a `#name` suffix in the Unbound/systemd-resolved style, parsed by one function that the app and the API validator both call.

**Tech Stack:** Go 1.26, `miekg/dns` v1.1.72, `crypto/tls`, `net/http` (HTTP/2), React 19 + Vite + TanStack Query + `@e412/rnui-react`.

**Spec:** `docs/superpowers/specs/2026-09-05-encrypted-upstreams-design.md`. Read it first — it records *why* each rule below is the rule, especially §3 (why hostnames are rejected rather than resolved) and §6 (why the stale-connection retry is not optional).

## Global Constraints

- **`internal/upstream` tests are internal** (`package upstream`) — see `internal/upstream/forwarder_test.go:1`. They can reach unexported identifiers, and every test in this plan does.
- **No assertion in `internal/upstream/forwarder_test.go` may change.** Task 3 moves today's exchange logic into `plainExchanger` verbatim; that suite passing on its existing assertions *is* the proof plaintext behaviour did not move. Re-baselining an assertion is a defect, not a fix.
  - **The one permitted edit** is line 309's `newUp("127.0.0.1:1", 100*time.Millisecond)`, whose signature Task 3 changes. It becomes `newUp(Upstream{Scheme: SchemePlain, Addr: "127.0.0.1:1", Canonical: "127.0.0.1:1"}, 100*time.Millisecond)`. `TestMarkResultPenalizesFailingUpstreamEwma` is a pure `markResult` unit test that never touches the transport, so this is a constructor call being updated and nothing else. Any *other* edit to that file is a defect. This is why `Config.Upstreams` stays `[]string` (see below) rather than becoming `[]Upstream`, which would have forced a mechanical edit at every construction site and destroyed the signal.
- **`upstream.Config.Upstreams` keeps type `[]string`.** `New` parses the entries itself via `parseEntries`. A bare `"127.0.0.1:5353"` parses to a plain upstream, which is exactly what every existing test means by it.
- **Plain entries keep today's permissiveness, deliberately.** `parseUpstreams` (`internal/app/app.go:294`) validates nothing beyond splittability — a plain upstream may be a hostname (`resolver.lan:5353`), and Go's dialer resolves it. Tightening that would reject configurations that work today and is out of scope. **Only `tls://` and `https://` entries get host-is-an-IP and port-range validation**, where there is nothing to break.
- **Camp 1: a hostname in the host position of an encrypted entry is REJECTED, never resolved.** Spec §3. There is no bootstrap resolver in this milestone and adding one is not a fix for a failing test.
- **Never downgrade.** No code path may retry an encrypted upstream in plaintext, for any reason — not on handshake failure, not on certificate failure, not on timeout. Task 7 asserts this with a listener that counts packets it must never receive.
- **`internal/upstream/testdata/grammar.json` is the single source of truth for the grammar.** The Go table test (Task 1) and the TypeScript mirror test (Task 8) both read that one file. A grammar change means changing the fixture, and both suites then fail until both parsers agree. Do not fork it.
- **Rejection reasons are compared by `Code`, never by message text.** `*ParseError` carries a stable code; Go renders one message and the web renders its own copy. A test asserting a cross-language message string is coupling copy to logic.
- **The API validator's error prefix stays `"invalid value for " + key`.** Task 2 changes the validator's *signature* (returning `error` instead of `bool`) and appends the reason; the existing prefix must survive so `internal/api` tests asserting on it by substring keep passing.
- `staticcheck ST1008`: `error` is the last return value.
- Gates on the **committed** tree, `git status --porcelain` empty: `go test -race ./...`, `~/go/bin/golangci-lint run ./...`, `gofmt -l internal cmd`, and for web tasks `cd web && pnpm lint && pnpm format:check && pnpm typecheck && pnpm test && pnpm build`.
- **Make the mutation's landing observable, not inferred.** A probe whose anchor no longer matches — because `gofmt` or `oxfmt` reindented the line, or an earlier fix moved it — prints its error and leaves the suite running against unmutated code. It then reports green, and *a green run against a mutation that never applied is indistinguishable from a green run against a mutation the suite cannot catch*. Diff the file, or have the probe print that it applied, before believing the result.
- **Choose a probe that discriminates.** Every fix's test is shown failing with the fix removed. If a proposed probe would pass against a broken implementation, say so instead of running it — a probe that cannot fail is worse than no probe, because it is recorded as evidence.
- **Conventional commits**, including on the PR title.
- **Docs are part of the work, not a follow-up** (Task 10): `README.md` and the linked `docs/` tree are updated in the same branch.

### Three refinements this plan makes to the spec

The spec was amended to match before this plan was written; they are listed here so a reviewer reading either document sees the same thing.

1. **`Upstream.Canonical`, not `Upstream.Raw`.** The identity field is the *normalized* spelling (port and path always written), not the entry as typed, so `tls://1.1.1.1#n` and `tls://1.1.1.1:853#n` are one upstream sharing one health record instead of two. Matches `FormatForwardTo`'s existing convention.
2. **No `FormatUpstreams`.** Nothing in production would call it. `Canonical` is a field; a round-trip test joins with `strings.Join`.
3. **`stripPadding` keeps the OPT record** even when Padding was its only option. The OPT also carries the DO bit, the extended rcode bits and the advertised UDP size; discarding those to save eleven bytes is a bad trade.

## File Structure

- `internal/upstream/addr.go` — the entry grammar: `Scheme`, `Upstream`, `ParseUpstreams`, `ParseError`
- `internal/upstream/testdata/grammar.json` — the accept/reject fixture both languages read
- `internal/upstream/exchanger.go` — the `exchanger` interface and `plainExchanger`
- `internal/upstream/padding.go` — `padQuery` and `stripPadding` (RFC 7830 / RFC 8467)
- `internal/upstream/dot.go` — `dotExchanger` and its connection pool
- `internal/upstream/doh.go` — `dohExchanger`
- `internal/upstream/tlstest_test.go` — the self-signed certificate and the local DoT/DoH servers
- `internal/upstream/forwarder.go` — `up` reshaped around the seam, `Forwarder.Close`, `newUp` dispatch
- `internal/app/app.go` — `parseUpstreams` deleted, the displaced forwarder closed
- `internal/api/settings_handlers.go` — validators return `error`; `upstreams` gets a real one
- `internal/api/openapi.yaml` — the extended grammar
- `web/src/lib/upstreams.ts` — the TypeScript mirror of the grammar
- `web/src/components/upstreams-field.tsx` — protocol selector, presets, per-entry rows
- `web/src/pages/settings.tsx` — the `kind: "upstreams"` field wiring
- `docs/configuration.md`, `README.md` — the grammar, the two camps in brief, the "the resolver still sees your queries" caveat

---

### Task 1: The entry grammar

**Files:**
- Create: `internal/upstream/addr.go`
- Create: `internal/upstream/addr_test.go`
- Create: `internal/upstream/testdata/grammar.json`

**Interfaces:**
- Produces:
  ```go
  type Scheme string

  const (
      SchemePlain Scheme = "udp"   // UDP with TCP fallback on truncation
      SchemeDoT   Scheme = "tls"
      SchemeDoH   Scheme = "https"
  )

  // Upstream is one parsed entry. Addr is always a literal host:port and
  // reaching it never requires a DNS lookup for tls:// and https:// —
  // spec §3.
  type Upstream struct {
      Scheme     Scheme
      Addr       string // "1.1.1.1:853"
      VerifyName string // certificate name; empty for SchemePlain
      Path       string // DoH only; "/dns-query" when unset
      Canonical  string // normalized spelling; the reuse and display identity
  }

  // ParseError is a rejection with a stable Code, so the web mirror can
  // assert the same reason without depending on this package's wording.
  type ParseError struct {
      Entry string
      Code  string
      Msg   string
  }
  func (e *ParseError) Error() string

  // ParseUpstreams parses the comma-separated `upstreams` setting value.
  func ParseUpstreams(s string) ([]Upstream, error)

  // parseEntries parses already-split entries. upstream.New goes through
  // here too, so a Forwarder built directly in a test takes the same
  // grammar as one built from the setting.
  func parseEntries(entries []string) ([]Upstream, error)
  ```

**The codes, and nothing else, are the cross-language contract:** `host_not_ip`, `missing_name`, `name_on_plain`, `mixed_schemes`, `bad_scheme`, `bad_addr`, `bad_url`, `empty`.

**Why `url.Parse` for the schemed forms and hand-splitting for the bare one.** `tls://1.1.1.1:853#cloudflare-dns.com` and `https://1.1.1.1/dns-query#name` are real URLs and `url.Parse` gets the host, port, path and fragment right including bracketed IPv6. `1.1.1.1:53` is not a URL — `url.Parse` reads `1.1.1.1` as the scheme. So: if the entry contains `://`, parse it as a URL; otherwise it is a bare plain entry and takes today's `parseUpstreams` treatment verbatim.

- [ ] **Step 1: Write the grammar fixture**

Create `internal/upstream/testdata/grammar.json`:

```json
{
  "accept": [
    { "in": "1.1.1.1:53", "scheme": "udp", "addr": "1.1.1.1:53", "verifyName": "", "path": "", "canonical": "1.1.1.1:53" },
    { "in": "1.1.1.1", "scheme": "udp", "addr": "1.1.1.1:53", "verifyName": "", "path": "", "canonical": "1.1.1.1:53" },
    { "in": "udp://9.9.9.9:53", "scheme": "udp", "addr": "9.9.9.9:53", "verifyName": "", "path": "", "canonical": "9.9.9.9:53" },
    { "in": "resolver.lan:5353", "scheme": "udp", "addr": "resolver.lan:5353", "verifyName": "", "path": "", "canonical": "resolver.lan:5353" },
    { "in": "::1", "scheme": "udp", "addr": "[::1]:53", "verifyName": "", "path": "", "canonical": "[::1]:53" },
    { "in": "[::1]:53", "scheme": "udp", "addr": "[::1]:53", "verifyName": "", "path": "", "canonical": "[::1]:53" },
    { "in": "tls://1.1.1.1:853#cloudflare-dns.com", "scheme": "tls", "addr": "1.1.1.1:853", "verifyName": "cloudflare-dns.com", "path": "", "canonical": "tls://1.1.1.1:853#cloudflare-dns.com" },
    { "in": "tls://1.1.1.1#cloudflare-dns.com", "scheme": "tls", "addr": "1.1.1.1:853", "verifyName": "cloudflare-dns.com", "path": "", "canonical": "tls://1.1.1.1:853#cloudflare-dns.com" },
    { "in": "tls://[2606:4700:4700::1111]:853#cloudflare-dns.com", "scheme": "tls", "addr": "[2606:4700:4700::1111]:853", "verifyName": "cloudflare-dns.com", "path": "", "canonical": "tls://[2606:4700:4700::1111]:853#cloudflare-dns.com" },
    { "in": "https://1.1.1.1/dns-query#cloudflare-dns.com", "scheme": "https", "addr": "1.1.1.1:443", "verifyName": "cloudflare-dns.com", "path": "/dns-query", "canonical": "https://1.1.1.1:443/dns-query#cloudflare-dns.com" },
    { "in": "https://9.9.9.9#dns.quad9.net", "scheme": "https", "addr": "9.9.9.9:443", "verifyName": "dns.quad9.net", "path": "/dns-query", "canonical": "https://9.9.9.9:443/dns-query#dns.quad9.net" },
    { "in": "https://8.8.8.8:8443/resolve#dns.google", "scheme": "https", "addr": "8.8.8.8:8443", "verifyName": "dns.google", "path": "/resolve", "canonical": "https://8.8.8.8:8443/resolve#dns.google" }
  ],
  "reject": [
    { "in": "tls://cloudflare-dns.com:853#cloudflare-dns.com", "code": "host_not_ip" },
    { "in": "https://dns.google/dns-query#dns.google", "code": "host_not_ip" },
    { "in": "tls://1.1.1.1:853", "code": "missing_name" },
    { "in": "https://1.1.1.1/dns-query", "code": "missing_name" },
    { "in": "tls://1.1.1.1:853#", "code": "missing_name" },
    { "in": "1.1.1.1:53#cloudflare-dns.com", "code": "name_on_plain" },
    { "in": "udp://1.1.1.1:53#cloudflare-dns.com", "code": "name_on_plain" },
    { "in": "quic://1.1.1.1:853#dns.adguard.com", "code": "bad_scheme" },
    { "in": "tls://1.1.1.1:99999#cloudflare-dns.com", "code": "bad_addr" },
    { "in": "tls://1.1.1.1:domain#cloudflare-dns.com", "code": "bad_addr" },
    { "in": "tls://user:pw@1.1.1.1:853#cloudflare-dns.com", "code": "bad_url" },
    { "in": "https://1.1.1.1/dns-query?ct=1#cloudflare-dns.com", "code": "bad_url" }
  ],
  "acceptList": [
    { "in": "tls://1.1.1.1:853#cloudflare-dns.com, tls://1.0.0.1:853#cloudflare-dns.com", "count": 2 },
    { "in": "1.1.1.1:53, , 9.9.9.9:53", "count": 2 },
    { "in": "1.1.1.1:53,9.9.9.9", "count": 2 }
  ],
  "rejectList": [
    { "in": "1.1.1.1:53,tls://9.9.9.9:853#dns.quad9.net", "code": "mixed_schemes" },
    { "in": "tls://1.1.1.1:853#a,https://9.9.9.9/dns-query#b", "code": "mixed_schemes" },
    { "in": "", "code": "empty" },
    { "in": "  ,  ", "code": "empty" }
  ]
}
```

`resolver.lan:5353` in `accept` is the regression guard for the Global Constraint above: plain entries may be hostnames and must keep working.

- [ ] **Step 2: Write the failing test**

Create `internal/upstream/addr_test.go`:

```go
package upstream

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
)

type grammarFixture struct {
	Accept []struct {
		In, Scheme, Addr, VerifyName, Path, Canonical string
	} `json:"accept"`
	Reject []struct {
		In, Code string
	} `json:"reject"`
	AcceptList []struct {
		In    string
		Count int
	} `json:"acceptList"`
	RejectList []struct {
		In, Code string
	} `json:"rejectList"`
}

func loadGrammar(t *testing.T) grammarFixture {
	t.Helper()
	b, err := os.ReadFile("testdata/grammar.json")
	if err != nil {
		t.Fatalf("reading the grammar fixture: %v", err)
	}
	var f grammarFixture
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatalf("parsing the grammar fixture: %v", err)
	}
	return f
}

// code returns the ParseError code, or "" when err is not one. Comparing
// codes rather than messages is what lets web/src/lib/upstreams.ts assert
// the same rejections against the same fixture with its own copy.
func code(err error) string {
	var pe *ParseError
	if errors.As(err, &pe) {
		return pe.Code
	}
	return ""
}

func TestParseUpstreamsAccepts(t *testing.T) {
	for _, c := range loadGrammar(t).Accept {
		t.Run(c.In, func(t *testing.T) {
			got, err := ParseUpstreams(c.In)
			if err != nil {
				t.Fatalf("ParseUpstreams(%q): %v", c.In, err)
			}
			if len(got) != 1 {
				t.Fatalf("ParseUpstreams(%q) returned %d entries, want 1", c.In, len(got))
			}
			u := got[0]
			if string(u.Scheme) != c.Scheme || u.Addr != c.Addr ||
				u.VerifyName != c.VerifyName || u.Path != c.Path || u.Canonical != c.Canonical {
				t.Errorf("ParseUpstreams(%q) =\n  scheme=%q addr=%q name=%q path=%q canonical=%q\nwant\n  scheme=%q addr=%q name=%q path=%q canonical=%q",
					c.In, u.Scheme, u.Addr, u.VerifyName, u.Path, u.Canonical,
					c.Scheme, c.Addr, c.VerifyName, c.Path, c.Canonical)
			}
		})
	}
}

func TestParseUpstreamsRejects(t *testing.T) {
	f := loadGrammar(t)
	cases := append(append([]struct{ In, Code string }{}, f.Reject...), f.RejectList...)
	for _, c := range cases {
		t.Run(c.In, func(t *testing.T) {
			_, err := ParseUpstreams(c.In)
			if err == nil {
				t.Fatalf("ParseUpstreams(%q) succeeded, want rejection %q", c.In, c.Code)
			}
			if got := code(err); got != c.Code {
				t.Errorf("ParseUpstreams(%q) code = %q, want %q (message: %v)", c.In, got, c.Code, err)
			}
		})
	}
}

func TestParseUpstreamsLists(t *testing.T) {
	for _, c := range loadGrammar(t).AcceptList {
		t.Run(c.In, func(t *testing.T) {
			got, err := ParseUpstreams(c.In)
			if err != nil {
				t.Fatalf("ParseUpstreams(%q): %v", c.In, err)
			}
			if len(got) != c.Count {
				t.Errorf("ParseUpstreams(%q) returned %d entries, want %d", c.In, len(got), c.Count)
			}
		})
	}
}

// Canonical is the identity *up is reused by, so parsing it again has to
// produce the same upstream — otherwise two spellings of one server would
// keep separate health records after a settings round trip.
func TestCanonicalRoundTrips(t *testing.T) {
	for _, c := range loadGrammar(t).Accept {
		t.Run(c.In, func(t *testing.T) {
			first, err := ParseUpstreams(c.In)
			if err != nil {
				t.Fatalf("ParseUpstreams(%q): %v", c.In, err)
			}
			again, err := ParseUpstreams(strings.Join([]string{first[0].Canonical}, ","))
			if err != nil {
				t.Fatalf("re-parsing %q: %v", first[0].Canonical, err)
			}
			if again[0] != first[0] {
				t.Errorf("round trip changed the upstream:\n first = %+v\n again = %+v", first[0], again[0])
			}
		})
	}
}
```

- [ ] **Step 3: Run it to watch it fail**

Run: `go test ./internal/upstream/ -run TestParseUpstreams -v 2>&1 | head -20`
Expected: FAIL to compile — `undefined: ParseUpstreams`, `undefined: ParseError`.

- [ ] **Step 4: Write the parser**

Create `internal/upstream/addr.go`:

```go
package upstream

import (
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
)

// Scheme is the transport an upstream is reached over.
type Scheme string

const (
	SchemePlain Scheme = "udp"
	SchemeDoT   Scheme = "tls"
	SchemeDoH   Scheme = "https"
)

// Default ports and path, applied when the entry omits them.
const (
	defaultPlainPort = "53"
	defaultDoTPort   = "853"
	defaultDoHPort   = "443"
	defaultDoHPath   = "/dns-query"
)

// Upstream is one parsed entry of the comma-separated `upstreams` setting.
//
// Addr is always a literal host:port. For SchemeDoT and SchemeDoH the host
// is always an IP, so reaching the upstream never requires a DNS lookup —
// that is what makes a DNS server able to use another DNS server over TLS
// without needing DNS first (spec §3). For SchemePlain the host may be a
// name, exactly as it could before this milestone.
type Upstream struct {
	Scheme     Scheme
	Addr       string
	VerifyName string
	Path       string
	Canonical  string
}

// ParseError is a rejected entry, carrying a stable Code beside the human
// message. web/src/lib/upstreams.ts mirrors these codes and renders its own
// copy, so the two parsers can be held to the same fixture without either
// one's wording becoming an interface.
type ParseError struct {
	Entry string
	Code  string
	Msg   string
}

func (e *ParseError) Error() string {
	if e.Entry == "" {
		return e.Msg
	}
	return fmt.Sprintf("upstream %q: %s", e.Entry, e.Msg)
}

func failf(entry, code, format string, args ...any) *ParseError {
	return &ParseError{Entry: entry, Code: code, Msg: fmt.Sprintf(format, args...)}
}

// ParseUpstreams parses the comma-separated `upstreams` setting value.
func ParseUpstreams(s string) ([]Upstream, error) {
	return parseEntries(strings.Split(s, ","))
}

// parseEntries parses already-split entries. upstream.New calls this with
// Config.Upstreams, so a Forwarder built directly in a test takes the same
// grammar as one built from the stored setting.
//
// Empty entries are dropped rather than rejected — that is what the old
// app.parseUpstreams did, and a trailing comma is a typo that costs nothing
// to forgive. A list that is *entirely* empty is a different thing: it
// leaves the server with nowhere to forward, so it is an error.
func parseEntries(entries []string) ([]Upstream, error) {
	out := make([]Upstream, 0, len(entries))
	for _, raw := range entries {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		u, err := parseEntry(raw)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	if len(out) == 0 {
		return nil, &ParseError{Code: "empty", Msg: "no upstreams configured"}
	}
	// Mixing transports would mean some queries are encrypted and some are
	// not, with nothing on screen or in the log saying which — a privacy
	// property that is unpredictable rather than partial. Spec §4 rule 4.
	for _, u := range out[1:] {
		if u.Scheme != out[0].Scheme {
			return nil, failf("", "mixed_schemes",
				"every upstream must use the same transport, but the list mixes %s and %s", out[0].Scheme, u.Scheme)
		}
	}
	return out, nil
}

func parseEntry(raw string) (Upstream, error) {
	if !strings.Contains(raw, "://") {
		return parsePlainEntry(raw, raw)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return Upstream{}, failf(raw, "bad_url", "not a valid URL: %v", err)
	}
	if u.User != nil {
		return Upstream{}, failf(raw, "bad_url", "a username or password has no meaning on a DNS upstream")
	}
	if u.RawQuery != "" {
		return Upstream{}, failf(raw, "bad_url", "a query string has no meaning on a DNS upstream")
	}
	switch Scheme(u.Scheme) {
	case SchemePlain:
		if u.Fragment != "" {
			return Upstream{}, failf(raw, "name_on_plain",
				`"#%s" only applies to tls:// and https:// upstreams, where it names the certificate to check`, u.Fragment)
		}
		if p := strings.Trim(u.Path, "/"); p != "" {
			return Upstream{}, failf(raw, "bad_url", "a path has no meaning on a plain DNS upstream")
		}
		return parsePlainEntry(raw, u.Host)
	case SchemeDoT:
		return parseEncryptedEntry(raw, u, SchemeDoT, defaultDoTPort, "")
	case SchemeDoH:
		return parseEncryptedEntry(raw, u, SchemeDoH, defaultDoHPort, defaultDoHPath)
	default:
		return Upstream{}, failf(raw, "bad_scheme",
			"unknown transport %q: use udp://, tls:// or https:// (or no scheme for plain DNS)", u.Scheme)
	}
}

// parsePlainEntry applies exactly the treatment app.parseUpstreams gave
// every entry before this milestone: split, default the port to 53, bracket
// a bare IPv6 literal so the appended port parses. It validates nothing
// else, on purpose — a plain upstream has always been allowed to be a
// hostname, and tightening that here would reject working configurations.
func parsePlainEntry(raw, hostPort string) (Upstream, error) {
	if strings.Contains(hostPort, "#") {
		name := hostPort[strings.Index(hostPort, "#")+1:]
		return Upstream{}, failf(raw, "name_on_plain",
			`"#%s" only applies to tls:// and https:// upstreams, where it names the certificate to check`, name)
	}
	addr := hostPort
	if _, _, err := net.SplitHostPort(addr); err != nil {
		if strings.Contains(addr, ":") && !strings.HasPrefix(addr, "[") {
			addr = "[" + addr + "]:" + defaultPlainPort
		} else {
			addr += ":" + defaultPlainPort
		}
	}
	return Upstream{Scheme: SchemePlain, Addr: addr, Canonical: addr}, nil
}

// parseEncryptedEntry handles tls:// and https://, which share every rule
// that makes an encrypted upstream different: the host must be an address,
// and the certificate name must be stated.
func parseEncryptedEntry(raw string, u *url.URL, scheme Scheme, defaultPort, defaultPath string) (Upstream, error) {
	host, port := u.Hostname(), u.Port()
	if host == "" {
		return Upstream{}, failf(raw, "bad_addr", "no address")
	}
	// Camp 1 (spec §3): the operator supplies the address. Resolving a name
	// here would need DNS to configure DNS, and every mainstream resolver
	// that takes this seriously — Unbound, systemd-resolved, Stubby, Knot —
	// asks for the address instead.
	if _, err := netip.ParseAddr(host); err != nil {
		return Upstream{}, failf(raw, "host_not_ip",
			"%q is a name, and an encrypted upstream needs an address here: write %s://<address>#%s", host, scheme, host)
	}
	if port == "" {
		port = defaultPort
	} else {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return Upstream{}, failf(raw, "bad_addr", "%q is not a port number", port)
		}
	}
	if u.Fragment == "" {
		return Upstream{}, failf(raw, "missing_name",
			`missing "#name": an encrypted upstream needs the name its certificate must present, e.g. %s#dns.example.net`, raw)
	}
	up := Upstream{
		Scheme:     scheme,
		Addr:       net.JoinHostPort(host, port),
		VerifyName: u.Fragment,
	}
	if defaultPath != "" {
		up.Path = u.Path
		if up.Path == "" || up.Path == "/" {
			up.Path = defaultPath
		}
	}
	up.Canonical = string(scheme) + "://" + up.Addr + up.Path + "#" + up.VerifyName
	return up, nil
}
```

- [ ] **Step 5: Run the tests**

Run: `go test ./internal/upstream/ -run 'TestParseUpstreams|TestCanonical' -v 2>&1 | tail -30`
Expected: PASS, every fixture case named as a subtest.

- [ ] **Step 6: Confirm the fixture is load-bearing, not decorative**

The grammar test is a table read from a file, and the failure mode of such a
test is that it silently exercises nothing. Prove it does:

```bash
python3 - <<'EOF'
import json
p = "internal/upstream/testdata/grammar.json"
d = json.load(open(p))
before = len(d["reject"])
d["reject"] = [c for c in d["reject"] if c["code"] != "host_not_ip"]
assert len(d["reject"]) < before, "MUTATION DID NOT APPLY — no host_not_ip cases found"
json.dump(d, open(p, "w"), indent=2)
print("mutation applied: dropped", before - len(d["reject"]), "host_not_ip cases")
EOF
go test ./internal/upstream/ -run TestParseUpstreamsRejects 2>&1 | tail -3
git checkout internal/upstream/testdata/grammar.json
```

Expected: the mutation prints that it applied, the run still PASSes (fewer
cases), and `git checkout` restores it. This confirms the fixture drives the
subtests. Then the discriminating half — break the parser, not the fixture:

```bash
sed -i 's/if _, err := netip.ParseAddr(host); err != nil {/if false {/' internal/upstream/addr.go
git diff --stat internal/upstream/addr.go   # must show 1 file changed
go test ./internal/upstream/ -run TestParseUpstreamsRejects 2>&1 | tail -5
git checkout internal/upstream/addr.go
```

Expected: `git diff --stat` shows the edit landed, then FAIL naming the two
`host_not_ip` cases. A pass here means the fixture is not reaching the parser.

- [ ] **Step 7: Commit**

```bash
gofmt -l internal && go test ./internal/upstream/ && git add internal/upstream/addr.go internal/upstream/addr_test.go internal/upstream/testdata/grammar.json && git commit -m "feat(upstream): the encrypted-upstream entry grammar"
```

---

### Task 2: One parser, two callers

**Files:**
- Modify: `internal/app/app.go` — delete `parseUpstreams` (lines 290-313), call `upstream.ParseUpstreams`
- Modify: `internal/upstream/forwarder.go` — `New` parses `cfg.Upstreams` through `parseEntries`
- Modify: `internal/api/settings_handlers.go` — validators return `error`
- Test: `internal/api/settings_handlers_test.go`

**Interfaces:**
- Consumes: `upstream.ParseUpstreams`, `parseEntries`, `*upstream.ParseError` (Task 1)
- Produces:
  ```go
  // internal/api/settings_handlers.go
  var editableSettings = map[string]func(string) error{...}
  func oneOf(vals ...string) func(string) error
  func nonNegInt(v string) error
  ```

**The defect being fixed, stated plainly.** The grammar lives in two places that do not agree. `app.parseUpstreams` does the real work; the API's validator for the same key is `strings.TrimSpace(v) != ""` (`settings_handlers.go:11`), which accepts anything non-blank. A malformed entry is therefore accepted at save and silently dropped at the next restart — the operator learns about the typo when DNS stops working, not when they made it. Teaching schemes to both would guarantee drift.

**`Config.Upstreams` stays `[]string`.** `New` calls `parseEntries` on it. Every existing test constructing `upstream.Config{Upstreams: []string{addr}}` keeps compiling and keeps meaning a plain upstream. See the Global Constraints for why this matters more than a tidier type.

- [ ] **Step 1: Write the failing test**

Add to `internal/api/settings_handlers_test.go` (match the file's existing harness — it uses the same `newTestServer`/`ts.do` shape as the other `internal/api` suites; read the top of the file before writing):

```go
// A malformed upstreams value is refused at the moment it is saved. Before
// this, editableSettings["upstreams"] was `TrimSpace(v) != ""`: the value
// was stored, and the entry was silently dropped at the next restart — so
// the typo surfaced as DNS being down, hours later, with nothing pointing
// back at the edit that caused it.
func TestSettingsPutRejectsMalformedUpstreams(t *testing.T) {
	for _, tc := range []struct {
		name, value, wantIn string
	}{
		{"hostname where an address is required", "tls://cloudflare-dns.com:853#cloudflare-dns.com", "is a name"},
		{"encrypted entry with no certificate name", "tls://1.1.1.1:853", `missing "#name"`},
		{"transports mixed", "1.1.1.1:53,tls://9.9.9.9:853#dns.quad9.net", "same transport"},
		{"unknown transport", "quic://1.1.1.1:853#dns.adguard.com", "unknown transport"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts := newTestServer(t)
			rec := ts.do(t, http.MethodPut, "/api/v1/settings",
				`{"key":"upstreams","value":`+strconv.Quote(tc.value)+`}`)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("PUT /settings = %d, want 400", rec.Code)
			}
			body := rec.Body.String()
			// The prefix is what other tests assert on; the reason is what
			// this task adds. Both have to be there.
			if !strings.Contains(body, "invalid value for upstreams") {
				t.Errorf("body %q lost the existing prefix", body)
			}
			if !strings.Contains(body, tc.wantIn) {
				t.Errorf("body %q does not say why; want it to contain %q", body, tc.wantIn)
			}
		})
	}
}

// A well-formed encrypted value still saves — the guard against a validator
// so strict it rejects the feature it exists to enable.
func TestSettingsPutAcceptsEncryptedUpstreams(t *testing.T) {
	ts := newTestServer(t)
	rec := ts.do(t, http.MethodPut, "/api/v1/settings",
		`{"key":"upstreams","value":"tls://1.1.1.1:853#cloudflare-dns.com, tls://1.0.0.1:853#cloudflare-dns.com"}`)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("PUT /settings = %d (%s), want 204", rec.Code, rec.Body.String())
	}
}
```

- [ ] **Step 2: Run it to watch it fail**

Run: `go test ./internal/api/ -run TestSettingsPut -v 2>&1 | tail -20`
Expected: FAIL — the malformed values return 204, because the validator accepts any non-blank string.

- [ ] **Step 3: Change the validator signature**

In `internal/api/settings_handlers.go`:

```go
// A validator returns why a value is refused, so the 400 can say it. It was
// a bool: every rejection read "invalid value for upstreams", which names
// the field and not the problem — unhelpful for a free-text setting with a
// grammar, and the reason the upstreams entry was never given a real check
// at all.
var editableSettings = map[string]func(string) error{
	"upstreams":             validUpstreams,
	"upstream.strategy":     oneOf("failover", "fastest", "race"),
	"blocking.mode":         oneOf("null-ip", "nxdomain"),
	"blocking.ttl":          nonNegInt,
	"cache.min_ttl":         nonNegInt,
	"cache.max_ttl":         nonNegInt,
	"cache.max_entries":     nonNegInt,
	"cache.serve_stale_for": nonNegInt,
	"lists.refresh_hours":   nonNegInt,
	"qlog.retention_days":   nonNegInt,
	"qlog.privacy":          oneOf("full", "anon", "none"),
}

// validUpstreams runs the same parser applySettings runs, so a value that
// saves is a value that will build a forwarder.
func validUpstreams(v string) error {
	_, err := upstream.ParseUpstreams(v)
	return err
}

func oneOf(vals ...string) func(string) error {
	return func(v string) error {
		if slices.Contains(vals, v) {
			return nil
		}
		return fmt.Errorf("must be one of: %s", strings.Join(vals, ", "))
	}
}

func nonNegInt(v string) error {
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 0 {
		return errors.New("must be a whole number, zero or more")
	}
	return nil
}
```

And in `handleSettingsPut`, replacing the `if !validate(body.Value)` block:

```go
	if err := validate(body.Value); err != nil {
		// The prefix stays: it is what the existing suite and the web form
		// both key off. The reason is appended, not substituted.
		errJSON(w, http.StatusBadRequest, "invalid value for "+body.Key+": "+err.Error())
		return
	}
```

Add `errors`, `fmt`, `slices` and `github.com/aloks98/dnsaur/internal/upstream` to the imports. `internal/upstream` imports `internal/dnssrv` and `internal/filter` and nothing from `internal/api`, so there is no cycle.

- [ ] **Step 4: Route `New` through the shared parser**

In `internal/upstream/forwarder.go`, replace the upstream-building part of `New`:

```go
	// Parsed here rather than by the caller so a Forwarder built directly in
	// a test takes exactly the grammar a stored setting does. Config.Upstreams
	// stays []string: a bare "127.0.0.1:5353" is a plain upstream, which is
	// what every existing caller means by it.
	ups, err := parseEntries(cfg.Upstreams)
	if err != nil {
		return nil, err
	}
	f := &Forwarder{strategy: cfg.Strategy, timeout: cfg.Timeout, now: time.Now, failCache: map[failKey]time.Time{}}
	for _, u := range ups {
		f.def = append(f.def, newUp(u, cfg.Timeout))
	}
```

Delete the `if len(cfg.Upstreams) == 0` guard at the top of `New` — `parseEntries` now returns the same `"no upstreams configured"` message for that case, and keeping both would be two places to change the wording.

`newUp` still takes an address in this task; it changes signature in Task 3. To keep this task compiling on its own, give it the interim shape:

```go
func newUp(u Upstream, timeout time.Duration) *up {
	return &up{
		addr: u.Canonical,
		udp:  &dns.Client{Net: "udp", Timeout: timeout},
		tcp:  &dns.Client{Net: "tcp", Timeout: timeout},
	}
}
```

and at the `SetConditional` call site (`forwarder.go:245`), where `a` is a plain address string from a zone's `forward_to`:

```go
				u = newUp(Upstream{Scheme: SchemePlain, Addr: a, Canonical: a}, f.timeout)
```

Zone forwarding stays plaintext (spec §9) — those addresses point at internal resolvers on trusted networks.

- [ ] **Step 5: Delete `app.parseUpstreams`**

In `internal/app/app.go`, delete `parseUpstreams` (lines 290-313) and change its two call sites in `applySettings`:

```go
	upstreams := strings.Split(a.getSetting(ctx, "upstreams"), ",")
```
```go
			fwd, err = buildForwarder(strings.Split(defaultSettings()["upstreams"], ","), "race")
```

`buildForwarder` keeps its `[]string` signature. Splitting and trimming now happen inside `parseEntries`, so a bad value reaches `buildForwarder` and comes back as an error — which `applySettings`' existing fallback ladder already handles, and which is why the ladder does not need to change. Remove the now-unused `net` import if nothing else in the file uses it (`gofmt`/`golangci-lint` will say).

- [ ] **Step 6: Run everything**

Run: `go test ./internal/... 2>&1 | tail -20`
Expected: PASS throughout. `internal/upstream/forwarder_test.go` and `internal/app`'s suites are **unmodified** — if either needed an edit to pass, stop and report it: something changed that this task was not supposed to change.

- [ ] **Step 7: Prove the validator is the thing rejecting**

```bash
sed -i 's/^func validUpstreams(v string) error {$/func validUpstreams(v string) error {\n\treturn nil \/\/ MUTANT/' internal/api/settings_handlers.go
git diff --stat internal/api/settings_handlers.go   # must show 1 file changed
go test ./internal/api/ -run TestSettingsPutRejectsMalformedUpstreams 2>&1 | tail -5
git checkout internal/api/settings_handlers.go
```

Expected: the diff confirms the edit landed, then FAIL on all four cases.

- [ ] **Step 8: Commit**

```bash
gofmt -l internal && go test ./internal/... > /dev/null && git add -A internal && git commit -m "refactor(upstream): one upstream parser, and the API validates with it"
```

---

### Task 3: The transport seam

**Files:**
- Create: `internal/upstream/exchanger.go`
- Modify: `internal/upstream/forwarder.go` — `up`, `newUp`, `Forwarder.exchange`, `SetConditional`, new `Forwarder.Close`
- Modify: `internal/app/app.go` — `swappable.set` returns the displaced forwarder; `applySettings` and `Shutdown` close
- Test: `internal/upstream/forwarder_test.go` **unchanged**; new `internal/upstream/exchanger_test.go`

**Interfaces:**
- Consumes: `Upstream`, `SchemePlain` (Task 1)
- Produces:
  ```go
  // internal/upstream/exchanger.go
  type exchanger interface {
      Exchange(ctx context.Context, m *dns.Msg) (*dns.Msg, error)
      Close() error
  }
  func newPlainExchanger(addr string, timeout time.Duration) *plainExchanger

  // internal/upstream/forwarder.go
  func (f *Forwarder) Close() error

  // internal/app/app.go
  func (s *swappable) set(f *upstream.Forwarder) *upstream.Forwarder // returns the displaced one, or nil
  ```

**The one permitted edit to `forwarder_test.go` happens in this task.** `newUp` changes signature here, and line 309's `newUp("127.0.0.1:1", 100*time.Millisecond)` becomes `newUp(Upstream{Scheme: SchemePlain, Addr: "127.0.0.1:1", Canonical: "127.0.0.1:1"}, 100*time.Millisecond)`. `TestMarkResultPenalizesFailingUpstreamEwma` is a pure `markResult` unit test that never touches a transport, so this is a constructor call being updated and nothing else. **No assertion in that file may change**, and any other edit to it is a defect. (Task 2 deliberately left `newUp(addr string, ...)` alone for this reason; the signature change lands here, where the seam needs the richer parameter.)

**This is otherwise a pure move plus a lifecycle addition, and `forwarder_test.go` is its test.** The 0x20 scramble, the UDP-then-TCP-on-truncation exchange, the case verification and the case restoration move from `Forwarder.exchange` into `plainExchanger.Exchange` unchanged. The existing suite passing without edits is the proof — there is no new test for the move itself, and writing one would be asserting the same behaviour a second time.

**What stays in `Forwarder.exchange`:** the clock, `markResult`, and the message copy. Those need `f.now` and `f.timeout`, which are the Forwarder's, not the transport's.

- [ ] **Step 1: Record the baseline**

Run: `go test ./internal/upstream/ -v 2>&1 | tail -20`
Expected: PASS. Record the count — this is the number the move must not move.

- [ ] **Step 2: Write the exchanger and move the plain path into it**

Create `internal/upstream/exchanger.go`:

```go
package upstream

import (
	"context"
	"fmt"
	"math/rand/v2"
	"strings"

	"github.com/miekg/dns"
)

// exchanger sends one query to one upstream and returns one reply.
//
// Implementations own their transport completely: dialing, connection
// reuse, TLS, and any rewriting the transport requires — 0x20 case
// randomization on plaintext, RFC 8467 padding on the encrypted ones,
// DNS-over-HTTPS's zero message ID. Above this line the Forwarder measures
// latency, counts failures and picks upstreams, and none of that knows or
// cares which transport it got.
type exchanger interface {
	Exchange(ctx context.Context, m *dns.Msg) (*dns.Msg, error)
	// Close releases any connections held. Called when a Forwarder is
	// replaced or shut down; safe to call more than once.
	Close() error
}

// plainExchanger is DNS over UDP with a TCP retry on truncation — the only
// transport dnsaur had before this milestone, moved here unchanged.
type plainExchanger struct {
	addr     string
	udp, tcp *dns.Client
}

func newPlainExchanger(addr string, timeout time.Duration) *plainExchanger {
	return &plainExchanger{
		addr: addr,
		udp:  &dns.Client{Net: "udp", Timeout: timeout},
		tcp:  &dns.Client{Net: "tcp", Timeout: timeout},
	}
}

// Exchange sends m with 0x20 case randomization and a TCP retry on
// truncation.
//
// 0x20 raises the difficulty of off-path cache poisoning, which is a threat
// specific to unauthenticated plaintext UDP. It is deliberately absent from
// the encrypted exchangers: inside a verified TLS channel there is no
// off-path attacker to defend against, and resolvers that normalize case
// would turn the check into failures against an upstream that is working.
func (e *plainExchanger) Exchange(ctx context.Context, m *dns.Msg) (*dns.Msg, error) {
	orig := m.Question[0].Name
	rnd := rand.New(rand.NewPCG(rand.Uint64(), rand.Uint64()))
	m.Question[0].Name = scramble(strings.ToLower(orig), rnd)
	r, _, err := e.udp.ExchangeContext(ctx, m, e.addr)
	if err == nil && r.Truncated {
		r, _, err = e.tcp.ExchangeContext(ctx, m, e.addr)
	}
	if err != nil {
		return nil, err
	}
	if r == nil {
		return nil, fmt.Errorf("upstream %s: no reply", e.addr)
	}
	if len(r.Question) != 1 || r.Question[0].Name != m.Question[0].Name {
		return nil, fmt.Errorf("upstream %s: 0x20 case check failed", e.addr)
	}
	// Restore the original case everywhere it echoes.
	r.Question[0].Name = orig
	for _, sec := range [][]dns.RR{r.Answer, r.Ns, r.Extra} {
		for _, rr := range sec {
			if strings.EqualFold(rr.Header().Name, orig) {
				rr.Header().Name = orig
			}
		}
	}
	return r, nil
}

// Close is a no-op: dns.Client dials per exchange and holds nothing.
func (e *plainExchanger) Close() error { return nil }
```

Add `"time"` to the import block.

- [ ] **Step 3: Reshape `up` and `Forwarder.exchange`**

In `internal/upstream/forwarder.go`:

```go
type up struct {
	addr      string // Upstream.Canonical — identity, display, reuse key
	ex        exchanger
	ewmaMicro atomic.Int64
	fails     atomic.Int32
	downUntil atomic.Int64 // unix nano
}

func newUp(u Upstream, timeout time.Duration) *up {
	return &up{addr: u.Canonical, ex: newPlainExchanger(u.Addr, timeout)}
}

// exchange sends m to u and records the outcome.
//
// The copy is made here because the exchanger is free to mutate what it is
// given — plainExchanger scrambles the question's case, the encrypted ones
// add padding and rewrite the ID — and req.Msg belongs to the caller, who
// may still be racing this exchange against another upstream.
func (f *Forwarder) exchange(ctx context.Context, m *dns.Msg, u *up) (*dns.Msg, error) {
	start := f.now()
	r, err := u.ex.Exchange(ctx, m.Copy())
	ok := err == nil && r != nil
	f.markLatency(u, ok && r.Rcode != dns.RcodeServerFailure, start)
	if !ok {
		return nil, err
	}
	return r, nil
}

func (f *Forwarder) markLatency(u *up, ok bool, start time.Time) {
	now := f.now()
	u.markResult(ok, now.Sub(start), now, f.timeout.Microseconds())
}
```

`markLatency` exists so `f.now()` is read once per outcome instead of three times; today's `exchange` calls it three times and can attribute a latency measured against one clock reading to a decision made against another. Behaviour is otherwise identical.

Move `scramble` from `forwarder.go` to `exchanger.go` — it is now only used there.

- [ ] **Step 4: Add `Forwarder.Close` and close orphans in `SetConditional`**

Append to `internal/upstream/forwarder.go`:

```go
// Close releases every upstream's transport. A Forwarder is not usable
// afterwards.
//
// This exists because App.applySettings replaces the Forwarder on every
// settings write and drops the old one. That was free while every transport
// was a dns.Client holding nothing; with pooled TLS connections it is a
// file-descriptor leak per save.
func (f *Forwarder) Close() error {
	f.cmu.Lock()
	defer f.cmu.Unlock()
	seen := map[*up]bool{}
	var first error
	closeAll := func(ups []*up) {
		for _, u := range ups {
			if seen[u] {
				continue // one *up can be named under several suffixes
			}
			seen[u] = true
			if err := u.ex.Close(); err != nil && first == nil {
				first = err
			}
		}
	}
	closeAll(f.def)
	if t := f.cond.Load(); t != nil {
		for _, ups := range t.routes {
			closeAll(ups)
		}
	}
	return first
}
```

And inside `SetConditional`, after the new table is built and before `f.cond.Store(t)` — plus on the `len(routes) == 0` early return:

```go
	// Upstreams the outgoing table had and the new one does not are orphans:
	// nothing will route to them again, and each may be holding pooled TLS
	// connections. Reuse-by-address above means the ones that survive keep
	// their pool along with their EWMA and backoff, which is the point —
	// a zone edit must not cost every upstream its connections.
	closeOrphans := func(old *condTable, kept map[*up]bool) {
		if old == nil {
			return
		}
		for _, ups := range old.routes {
			for _, u := range ups {
				if !kept[u] {
					_ = u.ex.Close()
				}
			}
		}
	}
```

Called as `closeOrphans(f.cond.Load(), nil)` before `f.cond.Store(nil)` in the empty-routes branch, and as `closeOrphans(old, kept)` in the main branch, where `kept` is built while filling `t.routes`:

```go
	kept := map[*up]bool{}
	// ... inside the per-suffix loop, after `ups = append(ups, u)`:
	kept[u] = true
```

Note `old` must be captured once at the top (it already is, as the `byAddr` seed) rather than re-loaded — re-loading after the store would return the new table.

- [ ] **Step 5: Close the displaced forwarder in the app**

In `internal/app/app.go`:

```go
// set installs f and returns the Forwarder it displaced, or nil if this is
// the first. The caller closes it — outside the lock, because Close walks
// every upstream and the documented order is routeMu -> swappable.mu (see
// App.routeMu): doing it here would put a slow, transport-touching call
// inside the lock every query path reads through.
func (s *swappable) set(f *upstream.Forwarder) *upstream.Forwarder {
	s.mu.Lock()
	old := s.f
	s.f = f
	s.h = f.Handler()
	s.has = true
	s.mu.Unlock()
	return old
}
```

and at the end of `applySettings`:

```go
	a.routeMu.Lock()
	a.installConditional(fwd)
	old := a.fwd.set(fwd)
	a.routeMu.Unlock()
	if old != nil && old != fwd {
		// After the swap and outside routeMu: the new forwarder is already
		// serving, so nothing waits on this, and no query can still be
		// holding one of these connections.
		_ = old.Close()
	}
```

and in `Shutdown`, before `return a.st.Close()`:

```go
	if f := a.fwd.forwarder(); f != nil {
		_ = f.Close()
	}
```

- [ ] **Step 6: Write the lifecycle test**

Create `internal/upstream/exchanger_test.go`:

```go
package upstream

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
)

// countingExchanger records what it was asked and whether it was closed.
type countingExchanger struct {
	closed atomic.Int32
}

func (c *countingExchanger) Exchange(context.Context, *dns.Msg) (*dns.Msg, error) {
	return nil, context.Canceled
}
func (c *countingExchanger) Close() error { c.closed.Add(1); return nil }

// Close reaches every upstream, defaults and conditional routes alike, and
// an upstream named under two suffixes is closed once rather than twice.
func TestForwarderCloseReleasesEveryUpstream(t *testing.T) {
	f, err := New(Config{Upstreams: []string{"127.0.0.1:5301"}, Strategy: "race"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := f.SetConditional(map[string][]string{
		"a.example": {"127.0.0.1:5302"},
		"b.example": {"127.0.0.1:5302"}, // same address: one *up, two suffixes
	}); err != nil {
		t.Fatalf("SetConditional: %v", err)
	}
	counters := map[*up]*countingExchanger{}
	for _, u := range f.def {
		c := &countingExchanger{}
		u.ex, counters[u] = c, c
	}
	for _, ups := range f.condTableLoad().routes {
		for _, u := range ups {
			if _, seen := counters[u]; seen {
				continue
			}
			c := &countingExchanger{}
			u.ex, counters[u] = c, c
		}
	}
	if len(counters) != 2 {
		t.Fatalf("expected 2 distinct upstreams (one default, one shared by both suffixes), got %d", len(counters))
	}
	if err := f.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	for u, c := range counters {
		if got := c.closed.Load(); got != 1 {
			t.Errorf("upstream %s closed %d times, want exactly 1", u.addr, got)
		}
	}
}

// A suffix losing its upstream closes it: nothing will route there again,
// and it may be holding pooled connections.
func TestSetConditionalClosesOrphans(t *testing.T) {
	f, err := New(Config{Upstreams: []string{"127.0.0.1:5301"}, Strategy: "race"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := f.SetConditional(map[string][]string{"a.example": {"127.0.0.1:5302"}}); err != nil {
		t.Fatalf("SetConditional: %v", err)
	}
	var orphan *up
	for _, ups := range f.condTableLoad().routes {
		orphan = ups[0]
	}
	c := &countingExchanger{}
	orphan.ex = c

	// The suffix now routes somewhere else, so the old upstream is orphaned.
	if err := f.SetConditional(map[string][]string{"a.example": {"127.0.0.1:5303"}}); err != nil {
		t.Fatalf("SetConditional: %v", err)
	}
	if got := c.closed.Load(); got != 1 {
		t.Errorf("orphaned upstream closed %d times, want 1", got)
	}

	// And a surviving upstream is NOT closed — losing its pool on every
	// unrelated zone edit is the failure this reuse exists to prevent.
	var kept *up
	for _, ups := range f.condTableLoad().routes {
		kept = ups[0]
	}
	k := &countingExchanger{}
	kept.ex = k
	if err := f.SetConditional(map[string][]string{
		"a.example": {"127.0.0.1:5303"},
		"c.example": {"127.0.0.1:5304"},
	}); err != nil {
		t.Fatalf("SetConditional: %v", err)
	}
	if got := k.closed.Load(); got != 0 {
		t.Errorf("surviving upstream was closed %d times, want 0", got)
	}
	_ = time.Second
}
```

Drop the trailing `_ = time.Second` and the `time` import if the file does not otherwise need them.

- [ ] **Step 7: Run everything**

Run: `go test -race ./internal/upstream/ ./internal/app/ -v 2>&1 | tail -30`
Expected: PASS, with `forwarder_test.go`'s count matching Step 1's baseline exactly.

- [ ] **Step 8: Prove the orphan close is real**

```bash
sed -i 's/^\t\t\t\tif !kept\[u\] {$/\t\t\t\tif false {/' internal/upstream/forwarder.go
git diff --stat internal/upstream/forwarder.go   # must show 1 file changed
go test ./internal/upstream/ -run TestSetConditionalClosesOrphans 2>&1 | tail -5
git checkout internal/upstream/forwarder.go
```

Expected: the diff confirms the edit, then FAIL with "orphaned upstream closed 0 times". If the anchor did not match (indentation differs), find the real line and mutate that — a mutation that did not apply reports a false green.

- [ ] **Step 9: Commit**

```bash
gofmt -l internal && go test -race ./internal/... > /dev/null && git add -A internal && git commit -m "refactor(upstream): put a transport seam under the forwarder"
```

---

### Task 4: Padding

**Files:**
- Create: `internal/upstream/padding.go`
- Create: `internal/upstream/padding_test.go`

**Interfaces:**
- Produces:
  ```go
  const paddingBlock = 128
  func padQuery(m *dns.Msg, block int) error
  func stripPadding(m *dns.Msg)
  ```

**Why this exists.** TLS hides the query name but not the message *length*, and DNS message length tracks name length closely enough to narrow the candidates. RFC 8467 closes the channel by rounding every query up to a block boundary, using the EDNS(0) Padding option of RFC 7830 (code 12, `dns.EDNS0_PADDING`).

**Why the reply's padding is removed.** A compliant upstream pads its responses too. That padding means something on the encrypted hop and nothing after it: left in place it would be stored by `internal/cache` and then sent to the client over plaintext UDP, inflating every response for no benefit and pushing some past the client's advertised buffer into needless truncation.

**The OPT record itself is kept** even when Padding was its only option — it also carries the DO bit, the extended rcode bits and the advertised UDP size, and discarding those to save eleven bytes is a bad trade.

- [ ] **Step 1: Write the failing test**

Create `internal/upstream/padding_test.go`:

```go
package upstream

import (
	"strings"
	"testing"

	"github.com/miekg/dns"
)

func query(name string) *dns.Msg {
	m := new(dns.Msg)
	m.SetQuestion(dns.Fqdn(name), dns.TypeA)
	return m
}

// Every padded query packs to a multiple of the block size, whatever its
// name length — which is the property that makes the length stop carrying
// information about the name.
func TestPadQueryRoundsUpToBlock(t *testing.T) {
	for _, name := range []string{
		"a.com",
		"example.com",
		"a-somewhat-longer-label.example.com",
		strings.Repeat("x", 60) + ".example.com",
		strings.Repeat("y", 60) + "." + strings.Repeat("z", 60) + ".example.com",
	} {
		t.Run(name, func(t *testing.T) {
			m := query(name)
			if err := padQuery(m, paddingBlock); err != nil {
				t.Fatalf("padQuery: %v", err)
			}
			wire, err := m.Pack()
			if err != nil {
				t.Fatalf("Pack: %v", err)
			}
			if len(wire)%paddingBlock != 0 {
				t.Errorf("packed length %d is not a multiple of %d", len(wire), paddingBlock)
			}
		})
	}
}

// Two queries of very different name lengths pad to the same size, which is
// the point restated as the thing an observer actually sees.
func TestPadQueryHidesNameLength(t *testing.T) {
	short, long := query("a.com"), query("a.com")
	long.Question[0].Name = dns.Fqdn(strings.Repeat("w", 40) + ".example.com")
	for _, m := range []*dns.Msg{short, long} {
		if err := padQuery(m, paddingBlock); err != nil {
			t.Fatalf("padQuery: %v", err)
		}
	}
	a, _ := short.Pack()
	b, _ := long.Pack()
	if len(a) != len(b) {
		t.Errorf("padded lengths differ: %d vs %d — the name length still shows", len(a), len(b))
	}
}

// Padding twice is padding once: the exchangers call it on a message they
// may then retry on a fresh connection.
func TestPadQueryIsIdempotent(t *testing.T) {
	m := query("example.com")
	if err := padQuery(m, paddingBlock); err != nil {
		t.Fatalf("first padQuery: %v", err)
	}
	first, _ := m.Pack()
	if err := padQuery(m, paddingBlock); err != nil {
		t.Fatalf("second padQuery: %v", err)
	}
	again, _ := m.Pack()
	if len(first) != len(again) {
		t.Errorf("padding twice changed the size: %d then %d", len(first), len(again))
	}
	var pads int
	for _, o := range m.IsEdns0().Option {
		if _, ok := o.(*dns.EDNS0_PADDING); ok {
			pads++
		}
	}
	if pads != 1 {
		t.Errorf("message carries %d padding options, want 1", pads)
	}
}

// A query with no OPT record gets one, because padding has nowhere else to
// live.
func TestPadQueryAddsOPTWhenAbsent(t *testing.T) {
	m := query("example.com")
	if m.IsEdns0() != nil {
		t.Fatal("fixture already has an OPT record; the test proves nothing")
	}
	if err := padQuery(m, paddingBlock); err != nil {
		t.Fatalf("padQuery: %v", err)
	}
	if m.IsEdns0() == nil {
		t.Error("no OPT record after padding")
	}
}

// The reply's padding is removed, and the OPT record survives with the rest
// of what it carries.
func TestStripPaddingKeepsTheOPT(t *testing.T) {
	m := query("example.com")
	m.SetEdns0(1232, true) // DO set: this is what must not be lost
	opt := m.IsEdns0()
	opt.Option = append(opt.Option,
		&dns.EDNS0_PADDING{Padding: make([]byte, 40)},
		&dns.EDNS0_NSID{Nsid: "abc"},
	)

	stripPadding(m)

	got := m.IsEdns0()
	if got == nil {
		t.Fatal("the OPT record was dropped; the DO bit and UDP size went with it")
	}
	if !got.Do() {
		t.Error("the DO bit was lost")
	}
	for _, o := range got.Option {
		if _, ok := o.(*dns.EDNS0_PADDING); ok {
			t.Error("padding survived the strip")
		}
	}
	if len(got.Option) != 1 {
		t.Errorf("the OPT holds %d options, want 1 (NSID kept, padding removed)", len(got.Option))
	}
}

// Padding-only OPT: still kept, still emptied.
func TestStripPaddingKeepsAnEmptiedOPT(t *testing.T) {
	m := query("example.com")
	m.SetEdns0(1232, false)
	opt := m.IsEdns0()
	opt.Option = append(opt.Option, &dns.EDNS0_PADDING{Padding: make([]byte, 40)})

	stripPadding(m)

	if m.IsEdns0() == nil {
		t.Fatal("the OPT record was dropped when padding was its only option")
	}
	if n := len(m.IsEdns0().Option); n != 0 {
		t.Errorf("the OPT holds %d options, want 0", n)
	}
}

// A message with no OPT at all is left alone rather than panicking.
func TestStripPaddingToleratesNoOPT(t *testing.T) {
	m := query("example.com")
	stripPadding(m)
	if m.IsEdns0() != nil {
		t.Error("stripPadding invented an OPT record")
	}
}
```

- [ ] **Step 2: Run it to watch it fail**

Run: `go test ./internal/upstream/ -run 'TestPad|TestStrip' -v 2>&1 | head -20`
Expected: FAIL to compile — `undefined: padQuery`, `undefined: stripPadding`, `undefined: paddingBlock`.

- [ ] **Step 3: Write the implementation**

Create `internal/upstream/padding.go`:

```go
package upstream

import "github.com/miekg/dns"

// paddingBlock is RFC 8467 §4.1's recommended client block size: every
// query is rounded up to a multiple of this, so its length says nothing
// about the name inside it.
const paddingBlock = 128

// padQuery pads m to a multiple of block octets using the EDNS(0) Padding
// option (RFC 7830, code 12).
//
// Only the encrypted exchangers call it. Padding a plaintext query hides
// nothing — the name is right there — and only wastes bytes.
//
// Idempotent: an existing padding option is reused and re-sized rather than
// added to, so a query padded before a failed attempt and retried on a
// fresh connection is padded once.
func padQuery(m *dns.Msg, block int) error {
	opt := m.IsEdns0()
	if opt == nil {
		// Padding has nowhere to live without an OPT record. 1232 is the
		// advertised UDP size used everywhere else in dnsaur; over a stream
		// transport it is inert, and the reply comes back over the same
		// stream regardless.
		m.SetEdns0(1232, false)
		opt = m.IsEdns0()
	}
	var pad *dns.EDNS0_PADDING
	for _, o := range opt.Option {
		if p, ok := o.(*dns.EDNS0_PADDING); ok {
			pad = p
			break
		}
	}
	if pad == nil {
		pad = &dns.EDNS0_PADDING{}
		opt.Option = append(opt.Option, pad)
	}
	// Measured with the option present but empty, so the four bytes of
	// option header are already counted and the padding added below is the
	// only thing that still has to fit.
	pad.Padding = nil
	wire, err := m.Pack()
	if err != nil {
		return err
	}
	if n := (block - len(wire)%block) % block; n > 0 {
		pad.Padding = make([]byte, n)
	}
	return nil
}

// stripPadding removes the Padding option from m's OPT record.
//
// The OPT record itself is kept even when padding was its only option: it
// also carries the DO bit, the extended rcode bits and the advertised UDP
// size, and dropping it to save eleven bytes would discard those. Called on
// every encrypted reply before it is returned, so neither the cache nor the
// client ever sees padding that meant something only on the TLS hop.
func stripPadding(m *dns.Msg) {
	opt := m.IsEdns0()
	if opt == nil {
		return
	}
	kept := opt.Option[:0]
	for _, o := range opt.Option {
		if _, isPad := o.(*dns.EDNS0_PADDING); isPad {
			continue
		}
		kept = append(kept, o)
	}
	opt.Option = kept
}
```

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/upstream/ -run 'TestPad|TestStrip' -v 2>&1 | tail -20`
Expected: PASS, all seven.

- [ ] **Step 5: Prove the block arithmetic is load-bearing**

```bash
sed -i 's/if n := (block - len(wire)%block) % block; n > 0 {/if n := 0; n > 0 {/' internal/upstream/padding.go
git diff --stat internal/upstream/padding.go   # must show 1 file changed
go test ./internal/upstream/ -run 'TestPadQueryRoundsUp|TestPadQueryHides' 2>&1 | tail -6
git checkout internal/upstream/padding.go
```

Expected: the diff confirms the edit, then FAIL on both — lengths not a multiple of 128, and short/long differing. A pass would mean the fixture names happen to land on a boundary already, in which case add a name that does not.

- [ ] **Step 6: Commit**

```bash
gofmt -l internal && go test ./internal/upstream/ > /dev/null && git add internal/upstream/padding.go internal/upstream/padding_test.go && git commit -m "feat(upstream): RFC 8467 query padding and reply de-padding"
```

---

### Task 5: DNS-over-TLS, and the connection pool

**Files:**
- Create: `internal/upstream/dot.go`
- Create: `internal/upstream/tlstest_test.go` — the certificate and the local DoT server
- Create: `internal/upstream/dot_test.go`

**Interfaces:**
- Consumes: `Upstream`, `SchemeDoT` (Task 1); `exchanger` (Task 3); `padQuery`, `stripPadding`, `paddingBlock` (Task 4)
- Produces:
  ```go
  const (
      idleConnTimeout = 30 * time.Second
      maxIdleConns    = 4
  )
  func newDoTExchanger(u Upstream, timeout time.Duration, roots *x509.CertPool) *dotExchanger

  // internal/upstream/tlstest_test.go — used again by Tasks 6 and 7
  func testCertFor(t *testing.T, name string) (tls.Certificate, *x509.CertPool)
  func startDoT(t *testing.T, cert tls.Certificate, h dns.HandlerFunc) (addr string, ln *recordingListener)
  // answerA is NOT defined here — forwarder_test.go already has it.
  type recordingListener struct{ ... }  // .count() int, .closeAll()
  ```

**`roots *x509.CertPool`, not `*tls.Config`.** Tests need to trust a self-signed certificate; nothing needs to *skip* verification. Taking only the root pool means there is no production code path that can set `InsecureSkipVerify`, which is the property worth protecting — a transport whose verification can be turned off is a transport whose guarantee cannot be relied on.

**Why the pool is mandatory rather than an optimization.** `dns.Client.ExchangeContext` dials per call. Without a pool every single query pays a TCP handshake plus a TLS handshake — two extra round trips on a hop that should cost one. RFC 7858 §3.4 exists for exactly this.

**The stale-connection retry, restated because it is the defect a hand-rolled pool always ships with.** A DoT server may close an idle connection whenever it likes; that is routine housekeeping, not a fault. If dnsaur borrows such a connection and the read fails, treating it as a query failure would surface the server's housekeeping as intermittent SERVFAILs *and* as `markResult` failures that eventually mark a perfectly healthy upstream down. So a failure on a **pooled** connection gets exactly one retry on a **freshly dialed** one; a failure on a connection that was already fresh is real and is reported. `http.Transport` does this internally, which is why the DoH path in Task 6 needs no equivalent code.

- [ ] **Step 1: Write the test fixtures**

Create `internal/upstream/tlstest_test.go`:

```go
package upstream

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	crand "crypto/rand" // crand: package upstream already binds `rand` to math/rand/v2
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/miekg/dns"
)

// testCertFor mints a self-signed certificate valid for name, and the pool
// that trusts it. Returned separately so a test can hand the pool to one
// exchanger and deliberately not to another.
func testCertFor(t *testing.T, name string) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), crand.Reader)
	if err != nil {
		t.Fatalf("generating a key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: name},
		DNSNames:              []string{name},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(crand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("creating the certificate: %v", err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parsing the certificate back: %v", err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, pool
}

// recordingListener counts accepted connections and keeps them, which is
// how the pooling tests tell connection reuse from re-dialing, and how the
// stale-connection test closes the server's side deterministically instead
// of waiting for an idle timeout.
type recordingListener struct {
	net.Listener
	mu      sync.Mutex
	accepts int
	conns   []net.Conn
}

func (l *recordingListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	l.mu.Lock()
	l.accepts++
	l.conns = append(l.conns, c)
	l.mu.Unlock()
	return c, nil
}

func (l *recordingListener) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.accepts
}

// closeAll shuts the server's end of every connection accepted so far —
// what a real DoT server does to an idle connection, on demand.
func (l *recordingListener) closeAll() {
	l.mu.Lock()
	conns := l.conns
	l.conns = nil
	l.mu.Unlock()
	for _, c := range conns {
		_ = c.Close()
	}
}

// startDoT runs a DNS-over-TLS server on 127.0.0.1 and returns its address
// and the listener, so a test can count accepts and close connections.
func startDoT(t *testing.T, cert tls.Certificate, h dns.HandlerFunc) (string, *recordingListener) {
	t.Helper()
	base, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	})
	if err != nil {
		t.Fatalf("listening: %v", err)
	}
	ln := &recordingListener{Listener: base}
	// Net is not set: ActivateAndServe uses the Listener it is given, and
	// this one already speaks TLS.
	srv := &dns.Server{Listener: ln, Handler: h}
	go func() { _ = srv.ActivateAndServe() }()
	t.Cleanup(func() { _ = srv.Shutdown() })
	return base.Addr().String(), ln
}

// answerA replies to any question with one A record, so a test can tell a
// real answer from a synthesized failure.
```

**Do NOT define `answerA` here.** `internal/upstream/forwarder_test.go:98` already has one, used by roughly twenty call sites in that file, and this is the same package — a second definition does not compile. The DoT tests use the existing helper as-is. (An earlier draft of this plan defined a second `answerA` with a different TTL; resolving the clash by deleting the original silently re-fixtured every existing forwarder test, which is exactly what the "no assertion may change" constraint exists to prevent.)

- [ ] **Step 2: Write the failing tests**

Create `internal/upstream/dot_test.go`:

```go
package upstream

import (
	"context"
	"testing"
	"time"

	"github.com/miekg/dns"
)

const dotName = "dot.test"

func dotUpstream(addr string) Upstream {
	return Upstream{Scheme: SchemeDoT, Addr: addr, VerifyName: dotName,
		Canonical: "tls://" + addr + "#" + dotName}
}

func TestDoTExchange(t *testing.T) {
	cert, pool := testCertFor(t, dotName)
	addr, _ := startDoT(t, cert, answerA("10.0.0.1"))
	e := newDoTExchanger(dotUpstream(addr), 5*time.Second, pool)
	t.Cleanup(func() { _ = e.Close() })

	r, err := e.Exchange(context.Background(), query("example.com"))
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if len(r.Answer) != 1 {
		t.Fatalf("got %d answers, want 1", len(r.Answer))
	}
	if a, ok := r.Answer[0].(*dns.A); !ok || a.A.String() != "10.0.0.1" {
		t.Errorf("answer = %v, want 10.0.0.1", r.Answer[0])
	}
}

// Several queries share one connection. Asserting only that the queries
// succeeded would pass with no pool at all — the accept count is the whole
// test.
func TestDoTReusesConnections(t *testing.T) {
	cert, pool := testCertFor(t, dotName)
	addr, ln := startDoT(t, cert, answerA("10.0.0.1"))
	e := newDoTExchanger(dotUpstream(addr), 5*time.Second, pool)
	t.Cleanup(func() { _ = e.Close() })

	for i := range 5 {
		if _, err := e.Exchange(context.Background(), query("example.com")); err != nil {
			t.Fatalf("query %d: %v", i, err)
		}
	}
	if got := ln.count(); got != 1 {
		t.Errorf("server accepted %d connections for 5 queries, want 1 — the pool is not being used", got)
	}
}

// The server closing an idle connection is routine (RFC 7858 §3.4), not a
// query failure. Without the one-shot retry on a fresh connection this is
// an intermittent SERVFAIL that also drives a healthy upstream toward being
// marked down.
func TestDoTRetriesOnceOnAStaleConnection(t *testing.T) {
	cert, pool := testCertFor(t, dotName)
	addr, ln := startDoT(t, cert, answerA("10.0.0.1"))
	e := newDoTExchanger(dotUpstream(addr), 5*time.Second, pool)
	t.Cleanup(func() { _ = e.Close() })

	if _, err := e.Exchange(context.Background(), query("example.com")); err != nil {
		t.Fatalf("first query: %v", err)
	}
	// The connection is now in the pool, and the server hangs up on it.
	ln.closeAll()

	r, err := e.Exchange(context.Background(), query("example.com"))
	if err != nil {
		t.Fatalf("query after the server closed the pooled connection: %v", err)
	}
	if len(r.Answer) != 1 {
		t.Errorf("got %d answers, want 1", len(r.Answer))
	}
	if got := ln.count(); got != 2 {
		t.Errorf("server accepted %d connections, want 2 (the first, and the redial)", got)
	}
}

// A connection idle longer than the pool's own deadline is discarded rather
// than handed out.
func TestDoTDiscardsIdleConnections(t *testing.T) {
	cert, pool := testCertFor(t, dotName)
	addr, ln := startDoT(t, cert, answerA("10.0.0.1"))
	e := newDoTExchanger(dotUpstream(addr), 5*time.Second, pool)
	e.idleTimeout = time.Millisecond
	t.Cleanup(func() { _ = e.Close() })

	if _, err := e.Exchange(context.Background(), query("example.com")); err != nil {
		t.Fatalf("first query: %v", err)
	}
	time.Sleep(20 * time.Millisecond)
	if _, err := e.Exchange(context.Background(), query("example.com")); err != nil {
		t.Fatalf("second query: %v", err)
	}
	if got := ln.count(); got != 2 {
		t.Errorf("server accepted %d connections, want 2 — the expired connection was reused", got)
	}
}

// A certificate that does not match the configured name is a failure. There
// is no plaintext retry and no "try anyway" — see
// TestEncryptedUpstreamNeverFallsBackToPlaintext for the other half.
func TestDoTRejectsAMismatchedCertificate(t *testing.T) {
	cert, pool := testCertFor(t, "somewhere.else")
	addr, _ := startDoT(t, cert, answerA("10.0.0.1"))
	e := newDoTExchanger(dotUpstream(addr), 5*time.Second, pool) // expects dot.test
	t.Cleanup(func() { _ = e.Close() })

	if _, err := e.Exchange(context.Background(), query("example.com")); err == nil {
		t.Fatal("Exchange succeeded against a certificate for the wrong name")
	}
}

// The query reaches the server padded, so its length says nothing about the
// name inside it.
func TestDoTPadsWhatItSends(t *testing.T) {
	cert, pool := testCertFor(t, dotName)
	sizes := make(chan int, 4)
	addr, _ := startDoT(t, cert, func(w dns.ResponseWriter, m *dns.Msg) {
		wire, err := m.Pack()
		if err == nil {
			sizes <- len(wire)
		}
		// Reply padded too, so the strip below has something to remove.
		r := new(dns.Msg)
		r.SetReply(m)
		_ = padQuery(r, paddingBlock)
		_ = w.WriteMsg(r)
	})
	e := newDoTExchanger(dotUpstream(addr), 5*time.Second, pool)
	t.Cleanup(func() { _ = e.Close() })

	r, err := e.Exchange(context.Background(), query("a.com"))
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	select {
	case n := <-sizes:
		if n%paddingBlock != 0 {
			t.Errorf("the server received %d bytes, not a multiple of %d", n, paddingBlock)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the server never recorded a query size")
	}
	if opt := r.IsEdns0(); opt != nil {
		for _, o := range opt.Option {
			if _, isPad := o.(*dns.EDNS0_PADDING); isPad {
				t.Error("the reply's padding was passed on; it would be cached and then sent to the client")
			}
		}
	}
}
```

- [ ] **Step 3: Run them to watch them fail**

Run: `go test ./internal/upstream/ -run TestDoT -v 2>&1 | head -20`
Expected: FAIL to compile — `undefined: newDoTExchanger`.

- [ ] **Step 4: Write the DoT exchanger**

Create `internal/upstream/dot.go`:

```go
package upstream

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"sync"
	"time"

	"github.com/miekg/dns"
)

const (
	// idleConnTimeout is how long a pooled connection may sit unused before
	// it is discarded rather than handed out. Checked when a connection is
	// taken, so there is no reaper goroutine: with maxIdleConns connections
	// per upstream and a handful of upstreams, the worst case is a few
	// descriptors held until the next query.
	idleConnTimeout = 30 * time.Second
	// maxIdleConns bounds the pool. Each connection carries one query at a
	// time, so this is also the concurrency ceiling per upstream — ample
	// behind a cache, and the alternative (pipelining) needs message-ID
	// bookkeeping this does not.
	//
	// Both constants are also used by doh.go's http.Transport, which is why
	// neither is named for a transport.
	maxIdleConns = 4
)

// dotExchanger is DNS over TLS (RFC 7858) with a small connection pool.
type dotExchanger struct {
	addr        string
	client      *dns.Client
	idleTimeout time.Duration
	maxIdle     int

	mu     sync.Mutex
	idle   []idleConn
	closed bool
}

type idleConn struct {
	c    *dns.Conn
	last time.Time
}

// newDoTExchanger builds the transport for u.
//
// roots is the certificate pool to verify against; nil means the system
// roots, which is what production passes. It takes a pool rather than a
// *tls.Config on purpose: a test needs to trust a self-signed certificate,
// and nothing needs to skip verification — so there is no code path that
// can turn it off.
func newDoTExchanger(u Upstream, timeout time.Duration, roots *x509.CertPool) *dotExchanger {
	return &dotExchanger{
		addr: u.Addr,
		client: &dns.Client{
			Net:     "tcp-tls",
			Timeout: timeout,
			TLSConfig: &tls.Config{
				// The certificate is checked against the name the operator
				// configured, while the connection goes to the address they
				// configured. That split is the whole reason an entry
				// carries both (spec §3).
				ServerName: u.VerifyName,
				RootCAs:    roots,
				MinVersion: tls.VersionTLS12,
			},
		},
		idleTimeout: idleConnTimeout,
		maxIdle:     maxIdleConns,
	}
}

func (e *dotExchanger) Exchange(ctx context.Context, m *dns.Msg) (*dns.Msg, error) {
	if err := padQuery(m, paddingBlock); err != nil {
		return nil, err
	}
	if c := e.get(); c != nil {
		r, _, err := e.client.ExchangeWithConnContext(ctx, m, c)
		if err == nil {
			e.put(c)
			stripPadding(r)
			return r, nil
		}
		_ = c.Close()
		// A pooled connection failing is expected: a DoT server may close an
		// idle connection at any time (RFC 7858 §3.4). Reporting that as a
		// query failure would turn the server's routine housekeeping into
		// intermittent SERVFAILs, and into markResult failures that
		// eventually mark a healthy upstream down. One fresh attempt below.
		if ctx.Err() != nil {
			// The caller gave up — under the race strategy the losers are
			// cancelled — so this was not the connection's fault and there
			// is nothing to retry for.
			return nil, err
		}
	}
	c, err := e.client.DialContext(ctx, e.addr)
	if err != nil {
		return nil, err
	}
	r, _, err := e.client.ExchangeWithConnContext(ctx, m, c)
	if err != nil {
		// Already fresh: this is a real failure, not a stale connection.
		_ = c.Close()
		return nil, err
	}
	e.put(c)
	stripPadding(r)
	return r, nil
}

// get takes a usable connection from the pool, or nil.
func (e *dotExchanger) get() *dns.Conn {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := time.Now()
	for len(e.idle) > 0 {
		last := e.idle[len(e.idle)-1]
		e.idle = e.idle[:len(e.idle)-1]
		if now.Sub(last.last) > e.idleTimeout {
			_ = last.c.Close()
			continue
		}
		return last.c
	}
	return nil
}

// put returns a connection to the pool, or closes it if the pool is full or
// the exchanger has been closed.
func (e *dotExchanger) put(c *dns.Conn) {
	e.mu.Lock()
	if e.closed || len(e.idle) >= e.maxIdle {
		e.mu.Unlock()
		_ = c.Close()
		return
	}
	e.idle = append(e.idle, idleConn{c: c, last: time.Now()})
	e.mu.Unlock()
}

// Close releases every pooled connection. Safe to call more than once, and
// safe to call while a query is in flight: that query's connection is not
// in the pool, and put closes it rather than pooling it afterwards.
func (e *dotExchanger) Close() error {
	e.mu.Lock()
	conns := e.idle
	e.idle, e.closed = nil, true
	e.mu.Unlock()
	for _, ic := range conns {
		_ = ic.c.Close()
	}
	return nil
}
```

- [ ] **Step 5: Run the tests**

Run: `go test -race ./internal/upstream/ -run TestDoT -v 2>&1 | tail -20`
Expected: PASS, all six.

- [ ] **Step 6: Prove the retry and the pool are both load-bearing**

Two mutations, because they are two different claims:

```bash
# 1. Remove the stale-connection retry.
python3 - <<'EOF'
p = "internal/upstream/dot.go"
s = open(p).read()
old = "\t\t_ = c.Close()\n\t\t// A pooled connection failing is expected"
assert old in s, "MUTATION DID NOT APPLY — anchor not found"
s = s.replace(old, "\t\t_ = c.Close()\n\t\treturn nil, err // MUTANT\n\t\t// A pooled connection failing is expected", 1)
open(p, "w").write(s)
print("mutation applied")
EOF
go test ./internal/upstream/ -run TestDoTRetriesOnceOnAStaleConnection 2>&1 | tail -5
git checkout internal/upstream/dot.go
```
Expected: FAIL — "query after the server closed the pooled connection".

```bash
# 2. Never pool.
sed -i 's/^func (e \*dotExchanger) put(c \*dns.Conn) {$/func (e *dotExchanger) put(c *dns.Conn) {\n\t_ = c.Close(); return \/\/ MUTANT/' internal/upstream/dot.go
git diff --stat internal/upstream/dot.go   # must show 1 file changed
go test ./internal/upstream/ -run TestDoTReusesConnections 2>&1 | tail -5
git checkout internal/upstream/dot.go
```
Expected: FAIL — "server accepted 5 connections for 5 queries".

- [ ] **Step 7: Commit**

```bash
gofmt -l internal && go test -race ./internal/upstream/ > /dev/null && git add internal/upstream/dot.go internal/upstream/dot_test.go internal/upstream/tlstest_test.go && git commit -m "feat(upstream): DNS-over-TLS with a pooled connection"
```

---

### Task 6: DNS-over-HTTPS

**Files:**
- Create: `internal/upstream/doh.go`
- Modify: `internal/upstream/tlstest_test.go` — add the local DoH server
- Create: `internal/upstream/doh_test.go`

**Interfaces:**
- Consumes: `Upstream`, `SchemeDoH` (Task 1); `exchanger` (Task 3); `padQuery`, `stripPadding` (Task 4); `testCertFor` (Task 5)
- Produces:
  ```go
  func newDoHExchanger(u Upstream, timeout time.Duration, roots *x509.CertPool) *dohExchanger

  // internal/upstream/tlstest_test.go
  type dohRecord struct{ ... }  // requests, lastSize, lastID, path, proto
  func startDoH(t *testing.T, cert tls.Certificate, reply func(*dns.Msg) *dns.Msg) (addr string, rec *dohRecord)
  ```

**The inversion that makes this work.** The request URL is built from the **verification name**, and the transport's `DialContext` **ignores the address it is handed** and dials the stored IP:

```
https://cloudflare-dns.com/dns-query   ← URL: Host header and SNI correct for free
DialContext(...) -> 1.1.1.1:443        ← pinned; Go's HTTP client never resolves anything
```

Building the URL from the address instead and overriding `ServerName` would work too, but then the `Host:` header names an IP, and every layer that reads it — the server's virtual hosting, any middlebox — sees something different from what TLS negotiated. This way there is exactly one name in play.

**Connection reuse is `http.Transport`'s**, not ours: `ForceAttemptHTTP2` plus a per-host idle pool. There is deliberately no test asserting it here — it would be testing the standard library, and Task 5's pool test covers the code this project actually wrote.

- [ ] **Step 1: Add the DoH test server**

Append to `internal/upstream/tlstest_test.go` (and add `io`, `net/http`, `net/http/httptest`, `sync/atomic` to its imports):

```go
// dohRecord is what the test DoH server saw, so a test can assert on the
// wire rather than on the exchanger's own account of itself.
type dohRecord struct {
	requests atomic.Int64
	lastSize atomic.Int64
	lastID   atomic.Int64
	path     atomic.Value // string
	proto    atomic.Value // string, e.g. "HTTP/2.0"
	ctype    atomic.Value // string
}

// startDoH runs an RFC 8484 server over TLS on 127.0.0.1 and returns its
// address and a record of what it received.
func startDoH(t *testing.T, cert tls.Certificate, reply func(*dns.Msg) *dns.Msg) (string, *dohRecord) {
	t.Helper()
	rec := &dohRecord{}
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, dns.MaxMsgSize))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		rec.requests.Add(1)
		rec.lastSize.Store(int64(len(body)))
		rec.path.Store(r.URL.Path)
		rec.proto.Store(r.Proto)
		rec.ctype.Store(r.Header.Get("Content-Type"))
		m := new(dns.Msg)
		if err := m.Unpack(body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		rec.lastID.Store(int64(m.Id))
		out, err := reply(m).Pack()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/dns-message")
		_, _ = w.Write(out)
	})
	srv := httptest.NewUnstartedServer(h)
	// Set before StartTLS: httptest only mints its own certificate when
	// TLS.Certificates is empty.
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	srv.EnableHTTP2 = true
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv.Listener.Addr().String(), rec
}
```

- [ ] **Step 2: Write the failing tests**

Create `internal/upstream/doh_test.go`:

```go
package upstream

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/miekg/dns"
)

const dohName = "doh.test"

func dohUpstream(addr string) Upstream {
	return Upstream{Scheme: SchemeDoH, Addr: addr, VerifyName: dohName, Path: "/dns-query",
		Canonical: "https://" + addr + "/dns-query#" + dohName}
}

func echoReply(ip string) func(*dns.Msg) *dns.Msg {
	return func(m *dns.Msg) *dns.Msg {
		r := new(dns.Msg)
		r.SetReply(m)
		if rr, err := dns.NewRR(m.Question[0].Name + " 60 IN A " + ip); err == nil {
			r.Answer = append(r.Answer, rr)
		}
		return r
	}
}

func TestDoHExchange(t *testing.T) {
	cert, pool := testCertFor(t, dohName)
	addr, rec := startDoH(t, cert, echoReply("10.0.0.2"))
	e := newDoHExchanger(dohUpstream(addr), 5*time.Second, pool)
	t.Cleanup(func() { _ = e.Close() })

	r, err := e.Exchange(context.Background(), query("example.com"))
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if len(r.Answer) != 1 {
		t.Fatalf("got %d answers, want 1", len(r.Answer))
	}
	if got := rec.path.Load(); got != "/dns-query" {
		t.Errorf("server saw path %v, want /dns-query", got)
	}
	if got := rec.ctype.Load(); got != "application/dns-message" {
		t.Errorf("server saw Content-Type %v, want application/dns-message", got)
	}
	if got := rec.proto.Load(); got != "HTTP/2.0" {
		t.Errorf("server saw %v, want HTTP/2.0 — ForceAttemptHTTP2 is not taking effect", got)
	}
}

// RFC 8484 §4.1: the ID on the wire is 0, and the caller gets its own ID
// back. Losing the restore breaks nothing visible until something matches
// on it, which is exactly why it is asserted here.
func TestDoHZeroesTheWireIDAndRestoresIt(t *testing.T) {
	cert, pool := testCertFor(t, dohName)
	addr, rec := startDoH(t, cert, echoReply("10.0.0.2"))
	e := newDoHExchanger(dohUpstream(addr), 5*time.Second, pool)
	t.Cleanup(func() { _ = e.Close() })

	m := query("example.com")
	m.Id = 0x1234
	r, err := e.Exchange(context.Background(), m)
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if got := rec.lastID.Load(); got != 0 {
		t.Errorf("the wire carried ID %d, want 0", got)
	}
	if r.Id != 0x1234 {
		t.Errorf("the reply carries ID %#x, want %#x", r.Id, 0x1234)
	}
}

func TestDoHPadsWhatItSendsAndStripsWhatItGets(t *testing.T) {
	cert, pool := testCertFor(t, dohName)
	addr, rec := startDoH(t, cert, func(m *dns.Msg) *dns.Msg {
		r := echoReply("10.0.0.2")(m)
		_ = padQuery(r, paddingBlock) // a compliant server pads its replies
		return r
	})
	e := newDoHExchanger(dohUpstream(addr), 5*time.Second, pool)
	t.Cleanup(func() { _ = e.Close() })

	r, err := e.Exchange(context.Background(), query("a.com"))
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if n := rec.lastSize.Load(); n%paddingBlock != 0 {
		t.Errorf("the server received %d bytes, not a multiple of %d", n, paddingBlock)
	}
	if opt := r.IsEdns0(); opt != nil {
		for _, o := range opt.Option {
			if _, isPad := o.(*dns.EDNS0_PADDING); isPad {
				t.Error("the reply's padding was passed on; it would be cached and then sent to the client")
			}
		}
	}
}

func TestDoHTreatsNon200AsFailure(t *testing.T) {
	cert, pool := testCertFor(t, dohName)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "over quota", http.StatusTooManyRequests)
	}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	srv.EnableHTTP2 = true
	srv.StartTLS()
	t.Cleanup(srv.Close)

	e := newDoHExchanger(dohUpstream(srv.Listener.Addr().String()), 5*time.Second, pool)
	t.Cleanup(func() { _ = e.Close() })

	if _, err := e.Exchange(context.Background(), query("example.com")); err == nil {
		t.Fatal("Exchange succeeded on a 429")
	}
}

func TestDoHRejectsAMismatchedCertificate(t *testing.T) {
	cert, pool := testCertFor(t, "somewhere.else")
	addr, _ := startDoH(t, cert, echoReply("10.0.0.2"))
	e := newDoHExchanger(dohUpstream(addr), 5*time.Second, pool) // expects doh.test
	t.Cleanup(func() { _ = e.Close() })

	if _, err := e.Exchange(context.Background(), query("example.com")); err == nil {
		t.Fatal("Exchange succeeded against a certificate for the wrong name")
	}
}
```

Add `"crypto/tls"` to this file's imports for the 429 test.

- [ ] **Step 3: Run them to watch them fail**

Run: `go test ./internal/upstream/ -run TestDoH -v 2>&1 | head -20`
Expected: FAIL to compile — `undefined: newDoHExchanger`.

- [ ] **Step 4: Write the DoH exchanger**

Create `internal/upstream/doh.go`:

```go
package upstream

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/miekg/dns"
)

const dohContentType = "application/dns-message"

// dohExchanger is DNS over HTTPS (RFC 8484), POST only.
type dohExchanger struct {
	url    string
	client *http.Client
}

// newDoHExchanger builds the transport for u.
//
// The URL is built from u.VerifyName and the dialer is pinned to u.Addr.
// That inversion is what lets a DNS server use DNS-over-HTTPS without
// needing DNS: the name in the URL makes the Host header and the TLS SNI
// correct with no special-casing, and DialContext discards the address it
// is handed, so net/http never performs a lookup.
func newDoHExchanger(u Upstream, timeout time.Duration, roots *x509.CertPool) *dohExchanger {
	dialer := &net.Dialer{Timeout: timeout}
	// The URL keeps a non-default port so the Host header matches what the
	// operator configured; on 443 it is omitted, as it would be in a browser.
	host := u.VerifyName
	if _, port, err := net.SplitHostPort(u.Addr); err == nil && port != defaultDoHPort {
		host = net.JoinHostPort(u.VerifyName, port)
	}
	return &dohExchanger{
		url: (&url.URL{Scheme: "https", Host: host, Path: u.Path}).String(),
		client: &http.Client{
			Timeout: timeout,
			Transport: &http.Transport{
				ForceAttemptHTTP2:   true,
				MaxIdleConnsPerHost: maxIdleConns,
				IdleConnTimeout:     idleConnTimeout,
				TLSClientConfig: &tls.Config{
					RootCAs:    roots, // nil = the system roots
					MinVersion: tls.VersionTLS12,
				},
				DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
					// The address argument is discarded on purpose: it was
					// derived from the URL's host, which is a name we must
					// never look up.
					return dialer.DialContext(ctx, network, u.Addr)
				},
			},
		},
	}
}

func (e *dohExchanger) Exchange(ctx context.Context, m *dns.Msg) (*dns.Msg, error) {
	if err := padQuery(m, paddingBlock); err != nil {
		return nil, err
	}
	// RFC 8484 §4.1: the ID carries no meaning over HTTP, and a constant
	// zero makes responses cacheable by the HTTP layer. The caller's ID is
	// restored below, because callers above this seam do match on it.
	id := m.Id
	m.Id = 0
	wire, err := m.Pack()
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.url, bytes.NewReader(wire))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", dohContentType)
	req.Header.Set("Accept", dohContentType)
	resp, err := e.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("doh %s: %s", e.url, resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, dns.MaxMsgSize))
	if err != nil {
		return nil, err
	}
	r := new(dns.Msg)
	if err := r.Unpack(body); err != nil {
		return nil, fmt.Errorf("doh %s: %w", e.url, err)
	}
	r.Id = id
	stripPadding(r)
	return r, nil
}

// Close releases the transport's idle connections.
func (e *dohExchanger) Close() error {
	e.client.CloseIdleConnections()
	return nil
}
```

- [ ] **Step 5: Run the tests**

Run: `go test -race ./internal/upstream/ -run TestDoH -v 2>&1 | tail -20`
Expected: PASS, all five.

- [ ] **Step 6: Prove the ID restore is load-bearing**

```bash
sed -i 's/^\tr.Id = id$/\t_ = id \/\/ MUTANT/' internal/upstream/doh.go
git diff --stat internal/upstream/doh.go   # must show 1 file changed
go test ./internal/upstream/ -run TestDoHZeroesTheWireID 2>&1 | tail -5
git checkout internal/upstream/doh.go
```
Expected: FAIL — "the reply carries ID 0x0, want 0x1234".

- [ ] **Step 7: Commit**

```bash
gofmt -l internal && go test -race ./internal/upstream/ > /dev/null && git add internal/upstream/doh.go internal/upstream/doh_test.go internal/upstream/tlstest_test.go && git commit -m "feat(upstream): DNS-over-HTTPS (RFC 8484)"
```

---

### Task 7: Wiring, and the no-downgrade guarantee

**Files:**
- Modify: `internal/upstream/forwarder.go` — `newUp` dispatches on scheme
- Test: `internal/upstream/forwarder_encrypted_test.go` (new file, so `forwarder_test.go` stays untouched)

**Interfaces:**
- Consumes: everything from Tasks 1-6
- Produces:
  ```go
  const encryptedTimeout = 5 * time.Second
  func newUp(u Upstream, timeout time.Duration) *up // now dispatches on u.Scheme
  ```

**Why encrypted upstreams get a longer timeout.** `Config.Timeout` defaults to 2s and nothing overrides it. A warm pooled connection answers well inside that. A **cold** one pays a TCP handshake plus a TLS handshake first, and 2s is tight enough that the first query to a distant resolver would fail against a configuration that is working. The floor is raised only for the encrypted transports and only upward, so a caller asking for more still gets it. No new setting.

- [ ] **Step 1: Write the failing tests**

Create `internal/upstream/forwarder_encrypted_test.go`:

```go
package upstream

import (
	"context"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
)

// A tls:// entry in the settings reaches the DoT server, end to end through
// the Forwarder rather than through the exchanger alone.
func TestForwarderUsesDoT(t *testing.T) {
	cert, pool := testCertFor(t, dotName)
	addr, _ := startDoT(t, cert, answerA("10.0.0.9"))
	f, err := New(Config{Upstreams: []string{"tls://" + addr + "#" + dotName}, Strategy: "failover"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })
	// The self-signed certificate is not in the system roots, so the
	// exchanger New built has to be told about it. This reaches inside on
	// purpose: everything else about the path — parsing, newUp's dispatch,
	// the strategy, markResult — is the production one.
	f.def[0].ex = newDoTExchanger(dotUpstream(addr), encryptedTimeout, pool)

	resp, err := f.Handler().ServeDNS(context.Background(), req("example.com"))
	if err != nil {
		t.Fatalf("ServeDNS: %v", err)
	}
	if len(resp.Msg.Answer) != 1 {
		t.Fatalf("got %d answers, want 1", len(resp.Msg.Answer))
	}
	if resp.Upstream != "tls://"+addr+"#"+dotName {
		t.Errorf("Response.Upstream = %q, want the canonical entry", resp.Upstream)
	}
}

// newUp builds the transport the scheme names, and nothing else.
func TestNewUpDispatchesOnScheme(t *testing.T) {
	for _, tc := range []struct {
		entry string
		want  string
	}{
		{"1.1.1.1:53", "*upstream.plainExchanger"},
		{"tls://1.1.1.1:853#cloudflare-dns.com", "*upstream.dotExchanger"},
		{"https://1.1.1.1/dns-query#cloudflare-dns.com", "*upstream.dohExchanger"},
	} {
		t.Run(tc.entry, func(t *testing.T) {
			ups, err := ParseUpstreams(tc.entry)
			if err != nil {
				t.Fatalf("ParseUpstreams: %v", err)
			}
			u := newUp(ups[0], 2*time.Second)
			t.Cleanup(func() { _ = u.ex.Close() })
			if got := fmt.Sprintf("%T", u.ex); got != tc.want {
				t.Errorf("newUp(%q) built %s, want %s", tc.entry, got, tc.want)
			}
		})
	}
}

// **The guarantee.** An encrypted upstream that cannot be verified fails.
// It does not quietly try again in plaintext — which would make the whole
// feature a lie at exactly the moment it matters, with nothing on screen
// saying so.
//
// The plain listener is bound to the same host and the same port number on
// UDP. UDP and TCP are separate namespaces, so it sits beside the DoT
// listener rather than fighting it, and plainExchanger tries UDP first — so
// this is precisely where a downgrade would land. Asserting only that the
// query failed would pass even if a plaintext attempt had succeeded first
// and then been discarded; the counter is what makes the test discriminate.
func TestEncryptedUpstreamNeverFallsBackToPlaintext(t *testing.T) {
	cert, _ := testCertFor(t, "right.test")
	addr, _ := startDoT(t, cert, answerA("10.0.0.1"))
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("splitting %q: %v", addr, err)
	}

	var plainHits atomic.Int64
	pc, err := net.ListenPacket("udp", net.JoinHostPort(host, port))
	if err != nil {
		t.Fatalf("binding the plaintext listener beside the DoT one: %v", err)
	}
	plain := &dns.Server{PacketConn: pc, Handler: dns.HandlerFunc(func(w dns.ResponseWriter, m *dns.Msg) {
		plainHits.Add(1)
		r := new(dns.Msg)
		r.SetReply(m)
		_ = w.WriteMsg(r)
	})}
	go func() { _ = plain.ActivateAndServe() }()
	t.Cleanup(func() { _ = plain.Shutdown() })

	// #wrong.test against a certificate for right.test, and the certificate
	// is self-signed so it is not in the system roots either: verification
	// fails for two independent reasons.
	f, err := New(Config{
		Upstreams: []string{"tls://" + addr + "#wrong.test"},
		Strategy:  "failover",
		Timeout:   2 * time.Second,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })

	if _, err := f.Handler().ServeDNS(context.Background(), req("example.com")); err == nil {
		t.Fatal("the query succeeded against an unverifiable upstream")
	}
	if n := plainHits.Load(); n != 0 {
		t.Errorf("the plaintext listener received %d queries — the encrypted upstream downgraded", n)
	}
}
```

Add `"fmt"` to the imports.

- [ ] **Step 2: Run them to watch them fail**

Run: `go test ./internal/upstream/ -run 'TestForwarderUsesDoT|TestNewUpDispatches|TestEncryptedUpstreamNever' -v 2>&1 | head -25`
Expected: FAIL — `undefined: encryptedTimeout`, and `TestNewUpDispatchesOnScheme` reporting `*upstream.plainExchanger` for all three, because `newUp` does not dispatch yet.

- [ ] **Step 3: Make `newUp` dispatch**

In `internal/upstream/forwarder.go`:

```go
// encryptedTimeout is the floor for a DoT or DoH exchange.
//
// A warm pooled connection answers in one round trip, well inside the 2s
// default. A cold one pays a TCP handshake and a TLS handshake first, and
// 2s is tight enough that the first query to a distant resolver fails on a
// configuration that is working. Only raised, never lowered: a caller
// asking for more still gets it.
const encryptedTimeout = 5 * time.Second

func newUp(u Upstream, timeout time.Duration) *up {
	switch u.Scheme {
	case SchemeDoT:
		return &up{addr: u.Canonical, ex: newDoTExchanger(u, max(timeout, encryptedTimeout), nil)}
	case SchemeDoH:
		return &up{addr: u.Canonical, ex: newDoHExchanger(u, max(timeout, encryptedTimeout), nil)}
	default:
		return &up{addr: u.Canonical, ex: newPlainExchanger(u.Addr, timeout)}
	}
}
```

`nil` roots is the system trust store — the only thing production ever passes, and the reason neither constructor takes a `*tls.Config` that could disable verification.

- [ ] **Step 4: Run the whole suite**

Run: `go test -race ./internal/... 2>&1 | tail -20`
Expected: PASS everywhere, `forwarder_test.go` still unmodified.

- [ ] **Step 5: Prove the no-downgrade test discriminates**

The mutation has to be a *downgrade*, not just a break — a test that fails when the upstream stops working proves nothing about fallback:

```bash
python3 - <<'EOF'
p = "internal/upstream/forwarder.go"
s = open(p).read()
old = "\t\treturn &up{addr: u.Canonical, ex: newDoTExchanger(u, max(timeout, encryptedTimeout), nil)}"
assert old in s, "MUTATION DID NOT APPLY — anchor not found"
s = s.replace(old, "\t\treturn &up{addr: u.Canonical, ex: newPlainExchanger(u.Addr, timeout)} // MUTANT: downgrade", 1)
open(p, "w").write(s)
print("mutation applied: tls:// now builds a plaintext exchanger")
EOF
go test ./internal/upstream/ -run TestEncryptedUpstreamNeverFallsBackToPlaintext 2>&1 | tail -5
git checkout internal/upstream/forwarder.go
```

Expected: FAIL with "the plaintext listener received 1 queries — the encrypted upstream downgraded". A pass would mean the UDP listener is not where a downgrade lands, and the test needs rethinking rather than accepting.

- [ ] **Step 6: Commit**

```bash
gofmt -l internal && go test -race ./internal/... > /dev/null && git add -A internal && git commit -m "feat(upstream): route tls:// and https:// entries to their transports"
```

---

### Task 8: The grammar, mirrored in TypeScript

**Files:**
- Create: `web/src/lib/upstreams.ts`
- Create: `web/src/lib/upstreams.test.ts`

**Interfaces:**
- Consumes: `internal/upstream/testdata/grammar.json` (Task 1) — read directly by the test
- Produces:
  ```ts
  export type UpstreamScheme = "udp" | "tls" | "https";
  export interface Upstream {
    scheme: UpstreamScheme;
    addr: string;
    verifyName: string;
    path: string;
    canonical: string;
  }
  export type UpstreamErrorCode =
    | "host_not_ip" | "missing_name" | "name_on_plain"
    | "mixed_schemes" | "bad_scheme" | "bad_addr" | "bad_url" | "empty";
  export interface UpstreamError { code: UpstreamErrorCode; entry: string; message: string }
  export type ParseResult = { ok: true; entries: Upstream[] } | { ok: false; error: UpstreamError };
  export function parseUpstreams(value: string): ParseResult;
  export function buildUpstream(scheme: UpstreamScheme, addr: string, verifyName: string, path?: string): string;
  ```

**Why a mirror rather than trusting the server.** The form has to tell the operator what is wrong *while they type*, and a round trip per keystroke is not that. The existing settings page already mirrors every server validator for the same reason (`web/src/pages/settings.tsx:94-130`), with a comment warning that an over-strict mirror false-rejects legitimate values — the failure this task must not repeat.

**The fixture is what keeps the two honest.** Both parsers are held to `internal/upstream/testdata/grammar.json`. They are compared on the parsed fields and on the rejection **code**, never on the message: Go renders one wording and the web renders its own, and a test coupling them would make copy an interface.

- [ ] **Step 1: Write the failing test**

Create `web/src/lib/upstreams.test.ts`:

```ts
import { readFileSync } from "node:fs";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { describe, expect, it } from "vitest";
import { parseUpstreams } from "./upstreams";

// The same file internal/upstream/addr_test.go reads. Two parsers, one
// grammar: a change here fails both suites until both agree, which is the
// only thing keeping the form's rules and the server's rules from drifting.
const fixture = JSON.parse(
  readFileSync(
    resolve(dirname(fileURLToPath(import.meta.url)), "../../../internal/upstream/testdata/grammar.json"),
    "utf8",
  ),
) as {
  accept: { in: string; scheme: string; addr: string; verifyName: string; path: string; canonical: string }[];
  reject: { in: string; code: string }[];
  acceptList: { in: string; count: number }[];
  rejectList: { in: string; code: string }[];
};

describe("parseUpstreams", () => {
  it.each(fixture.accept)("accepts $in", (c) => {
    const got = parseUpstreams(c.in);
    if (!got.ok) throw new Error(`rejected with ${got.error.code}: ${got.error.message}`);
    expect(got.entries).toHaveLength(1);
    expect(got.entries[0]).toEqual({
      scheme: c.scheme,
      addr: c.addr,
      verifyName: c.verifyName,
      path: c.path,
      canonical: c.canonical,
    });
  });

  it.each([...fixture.reject, ...fixture.rejectList])("rejects $in as $code", (c) => {
    const got = parseUpstreams(c.in);
    if (got.ok) throw new Error(`accepted, but the grammar says ${c.code}`);
    expect(got.error.code).toBe(c.code);
  });

  it.each(fixture.acceptList)("parses $in into $count entries", (c) => {
    const got = parseUpstreams(c.in);
    if (!got.ok) throw new Error(`rejected with ${got.error.code}: ${got.error.message}`);
    expect(got.entries).toHaveLength(c.count);
  });

  // canonical is what gets saved when the form builds an entry from the
  // preset picker, so parsing it again has to produce the same upstream.
  it.each(fixture.accept)("round-trips the canonical form of $in", (c) => {
    const first = parseUpstreams(c.in);
    if (!first.ok) throw new Error(`rejected: ${first.error.message}`);
    const again = parseUpstreams(first.entries[0].canonical);
    if (!again.ok) throw new Error(`canonical form rejected: ${again.error.message}`);
    expect(again.entries[0]).toEqual(first.entries[0]);
  });
});
```

- [ ] **Step 2: Run it to watch it fail**

Run: `cd web && pnpm vitest run src/lib/upstreams.test.ts 2>&1 | tail -15`
Expected: FAIL — cannot resolve `./upstreams`.

- [ ] **Step 3: Write the mirror**

Create `web/src/lib/upstreams.ts`. Mirror `internal/upstream/addr.go` clause for clause; the notes below are the ones where TypeScript differs from Go and getting them wrong produces a mirror that disagrees with the server:

- **Use `URL` only when the entry contains `://`.** `new URL("1.1.1.1:53")` reads `1.1.1.1` as the protocol, exactly as Go's `url.Parse` does. A bare entry takes the plain path.
- **`URL` lowercases the protocol and strips the trailing `:`** — compare `u.protocol` as `"tls:"`, or slice it.
- **`URL.hostname` KEEPS the brackets on an IPv6 literal** — verified on Node v25 (`new URL('tls://[2606:4700:4700::1111]:853#n').hostname === '[2606:4700:4700::1111]'`), per the URL Standard's host serializer. Go's `u.Hostname()` strips them. The two languages disagree here, so a mirror that assumes either behaviour is fragile: normalise to a bare host first, then re-bracket when composing `canonical`, letting the rest of the module assume Go's bracket-free contract. (An earlier draft asserted the opposite; the Task 8 implementer caught it.)
- **`URL` percent-decodes the fragment**; `u.hash` includes the leading `#`. Take `decodeURIComponent(u.hash.slice(1))`.
- **`new URL` does not reject a bad port** the way `strconv.Atoi` does — it throws only for some cases. Check the port explicitly: digits only, 1-65535.
- **IP detection:** an IPv4 literal is four dot-separated decimal octets 0-255; an IPv6 literal is anything the `URL` constructor accepted inside brackets. Reject everything else as `host_not_ip`. Do not use a permissive "contains a dot or a colon" test — `dns.google` contains a dot.
- **Plain entries validate nothing beyond splittability**, matching the Go side. `resolver.lan:5353` must be accepted; a mirror that requires an IP here rejects a working configuration and is the exact failure the existing settings-page comment warns about.

`buildUpstream` is what the preset picker calls: it assembles `scheme://addr[path]#name` and is the inverse of the canonical form, so `parseUpstreams(buildUpstream(...)).entries[0].canonical === buildUpstream(...)`.

- [ ] **Step 4: Run the tests**

Run: `cd web && pnpm vitest run src/lib/upstreams.test.ts 2>&1 | tail -15`
Expected: PASS, every fixture case as a named case.

- [ ] **Step 5: Prove the mirror is reading the shared fixture**

```bash
python3 - <<'EOF'
import json
p = "internal/upstream/testdata/grammar.json"
d = json.load(open(p))
d["reject"].append({"in": "tls://1.1.1.1:853#name", "code": "bad_scheme"})
json.dump(d, open(p, "w"), indent=2)
print("mutation applied: added a case the grammar says is valid")
EOF
cd web && pnpm vitest run src/lib/upstreams.test.ts 2>&1 | tail -6
cd .. && git checkout internal/upstream/testdata/grammar.json
```

Expected: FAIL on the added case in the **web** suite — proving it is driven by that file and not by a copy. Run `go test ./internal/upstream/ -run TestParseUpstreamsRejects` under the same mutation and confirm it also fails: one fixture, two suites.

- [ ] **Step 6: Commit**

```bash
cd web && pnpm lint && pnpm format:check && pnpm typecheck && pnpm test && cd .. && git add web/src/lib/upstreams.ts web/src/lib/upstreams.test.ts && git commit -m "feat(web): mirror the upstream grammar for form validation"
```

---

### Task 9: The upstreams field

**Files:**
- Create: `web/src/components/upstreams-field.tsx`
- Create: `web/src/components/upstreams-field.test.tsx`
- Modify: `web/src/pages/settings.tsx` — a `kind: "upstreams"` field, dispatched in `SettingRow`
- Modify: `web/src/pages/settings.test.tsx` — the upstreams field's presence

**Interfaces:**
- Consumes: `parseUpstreams`, `buildUpstream`, `UpstreamScheme` (Task 8)
- Produces:
  ```tsx
  export function UpstreamsField({ value, onChange }: { value: string; onChange: (next: string) => void }): JSX.Element
  ```

**The shape.** A protocol selector — **Plain / DNS-over-TLS / DNS-over-HTTPS** — above the entries. Choosing an encrypted protocol reveals presets: **Cloudflare, Quad9, Google, Custom**. A preset fills both halves of every entry in one click; Custom leaves two fields per row, **Address** and **Server name**, because those are the two things TLS actually requires and pretending otherwise is what makes the feature confusing.

The presets, which must round-trip through `parseUpstreams` (Task 8's test is the guard):

| Preset | DNS-over-TLS | DNS-over-HTTPS |
|---|---|---|
| Cloudflare | `tls://1.1.1.1:853#cloudflare-dns.com`, `tls://1.0.0.1:853#cloudflare-dns.com` | `https://1.1.1.1:443/dns-query#cloudflare-dns.com` |
| Quad9 | `tls://9.9.9.9:853#dns.quad9.net`, `tls://149.112.112.112:853#dns.quad9.net` | `https://9.9.9.9:443/dns-query#dns.quad9.net` |
| Google | `tls://8.8.8.8:853#dns.google`, `tls://8.8.4.4:853#dns.google` | `https://8.8.8.8:443/dns-query#dns.google` |

**Why the protocol selector is one control for the whole list, not one per row.** The grammar rejects a mixed list, so a per-row selector would let the operator build a value the form then refuses to save — offering a choice and then punishing it. Switching protocol converts the current entries, or clears them when they cannot be converted (a plain hostname has no address to carry into a `tls://` entry), and says which it did.

**Copy states the fact and stops.** "Queries to upstream resolvers are encrypted." What that does and does not protect belongs in `docs/`, which Task 10 writes — not on this screen. Specifically: do not put the "your chosen resolver still sees every query" caveat in the UI. It is true, it is important, and it is three sentences long.

**Read the existing page first.** `SettingRow` (`web/src/pages/settings.tsx:435`) dispatches on `field.kind` and wraps everything in `FormField`/`FormItem`; `SettingRadioList` above it is the pattern for a grouped choice, including its `aria-label`-not-`<legend>` note. Follow both. The new kind slots into the `SettingField` union beside `TextField`, `IntField` and `SelectField`, and the `upstreams` entry in `SETTING_GROUPS` (line ~160) changes `kind: "text"` to `kind: "upstreams"`, keeping its `key`, `label` and `schema`.

- [ ] **Step 1: Write the failing test**

Create `web/src/components/upstreams-field.test.tsx`. Write it against the real component API and this project's existing testing-library conventions — read `web/src/components/pause-control.test.tsx` for the harness shape. Pin these behaviours, each of which a plausible wrong implementation would get wrong:

1. **Given a plain value, the Plain protocol is selected** and no server-name field is shown.
2. **Given `tls://1.1.1.1:853#cloudflare-dns.com`, DNS-over-TLS is selected** and both the address `1.1.1.1:853` and the server name `cloudflare-dns.com` are visible as separate values — not one opaque string.
3. **Choosing the Cloudflare preset under DNS-over-TLS calls `onChange` with a value that `parseUpstreams` accepts**, and whose entries are all `tls`. Assert by parsing the emitted string, not by string equality — the test then survives a change to the preset's second address.
4. **Typing a hostname into Address under DNS-over-TLS shows the `host_not_ip` message and does not call `onChange`.** This is the Camp 1 rule reaching the operator at the moment they make the mistake, which is the whole point of the mirror.
5. **Switching from DNS-over-TLS to Plain keeps the addresses and drops the server names**, and switching from Plain with a hostname entry to DNS-over-TLS clears it and says so. A silent clear is a lost configuration.

- [ ] **Step 2: Run it to watch it fail**

Run: `cd web && pnpm vitest run src/components/upstreams-field.test.tsx 2>&1 | tail -15`
Expected: FAIL — cannot resolve `./upstreams-field`.

- [ ] **Step 3: Build the component and wire it in**

Write `web/src/components/upstreams-field.tsx`, then in `web/src/pages/settings.tsx`: add the `UpstreamsField` variant to the `SettingField` union, change the `upstreams` entry's `kind`, replace `nonEmptySchema` on that field with one that runs `parseUpstreams` and surfaces `error.message`, and add the branch to `SettingRow`'s dispatch beside the existing `field.kind === "select"` case.

Update the comment at `web/src/pages/settings.tsx:94` — it currently says the server does no format checking on `upstreams`, which stops being true in Task 2 and would otherwise stand as a note telling the next reader not to validate.

- [ ] **Step 4: Run the tests**

Run: `cd web && pnpm vitest run 2>&1 | tail -15`
Expected: PASS, including `settings.test.tsx` unchanged except for the upstreams field's own assertions.

- [ ] **Step 5: Look at it**

```bash
cd web && pnpm build && cd .. && go build -o /tmp/dnsaur ./cmd/dnsaur
```

Run the binary bound to the host's LAN address (`0.0.0.0` and the host IP, **not** `127.0.0.1`), open Settings, and check the field against the three protocols. Confirm: presets fill both halves; a hostname under DNS-over-TLS is refused in place with a message naming the fix; switching protocols says what it did to the existing entries.

- [ ] **Step 6: Commit**

```bash
cd web && pnpm lint && pnpm format:check && pnpm typecheck && pnpm test && pnpm build && cd .. && git add web && git commit -m "feat(web): protocol selector and presets for upstreams"
```

---

### Task 10: Documentation

**Files:**
- Modify: `internal/api/openapi.yaml` — lines 14-16 and ~811, where `upstreams` is described as a comma-separated list
- Modify: `docs/configuration.md` — the `upstreams` row and a new section
- Modify: `README.md` — encrypted upstreams in the feature list

**Interfaces:**
- Consumes: the grammar (Task 1), the settings behaviour (Task 2), the UI (Task 9)

- [ ] **Step 1: Update the OpenAPI description**

Both places that describe `upstreams` state the extended grammar with one example per scheme, and note that a malformed value is now rejected with 400 and a reason — which is a behaviour change to `PUT /settings` and belongs in the document that describes it.

- [ ] **Step 2: Write the configuration section**

In `docs/configuration.md`, under the upstreams row, add a section covering:

- The three forms with a worked example each.
- **Why an address and not a hostname** — the circular dependency in one short paragraph, and that Unbound, systemd-resolved, Stubby and Knot all ask for the address too. Not the full two-camps analysis; a pointer to the spec for that.
- **What encryption buys and what it does not.** The path stops seeing the names; the resolver you chose still sees all of them. This is the caveat deliberately kept off the settings screen, and this is where it goes.
- The presets table from Task 9.
- That all entries must share a transport, and why.
- That encrypted upstreams are the **global** setting only: a forwarder zone's `forward_to` stays plaintext, because it points at internal resolvers on trusted networks.

- [ ] **Step 3: Update the README**

One line in the feature list. Match the surrounding density — the README summarizes, `docs/configuration.md` explains.

- [ ] **Step 4: Check every link and claim**

```bash
grep -rn "](.*\.md" README.md docs/ | grep -v http | while IFS= read -r line; do
  f=$(echo "$line" | sed 's/.*](\([^)#]*\).*/\1/')
  d=$(dirname "$(echo "$line" | cut -d: -f1)")
  [ -e "$d/$f" ] || echo "BROKEN: $line"
done
```
Expected: no output.

- [ ] **Step 5: Commit**

```bash
git add README.md docs internal/api/openapi.yaml && git commit -m "docs: encrypted upstreams (DoT/DoH)"
```

---

## Done when

- `go test -race ./...` passes, `~/go/bin/golangci-lint run ./...` is clean, `gofmt -l internal cmd` is empty.
- `cd web && pnpm lint && pnpm format:check && pnpm typecheck && pnpm test && pnpm build && pnpm test:e2e` passes.
- `internal/upstream/forwarder_test.go` differs from the branch point by exactly the one `newUp` construction line: `git diff main -- internal/upstream/forwarder_test.go` shows a single changed line and no changed assertion.
- One grammar: `internal/upstream/testdata/grammar.json` drives both `addr_test.go` and `upstreams.test.ts`, and a change to it fails both.
- A `tls://` or `https://` upstream that cannot be verified fails, and nothing on the machine ever receives that query in plaintext.
- `git status --porcelain` is empty and every mutation used as evidence has been reverted.
