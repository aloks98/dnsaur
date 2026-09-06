# dnsaur Encrypted Upstreams (DoT/DoH): design

**Status:** design, approved for implementation 2026-09-05
**Scope:** the *upstream* half only — dnsaur → public resolver over TLS.
**Explicitly deferred:** the *serving* half (clients → dnsaur over DoT/DoH),
which gets its own spec. See §2.

---

## 1. Why

Every query dnsaur cannot answer locally goes out over plaintext UDP on port
53. Anyone on the path — the ISP first among them — sees every name this
network looks up, in the clear, attributable to this connection. Blocklists
and a local cache reduce the *volume* of that leak; they do not change its
nature.

Encrypting the upstream hop closes it. The ISP sees that dnsaur talks to
`1.1.1.1:853` and nothing about what it asks. That is the entire goal of this
milestone, and it is achieved by changing one thing: the transport used to
reach an upstream.

It is worth being precise about what this does *not* buy. The chosen
resolver still sees every query — encryption moves trust from "everyone on
the path" to "the operator of the resolver you picked", it does not remove
trust. Removing it requires running our own recursion, which is deliberately
a later release (task #68).

---

## 2. Scope: this is one of two subsystems, and only one is in scope

"Support DoT and DoH" names two features that share no code:

| | **E1 — encrypted upstreams (this spec)** | **E2 — encrypted serving (deferred)** |
|---|---|---|
| Direction | dnsaur → Cloudflare/Quad9 | client → dnsaur |
| Code | `internal/upstream` | `internal/dnssrv`, `internal/api` |
| Buys | the ISP stops seeing queried names | a second encryption layer, typically inside an existing VPN |
| New machinery | TLS dialing, connection pooling | RFC 8484 handler, PROXY protocol, trusted-proxy ACL |

They are sequenced rather than merged because E2's cost is concentrated
entirely in a problem E1 does not have: **client identity**. dnsaur derives
`Request.ClientIP` from `w.RemoteAddr()` (`internal/dnssrv/server.go:147-153`)
and that address drives per-client filtering. Behind a TLS-terminating
reverse proxy every encrypted client collapses into the proxy's address, so
per-client anything silently stops meaning what it says. Recovering identity
needs `X-Forwarded-For` for DoH and the PROXY protocol for DoT, both gated on
a trusted-proxy allowlist — three mechanisms with their own spoofing
failure modes.

E2 depends on nothing in E1, so nothing is lost by sequencing. Note that
dnsaur **already serves DNS over TCP** (`server.go`, `s.tcp`), so DoT behind
a TLS-terminating proxy already works today *except* for client identity —
which is precisely the deferred part.

---

## 3. The bootstrap problem, and why hostnames are rejected

To reach Cloudflare's DoT endpoint you connect to `cloudflare-dns.com:853`.
Resolving that name is a DNS lookup, and dnsaur *is* the DNS. On boot it
would need an upstream to find its upstream.

Separately, TLS certificates are issued to **names**, not addresses. Dialing
`1.1.1.1` and asking the certificate "are you 1.1.1.1?" fails even when it is
genuinely Cloudflare. So two distinct values are always needed:

- **the dial address** — an IP and port; requires no DNS
- **the verification name** — what the certificate must assert; never dialed

Products split into two camps on how the operator supplies these:

**Camp 1 — the operator supplies the IP.** Unbound
(`forward-addr: 1.1.1.1@853#cloudflare-dns.com`), systemd-resolved
(`DNS=1.1.1.1#cloudflare-dns.com`), Stubby (`address_data` + `tls_auth_name`),
Knot Resolver, Technitium (`1.1.1.1 (cloudflare-dns.com)`). There is no
hostname in the configuration, so the circular dependency does not exist —
it is deleted rather than worked around.

**Camp 2 — hostnames allowed, with a dedicated bootstrap resolver.** AdGuard
Home (`bootstrap_dns`), dnscrypt-proxy (`bootstrap_resolvers`). A separate
list of plaintext servers, used only to resolve encrypted-upstream hostnames.

**dnsaur takes Camp 1.** It removes an entire subsystem: no bootstrap
setting, no re-resolution timer, no TTL handling, no "which upstream did the
bootstrap go through" question, and no stale-frozen-address failure mode.
Camp 2 exists because AdGuard ships to thousands of operators pointing at
arbitrary providers; dnsaur here is one operator pointing at anycast
addresses that have not moved in a decade.

A third option — resolving the hostname once at save time and storing the
result — was considered and rejected. Nobody does it, and it produces a
uniquely confusing failure: the operator typed a name, the stored value is
an address they never chose, and when that address dies the error points at
the address.

**Camp 1 does not foreclose Camp 2.** Adding `upstreams.bootstrap` later is
purely additive; the stored entry format below is identical either way.

---

## 4. Configuration grammar

The `upstreams` setting keeps its comma-separated form. Entries gain an
optional scheme and an optional `#name` suffix — the Unbound and
systemd-resolved convention.

```
1.1.1.1:53                                     plain (unchanged)
udp://1.1.1.1:53                               plain, scheme written explicitly
tls://1.1.1.1:853#cloudflare-dns.com           DoT
https://1.1.1.1/dns-query#cloudflare-dns.com   DoH
```

**Defaults:** port 53 for `udp://` and bare entries, 853 for `tls://`, 443
for `https://`; path `/dns-query` for `https://` when none is given.

**Rules the parser enforces, each rejecting the save with a reason:**

1. For `tls://` and `https://`, the host **must be an IP literal**. A
   hostname is rejected with a message naming the required form. (§3.)
2. For `tls://` and `https://`, `#name` is **required**. Without it there is
   nothing to verify the certificate against, and dnsaur will not fall back
   to verifying against an IP.
3. `#name` on a plain entry is rejected — it would be silently ignored, and
   a silently ignored security parameter reads as one that is in effect.
4. **All entries must share one scheme.** Mixing means some queries are
   encrypted and some are not, with no way to tell which; the privacy
   property becomes unpredictable rather than partial.
5. Bare IPv6 literals are bracketed before the port is appended, as
   `parseUpstreams` does today.

### The parser consolidates two implementations that do not agree

The grammar currently lives in two places. `parseUpstreams`
(`internal/app/app.go:294`) does the real work — trimming, dropping empties,
defaulting the port. The API's validator for the same key
(`internal/api/settings_handlers.go:11`) is `strings.TrimSpace(v) != ""`,
which validates nothing: a malformed entry is accepted at save and silently
dropped at boot.

Teaching schemes to both would guarantee drift. So:

- The parser moves to **`internal/upstream/addr.go`**, exporting
  `ParseUpstreams(string) ([]Upstream, error)`. It rejects with a typed
  `*ParseError` carrying a stable `Code` (`host_not_ip`, `missing_name`,
  `name_on_plain`, `mixed_schemes`, `bad_scheme`, `bad_addr`, `bad_url`,
  `empty`) beside the human message, so the web mirror can assert on the
  same reason without depending on Go's copy.
- `internal/app` calls it instead of its private copy.
- The API validator calls it too, and **rejects a malformed `upstreams`
  value at save** with the parser's own message.

This is a behaviour change beyond the feature and is intended: it is the
difference between a typo failing at the moment it is made and a typo taking
DNS down at the next restart.

```go
// internal/upstream/addr.go
type Scheme string

const (
    SchemePlain Scheme = "udp"   // UDP with TCP fallback on truncation
    SchemeDoT   Scheme = "tls"
    SchemeDoH   Scheme = "https"
)

// Upstream is one parsed entry. Addr is always a literal host:port — it is
// never a name, and resolving it is never required.
type Upstream struct {
    Scheme     Scheme
    Addr       string // "1.1.1.1:853"
    VerifyName string // certificate name; empty for SchemePlain
    Path       string // DoH only; defaults to "/dns-query"
    Canonical  string // normalized spelling: port and path always written
}
```

`Upstream.Canonical` is the identity `SetConditional` reuses `*up`s by
(`forwarder.go:216-223`) and what `Response.Upstream` reports. It is the
*normalized* spelling rather than the entry as typed — port and path always
written, matching `FormatForwardTo`'s existing convention — so that
`tls://1.1.1.1#n` and `tls://1.1.1.1:853#n` are one upstream sharing one
health record rather than two.

---

## 5. The transport seam

`up` currently hardcodes two `*dns.Client`s (`forwarder.go:26-40`). They
become one interface:

```go
type up struct {
    addr      string     // Upstream.Raw — identity, display, reuse key
    ex        exchanger
    ewmaMicro atomic.Int64
    fails     atomic.Int32
    downUntil atomic.Int64
}

// exchanger sends one query and returns one reply. Implementations own their
// transport entirely: dialing, pooling, TLS, and any transport-specific
// message rewriting (0x20, padding, DoH's zero ID) happen below this line.
type exchanger interface {
    Exchange(ctx context.Context, m *dns.Msg) (*dns.Msg, error)
    Close() error
}
```

Everything above the seam is untouched: `markResult`, `healthy`, `pick`,
`race`, the RFC 9520 failure cache, the strategy switch, and
`SetConditional`'s reuse-by-address with its EWMA and backoff preservation.
That is why the seam goes here and not higher — the health and routing
machinery is transport-agnostic already, and this change proves it.

`Forwarder.exchange` reduces to: copy the message once, hand the copy to
`u.ex.Exchange`, then `markResult` on the outcome. The copy is made here (as
today, `forwarder.go:139`) and the exchanger may mutate it freely.

### Three implementations

**`plainExchanger`** — today's behaviour moved verbatim: 0x20 scramble, UDP
`ExchangeContext`, TCP retry on truncation, 0x20 verify, case restore. No
functional change; the existing forwarder tests are the proof of that.

**`dotExchanger`** — `dns.Client{Net: "tcp-tls", TLSConfig: &tls.Config{
ServerName: VerifyName}}` dialing `Addr`, over a pooled connection (§6).

**`dohExchanger`** — RFC 8484 POST. The request URL is built from the
**verification name**, not the address:

```
https://cloudflare-dns.com/dns-query      ← URL: sets Host header and SNI correctly
                                            for free
Transport.DialContext ignores the address it is given and dials 1.1.1.1:443
```

That inversion is the whole trick: the URL carries the name so certificate
verification and the `Host` header are correct without special-casing, and
the pinned dialer means Go's HTTP client never performs a DNS lookup. Body
is the wire-format query with `Content-Type: application/dns-message` and
`Accept: application/dns-message`; a non-200 response is an error carrying
the status.

Per RFC 8484 §4.1 the message **ID is set to 0** on the wire (it improves
HTTP cache behaviour and carries no meaning over HTTP), and the caller's
original ID is restored on the reply before it is returned.

### 0x20 is dropped on the encrypted paths

Case randomization (`scramble`, `forwarder.go:126`) raises the difficulty of
off-path cache-poisoning against plaintext UDP. Inside an authenticated TLS
channel there is no off-path attacker to defend against, and resolvers that
normalize case would turn the verification step into false failures against
a correctly-working upstream. It stays exactly as-is on `plainExchanger`.

### Never downgrade

A TLS handshake failure, a certificate verification failure, or an
unreachable endpoint is a **failure**: it feeds `markResult`, counts toward
the three-strike backoff, and the query moves to the next upstream by the
configured strategy. It is **never** retried in plaintext. A silent fallback
would make the feature a lie at exactly the moment it matters, and the
operator would have no way to notice.

This is asserted by a test, not only by inspection: §10.

---

## 6. Connection reuse

DoH gets pooling free from `http.Transport` (`ForceAttemptHTTP2: true`,
`MaxIdleConnsPerHost`, `IdleConnTimeout`). DoT does not: miekg's
`Client.ExchangeContext` dials per call, so without a pool **every query pays
a full TCP + TLS handshake** — two extra round trips on a hop that should
cost one. RFC 7858 §3.4 exists for this reason.

`dotExchanger` therefore owns a small pool of `*dns.Conn`:

- **Cap 4 idle connections** per upstream, each used by exactly one query at
  a time. Serialised use means DNS message IDs on a connection can never
  interleave, so `Client.ExchangeWithConnContext`'s ID check is sufficient
  and no pipelining bookkeeping is needed.
- **Idle deadline 30s**, checked on `get()`: a connection older than that is
  closed and a fresh one dialed. There is no background reaper — with four
  connections per upstream and a handful of upstreams the worst-case idle
  descriptor count is bounded and small.
- A connection that **errors is closed**, never returned to the pool.

### The stale-connection retry

This is the defect a hand-rolled pool always ships with, so it is specified
rather than left to judgement.

A DoT server may close an idle connection at any time — that is normal and
expected, not a fault. If dnsaur borrows such a connection and gets a read
error, treating it as a query failure would surface the server's routine
housekeeping as intermittent SERVFAILs and as spurious `markResult`
failures that eventually mark a healthy upstream down.

So: **a failure on a connection taken from the pool triggers exactly one
retry on a freshly dialed connection.** A failure on a connection that was
already fresh is a real failure and is reported. This mirrors what
`http.Transport` does internally, and it is what makes the DoH path work
without any equivalent code.

### Forwarder lifecycle: pooled connections must be closed

`swappable.set` (`internal/app/app.go:64`) replaces the forwarder on every
settings change and drops the old one to the garbage collector. That is
harmless today because `dns.Client` holds nothing. With pooled TLS
connections it becomes a **file-descriptor leak on every settings save**.

So `Forwarder` gains `Close() error`, closing every exchanger it owns —
defaults and conditional routes both. `swappable.set` returns the displaced
forwarder and the caller closes it **outside the lock**, preserving the
documented `routeMu → swappable.mu` ordering (`app.go:147`) rather than
adding an edge to it. `App.Close` closes the live forwarder.

`SetConditional` reuses `*up`s by address, so a swap that keeps an upstream
keeps its pool along with its EWMA and backoff — an upstream that survives a
zone edit does not drop its TLS connections. Upstreams the new table does
*not* adopt are closed.

---

## 7. Padding (RFC 8467)

Encryption hides the query name; it does not hide the query *length*, and
DNS message length correlates strongly with name length. RFC 8467 closes
that channel by padding to a block boundary.

`dotExchanger` and `dohExchanger` pad every outgoing query to a **multiple of
128 octets** (RFC 8467's recommended client block size) using the EDNS(0)
Padding option of RFC 7830 (code 12, `dns.EDNS0_PADDING`):

1. Attach an OPT record if the message has none (UDP size 1232, matching the
   convention in `dnssrv`), carrying a zero-length padding option.
2. Pack the message and measure it: `L`.
3. Grow the padding by `(128 - L%128) % 128` zero octets.

`plainExchanger` does **not** pad. Padding a plaintext query hides nothing
and only wastes bytes.

### Response padding is stripped

A compliant upstream pads its *responses* too (RFC 8467 recommends a 468-octet
block for servers). That padding is meaningful only on the encrypted hop. If
it were left in place it would be cached by `internal/cache`, and then
forwarded to the client over plaintext UDP — where it inflates every response
for no benefit and can push a reply past the client's advertised buffer into
needless truncation.

So both encrypted exchangers **remove the padding option from the reply's OPT
record** before returning. The OPT record itself is kept even when padding
was its only option: it also carries the DO bit, the extended rcode bits and
the advertised UDP size, and dropping it would discard those to save eleven
bytes. This happens in the exchanger, below the seam, so the cache and the
response path never see the padding.

---

## 8. Timeouts

`Config.Timeout` defaults to 2s (`forwarder.go:102`) and nothing overrides
it. A warm pooled connection returns in one round trip, well inside that. A
**cold** connection needs a TCP handshake plus a TLS handshake first, and 2s
is tight enough that a first query to a distant resolver would fail on a
working configuration.

Encrypted exchangers therefore default to **5s** where plain keeps 2s. The
cost is bounded and paid only on cold connections: under `race` a slow cold
upstream simply loses to a warm one, and under `failover`/`fastest` the
delay is the handshake it genuinely needs. No new setting.

---

## 9. What does not change

Stated explicitly because the seam's value is in how much it leaves alone:

- Zone-based conditional routing. A forwarder zone's `forward_to` addresses
  are **plaintext, as today**. They point at internal resolvers on trusted
  networks, where the threat this milestone addresses does not exist, and
  §9.11.5's suffix-claim invariant is untouched.
- Strategy (`race` / `failover` / `fastest`), health tracking, the
  three-strike backoff, the RFC 9520 failure cache, `SetConditional`'s
  reuse-by-address, cache behaviour, the query log, filtering.
- The `upstreams` setting key and its comma-separated shape. Existing
  plaintext values parse unchanged and behave identically.

---

## 10. Test posture

Real servers, no mocks — a local DoT server (`dns.Server{Net: "tcp-tls"}`
with a generated self-signed certificate) and a local DoH server
(`httptest.NewTLSServer` speaking RFC 8484). Each exchanger takes an
injectable `*tls.Config` so tests supply the test root pool without any
production path being able to skip verification.

Beyond the happy path, each of these fails against a plausible wrong
implementation:

| Test | The implementation it catches |
|---|---|
| **Connection reuse** — N queries, count `Accept`s on the listener | A pool that dials per query. Asserting only "the query succeeded" passes without a pool at all. |
| **Stale pooled connection** — server closes an idle connection, next query still succeeds | Missing the §6 retry. Without this the failure is intermittent and only under real idle timing. |
| **No downgrade** — upstream presents an untrusted certificate; assert the query fails **and** that a plaintext listener on the same host received nothing | A fallback path. Asserting only "it failed" passes even if a plaintext retry succeeded first. |
| **Padding** — captured query length is a multiple of 128; a plain-transport query is **not** padded | Padding applied unconditionally, or the length arithmetic off by the option header. |
| **Padding stripped** — upstream pads its reply; the returned message carries no padding option | Padding forwarded into the cache. |
| **DoH ID** — wire ID is 0, returned message carries the caller's original ID | Restoring the ID being forgotten, which breaks nothing visible until a caller matches on it. |
| **Grammar table** — every rejection in §4 by name, plus every accepted form | A parser that accepts a hostname or a mixed-scheme list, both of which fail only at runtime. |
| **Validator rejects at save** — `PUT /settings` with a malformed `upstreams` returns 400 | The validator staying `TrimSpace != ""`. |
| **Close releases connections** — swap the forwarder, assert pooled connections closed | The §6 descriptor leak, which is invisible until a long-running instance runs out. |

The existing forwarder suite is the regression proof for `plainExchanger`:
it must pass **unmodified**. A change to those tests is a signal that
plaintext behaviour moved, which this milestone does not authorise.

---

## 11. Settings, API, and UI

- **Setting:** `upstreams` — unchanged key, extended grammar (§4).
- **Validation:** `internal/api/settings_handlers.go` calls
  `upstream.ParseUpstreams` and returns 400 with the parser's message.
- **OpenAPI:** `internal/api/openapi.yaml` documents the extended grammar
  where it currently describes the comma-separated list (lines 14-16, 811).
- **UI:** the upstreams settings screen gains a **protocol selector** —
  Plain / DNS-over-TLS / DNS-over-HTTPS — and, for the encrypted options,
  presets (Cloudflare, Quad9, Google, Custom) that fill both the address and
  the verification name in one click. Camp 1's cost to the operator is a
  paste; the presets reduce the common case to a selection. Custom entries
  get two fields, address and server name, matching the two things TLS
  actually requires.
- **Copy:** states the fact and stops. "Queries to upstream resolvers are
  encrypted." Explanation of what that does and does not protect belongs in
  `docs/`, not on screen.
- **Docs:** `README.md` and the linked `docs/` tree are updated in the same
  change set — the upstream configuration page gains the grammar, the two
  camps' rationale in brief, and the "the resolver still sees your queries"
  caveat from §1.

---

## 12. RFC conformance added here

| RFC | Section | What is implemented |
|---|---|---|
| **7858** | §3 | DNS over TLS: port 853 default, TLS 1.2 minimum (Go default), certificate verification against a configured name |
| **7858** | §3.4 | Connection reuse and idle-connection management |
| **8484** | §4.1 | DNS over HTTPS: POST, `application/dns-message`, message ID 0 |
| **7830** | §3 | The EDNS(0) Padding option itself (code 12) |
| **8467** | §4.1 | Client query padding to a 128-octet block |
| **8467** | §4.2 | Response padding removed before caching or forwarding |
| **6891** | — | OPT record synthesised when padding a query that carries none |

### Deliberately not implemented, and why

- **DNS-over-QUIC (RFC 9250) and DoH/3.** Client support is thin and the
  benefit over pooled DoT/DoH on a always-on server with a warm connection
  is a handshake that has already been paid. Revisit only with a concrete
  need.
- **DNSSEC validation of upstream answers.** Orthogonal to transport, and
  deferred with the rest of DNSSEC.
- **Certificate pinning (SPKI hashes, as DNS Stamps carry).** Name
  verification against the public web PKI is the model Unbound and
  systemd-resolved use for this. Pinning adds a rotation failure mode with
  no attacker it defeats that name verification does not.
- **A bootstrap resolver.** §3. Additive later if hostname entries are ever
  wanted.
- **Per-zone encrypted forwarding.** §9.
- **Upstream response padding by dnsaur.** dnsaur is a client here. Padding
  its own responses belongs to E2, where it serves encrypted clients.

---

## 13. Task shape

Sequenced so each step ends with something independently testable:

1. **`internal/upstream/addr.go`** — the grammar: `Upstream`,
   `ParseUpstreams`, `FormatUpstreams`, the rejection table. Pure parsing,
   no transport.
2. **Consolidate the callers** — `internal/app` drops its private
   `parseUpstreams`; the API validator becomes real and rejects at save.
   Plaintext behaviour provably unchanged.
3. **The seam** — `exchanger` interface, `plainExchanger` extracted verbatim,
   `up` reshaped, `Forwarder.Close`, `swappable.set` returning the displaced
   forwarder. Existing tests pass unmodified.
4. **Padding helpers** — pad/strip, tested standalone against packed sizes.
5. **`dotExchanger` + connection pool** — including the stale-connection
   retry and its test.
6. **`dohExchanger`** — pinned dialer, zero ID, HTTP/2.
7. **Wiring and no-downgrade test** — end to end through `internal/app`
   against local DoT and DoH servers.
8. **UI, OpenAPI, and docs** — protocol selector, presets, grammar
   documentation.
