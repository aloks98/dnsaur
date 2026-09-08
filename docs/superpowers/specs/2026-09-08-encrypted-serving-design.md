# dnsaur Encrypted Serving (DoT/DoH): design

**Status:** design, approved for implementation 2026-09-08
**Scope:** the *serving* half of encrypted DNS — clients → dnsaur.
**Companion:** `docs/superpowers/specs/2026-09-05-encrypted-upstreams-design.md`
(E1, shipped), which deferred this half and recorded why in its §2.

---

## 1. Why

Queries from a phone or a laptop to dnsaur travel in plaintext UDP on port
53. Everything upstream of dnsaur is now encrypted (E1); this last hop is
not.

On a home LAN behind a tunnel that is a smaller problem than the upstream
hop was, and this design does not pretend otherwise. What it buys is real
but modest:

- **Other devices on the same network stop seeing the queries.** A LAN is
  not a private channel — a compromised IoT device, a guest, or anything
  on the same segment can observe plaintext DNS.
- **Encrypted DNS is a device setting, not a network setting.** Android's
  Private DNS holds a hostname and follows the device onto other networks;
  it does not depend on the router handing out the right resolver.
- **It closes the loop.** An operator who configured encrypted upstreams
  reasonably assumes their DNS is encrypted. Today half of it is.

It is not a defence against an attacker who already controls the local
network, and it hides nothing from dnsaur itself.

---

## 2. Scope: what the deployment decision deleted

E1 deferred this half because of one problem: `Request.ClientIP` comes from
`w.RemoteAddr()` (`internal/dnssrv/server.go`) and drives per-client
filtering through `clients.Registry.Lookup`. Behind a TLS-terminating
reverse proxy every client collapses into the proxy's address, so filtering
silently applies the wrong group and the query log attributes everything to
one IP. Recovering identity needs the PROXY protocol for DoT,
`X-Forwarded-For` for DoH, and a trusted-proxy allowlist so neither can be
spoofed.

**dnsaur terminates TLS itself, so none of that is needed.** Nothing sits
between the client and dnsaur on the DNS path. Client addresses arrive
real, `clients.Registry.Lookup` keeps working unchanged, and the whole
identity subsystem is out of scope.

| | In | Out |
|---|---|---|
| Transports | DoT (RFC 7858), DoH (RFC 8484) | DoQ (RFC 9250), DoH/3 |
| TLS | terminated by dnsaur | terminated by a proxy |
| Identity | `w.RemoteAddr()`, unchanged | PROXY protocol, `X-Forwarded-For`, trusted-proxy ACL |
| Certificates | read from a path, reloaded on change | ACME client, certificate upload |
| Access control | the network the listener is on | rate limiting, secret paths, tokens |

**DoQ is deferred deliberately.** Client support is thin, it needs a
heavyweight QUIC dependency, and against an always-on resolver with a warm
connection it saves a handshake that has already been paid. Revisit when
something the operator owns actually speaks it.

**If a proxy is ever put in front of the DNS path, this section is what to
re-open** — not the transports, which are indifferent to it, but identity.

---

## 3. The deployment this is designed for

Recorded because several decisions below only make sense against it.

`e412.in` is split-horizon: public records at Cloudflare, internal records
served by dnsaur itself. The router hands out dnsaur's **local** addresses
as resolvers, and the WireGuard and OpenVPN tunnels push the same
addresses. So every client — LAN or tunnel — already reaches dnsaur at a
private address, and will reach the encrypted endpoints the same way.

The admin UI is fronted by a reverse proxy (`waygates`) **on a different
host**, at `admin.<instance>.dns.e412.in`. The DNS endpoints are
`<instance>.dns.e412.in` and are served by dnsaur directly. Because the
proxy is on another machine it never touches DNS traffic — and it cannot
share its certificate store, which is why §4 looks as it does.

### The bootstrap chain, which works

A DoT client is configured with a **hostname**, not an address — Android's
Private DNS accepts nothing else. So:

1. The device resolves `adam.dns.e412.in` using the resolver the network
   handed it, which is dnsaur on plain port 53.
2. dnsaur answers with its own private address, because the split-horizon
   zone says so.
3. The device opens DoT to that address and validates the certificate
   against the hostname.

**Plain DNS bootstraps encrypted DNS**, which is E1's upstream bootstrap
problem mirrored — and it resolves the same way, by something already
working answering the first question. The documentation consequence: the
DoT/DoH hostname **must resolve to the listener's address for internal
clients**, and dnsaur is the thing that makes that true.

---

## 4. Certificates

```
serve.tls.cert  /etc/letsencrypt/live/adam.dns.e412.in/fullchain.pem
serve.tls.key   /etc/letsencrypt/live/adam.dns.e412.in/privkey.pem
```

Loaded through a `tls.Config.GetCertificate` callback that re-reads the
pair when either file's mtime changes and serves a cached keypair
otherwise. A renewal is therefore picked up on the next handshake, with no
restart and no settings change.

**How the file arrives is out of scope.** certbot on the dnsaur host, an
rsync from elsewhere, a manual copy — all identical from dnsaur's side, and
the mtime reload is what makes every one of them work. This is deliberately
not a certificate-management feature.

**DNS-01 is the only usable ACME challenge here**, because the hostname
resolves to a private address for internal clients and Let's Encrypt cannot
reach it for HTTP-01 or TLS-ALPN-01. Cloudflare holds the public zone, so a
DNS-01 plugin is straightforward. This belongs in the documentation, not in
the code.

### Two failure modes worth designing for

**Permissions.** certbot writes `privkey.pem` as `0600 root:root`. dnsaur
binds ports 53 and 853, so it runs as root or with `CAP_NET_BIND_SERVICE` —
and in the second case **it cannot read the key**. Save-time validation
(§7) catches this with a message naming the file, rather than a failed bind
at three in the morning. The fix is a certbot `--deploy-hook` adjusting
ownership; that goes in the docs.

**Silent delivery failure.** A stopped timer, a hook that stopped firing
and a forgotten manual copy all look identical: the certificate simply
stops being renewed. dnsaur **warns 14 days before expiry** (§8), which
catches every delivery mechanism because it inspects the artefact rather
than the process.

---

## 5. The transports

Both reuse machinery that already exists, and neither is visible above the
handler interface.

### DoT

`dns.Server` over a `tls.Listener` — the exact shape E1's own test fixtures
already use (`startDoT` in `internal/upstream/tlstest_test.go`).
`internal/dnssrv` gains a TLS mode that binds **TCP only**: there is no UDP
sibling for DoT, and `Server.Start`'s current retry loop assumes a pair.

Everything above the socket is unchanged — the TSIG provider, the transfer
and NOTIFY intercepts, and the whole handler pipeline never knew what
carried the bytes.

### DoH

A small `http.Server` with its own TLS config and its own listener, serving
RFC 8484 at `/dns-query`:

- `POST` with `Content-Type: application/dns-message`, the wire-format
  query as the body.
- `GET` with `?dns=<base64url, unpadded>`.

Anything else is a 405 or a 400. Responses carry
`Content-Type: application/dns-message`.

**The request body is bounded at `dns.MaxMsgSize`.** It arrives from a
client, and an unbounded read is a trivial denial of service against a
resolver that is otherwise unauthenticated on its own network.

**HTTP/2 must be configured explicitly.** Go wires h2 automatically only
through `ListenAndServeTLS`/`ServeTLS`; a hand-built `tls.Listener` handed
to `Serve` negotiates HTTP/1.1 unless `NextProtos` includes `h2` and the
server is configured for it. Most DoH clients expect h2, and the failure is
silent — everything works, more slowly, over one connection per query.

**Its own listener, not the admin server's.** Mounting DNS on the admin
`http.Server` would put `waygates` in the DNS path and undo §2 — every
client would arrive as the proxy. The separation is the design, not a
tidiness preference.

### Response padding

RFC 8467 §4.2: when a query carries an EDNS(0) Padding option, the response
is padded to a multiple of **468 octets** (the server profile; E1 pads
queries to 128). Applied on the encrypted transports only — padding a
plaintext reply hides nothing.

**`padQuery` and `stripPadding` move from `internal/upstream` into
`internal/dnssrv`.** `internal/upstream` already imports `internal/dnssrv`,
so the dependency direction is right, and E1 proved the off-by-four trap in
that arithmetic — two copies would drift. The block size is already a
parameter.

---

## 6. The listener reconciler

The only genuinely new subsystem, and the price of runtime toggling.

Listeners today are built once in `App.Start` and live in `a.servers`.
Under this design the desired set is a function of settings, so a
**reconciler** owns the active set and, on every settings change, diffs
desired against actual:

- stop listeners no longer wanted,
- start listeners newly wanted,
- **leave unchanged listeners strictly alone.**

That last rule is load-bearing: toggling DoH must not drop live DoT
connections, and a settings write unrelated to serving must not disturb
either.

**An address change is a stop followed by a start**, not the reverse — the
new listener usually cannot bind while the old one holds the address. That
leaves a brief window with nothing listening, which is correct and
unavoidable. If the start then fails, the reconciler **reports it and
leaves the protocol down**; it does not silently revert to the previous
address, because a listener serving an address the settings no longer name
is a worse lie than one that is honestly off.

Plain `:53` listeners stay outside the reconciler, built from `dns_listen`
in config as they are today. They are not settings-driven, and making them
so is not in scope.

### Two failure classes, two mechanisms

- **Knowable at save time** — the address does not parse, no certificate is
  configured, the certificate or key is unreadable, the key does not match
  the certificate. **Rejected at save with the reason** (§7), the pattern
  E1 established for `upstreams`.
- **Knowable only at bind time** — the port is already in use, or
  permission is denied. Ports 853 and 443 are privileged, so this will
  happen. Reported through status and a banner (§8).

---

## 7. Settings and validation

```
serve.dot.enabled   bool     default false
serve.dot.listen    string   default ":853"
serve.doh.enabled   bool     default false
serve.doh.listen    string   default ":443"
serve.tls.cert      string   default ""
serve.tls.key       string   default ""
```

Listen addresses default to binding **all interfaces**, matching
`dns_listen`'s existing `:53`. An explicit address is accepted for a host
that needs one.

### Per-key validation

`serve.*.listen` must split into a host and a numeric port in range.
`serve.*.enabled` must be `true` or `false`. `serve.tls.*` must be an
absolute path or empty.

### Cross-field validation, which the current shape cannot express

`editableSettings` is `map[string]func(string) error` — **per key**. But
"enable DoT" is only valid if a certificate and key are already configured
and loadable, which a per-key validator cannot see.

So `handleSettingsPut` gains a cross-field check that reads the current
values from the store before accepting the write. Enabling either protocol
requires `serve.tls.cert` and `serve.tls.key` to be set, readable, and to
form a valid pair under `tls.LoadX509KeyPair`.

**Consequence: order matters.** Certificate paths must be saved before a
protocol is enabled. The rejection message must say so rather than merely
refusing — "set serve.tls.cert and serve.tls.key first" is actionable;
"invalid value" is not.

---

## 8. Status and warnings

E1 built the pattern: `GET /api/v1/resolver/status` (authenticated), backed
by the `api.ResolverStatus` interface that `internal/app` implements, with
a `WarningStrip` banner mounted in the app shell.

This milestone **extends that interface** rather than adding a second one.
Two new facts:

- **A protocol is enabled but not listening**, with the bind error,
  **reported per protocol** — DoT and DoH fail independently, and one
  status covering both would leave the operator unable to tell which is
  down. This is the checkbox lying, and it is exactly what the downgrade
  banner exists to prevent: intent is not reality.
- **The certificate expires within 14 days**, with the date. The threshold
  is a constant, not a setting: it exists to catch a delivery mechanism
  that stopped working, and an operator who can tune it can also tune it
  to never fire.

Both clear on their own — the first when a later reconcile binds
successfully, the second when a renewed certificate is loaded.

The Settings screen shows, per protocol, a checkbox (intent) **and**
whether it is actually listening (reality). A checked box beside "not
listening: address already in use" is honest; a checked box alone is not.

---

## 9. What does not change

- **The handler pipeline.** Filtering, cache, query log, zones, TSIG,
  transfers and NOTIFY behave identically, because none of them knew which
  transport carried the query.
- **Client identity.** `Request.ClientIP` still comes from
  `w.RemoteAddr()`; `clients.Registry.Lookup` is untouched.
- **Plain `:53`.** Still served, still configured by `dns_listen`.
- **Encrypted upstreams.** E1 is orthogonal — a query may arrive over DoT
  and leave over DoH, or arrive in plaintext and leave over DoT.

---

## 10. Test posture

Real servers and real TLS, no mocks — a `dns.Client{Net: "tcp-tls"}` and an
`http.Client` against listeners the tests start.

**E1's `testCertFor` moves into a small importable package** (a normal
package, not a `_test.go` file) so both `internal/upstream` and
`internal/dnssrv` can mint self-signed certificates. Helpers in `_test.go`
files cannot cross package boundaries, and duplicating certificate minting
is how two copies diverge.

| Test | The implementation it catches |
|---|---|
| **Client identity over DoT** — a DoT client's real address reaches `Request.ClientIP` and resolves the right group | The entire reason direct termination was chosen. Nothing else in the suite would notice if it broke. |
| **DoT and DoH answer identically to plain** for the same query | A transport wired below the pipeline instead of into it |
| **DoH `POST` and `GET ?dns=`** both accepted, other methods rejected | Half of RFC 8484 |
| **Certificate reload** — replace the files, the next handshake serves the new certificate, no restart | A keypair read once at startup, so renewal silently breaks in 90 days |
| **Reconcile leaves unchanged listeners alone** — toggle DoH, assert a live DoT connection survives | A reconciler that rebuilds everything on any settings write |
| **Bind failure is reported, not just logged** — occupy the port, enable, assert status says so | The checkbox lying |
| **Save-time rejection table** — each §7 rejection by name | Enabling with no certificate, which then fails at bind |
| **Response padded only on encrypted transports**, and only when the query was padded | Padding applied unconditionally |
| **Expiry warning fires at the threshold** | A warning that never fires, which is indistinguishable from no problem |

---

## 11. RFC conformance

| RFC | Section | What is implemented |
|---|---|---|
| **7858** | §3 | DNS over TLS: port 853, TLS 1.2 minimum, server side |
| **8484** | §4.1 | DNS over HTTPS: `POST` and `GET`, `application/dns-message` |
| **7830** | §3 | The EDNS(0) Padding option |
| **8467** | §4.2 | Response padding to a 468-octet block when the query was padded |

### Deliberately not implemented

- **DNS-over-QUIC (RFC 9250) and DoH/3** — §2.
- **PROXY protocol and `X-Forwarded-For`** — no proxy on the DNS path. §2
  records what to re-open if that ever changes.
- **ACME** — dnsaur reads a certificate; it does not obtain one. §4.
- **Certificate upload** — it would break renewal and put private key
  material in the settings table, which `handleSettingsGet` returns
  wholesale. The `GetCertificate` seam accepts a different source later
  without reshaping anything.
- **Rate limiting and access tokens** — the listener's network is the
  boundary. Re-open if an endpoint is ever exposed publicly.

---

## 12. Task shape

1. **Padding moves** to `internal/dnssrv` with the block size as a
   parameter; `internal/upstream` calls it. A pure refactor, and the
   existing tests are its proof.
2. **`internal/certtest`** — `testCertFor` extracted into an importable
   package, `internal/upstream`'s tests switched to it.
3. **DoT listener** — `internal/dnssrv` gains a TCP-only TLS mode, with the
   client-identity test.
4. **Certificate loading** — the mtime-watching `GetCertificate`, with the
   reload test.
5. **DoH handler and listener** — RFC 8484, `POST` and `GET`.
6. **Response padding** on the encrypted serving paths.
7. **Settings, per-key and cross-field validation** — including the
   ordering message.
8. **The reconciler** — desired-vs-actual, with the leave-alone test.
9. **Status and banners** — extend `ResolverStatus`, bind failure and
   expiry.
10. **UI** — the Protocols group: intent beside reality.
11. **Docs** — the bootstrap requirement, DNS-01, the key-permissions hook.
