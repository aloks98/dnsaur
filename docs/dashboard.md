# Using the dashboard

A guide to the screens in the dnsaur web UI: what each one decides, and the
rules behind the decisions that aren't obvious from looking at them.

The UI itself is deliberately terse — it states what a control does and stops.
The reasoning lives here.

Related: [`configuration.md`](configuration.md) for bootstrap YAML and env vars,
[`api.md`](api.md) for driving the same things over HTTP,
[`architecture.md`](architecture.md) for how a query actually flows through the
resolver.

---

## First run

Point a browser at the server and you get the setup wizard. It runs once: it
creates the admin account, optionally subscribes you to starter blocklists, and
then `/setup` reports that setup is complete and refuses to run again.

The blocklist step downloads and compiles each list before it moves on, so the
"Add" click sits for a moment on a slow link — that pause is the fetch, and the
entry counts you see afterwards are real, read back from the compiled lists.

Nothing here can't be changed later. Skipping the lists step leaves you with a
working resolver that blocks nothing.

---

## Dashboard

Query volume, block rate, and what's being asked for most. It's a read-only
view — every number on it is derived from the query log, so a query log privacy
setting of `none` (see [Settings](#settings)) leaves it empty going forward
while keeping the history already recorded.

---

## Query log

A live tail of queries as they're answered, streamed over SSE, plus search and
filters over what's already stored.

How much lands here is set by `qlog.privacy`, and how long it stays is set by
`qlog.retention_days` — a background pruner deletes rows older than the cutoff.
Setting retention to `0` prunes everything on the next pass.

---

## Filtering

Three screens that together decide whether a name gets answered or blocked.

### The order that matters

For any query, dnsaur walks six stages and stops at the first match:

1. **Allow rules** — exact and wildcard
2. **Allow rules** — regex
3. **Block rules** — exact and wildcard
4. **Block rules** — regex
5. **Allow lists**
6. **Block lists**

Two consequences worth internalising:

**A rule always beats a list.** All four rule stages run before either list
stage. If a list blocks something you need, you don't have to find and edit the
list — an allow rule wins, and keeps winning after the list refreshes.

**Allow beats block within the same tier.** An allow rule beats a block rule;
an allow list beats a block list. So the way to carve an exception out of a
blocklist is an allow entry, not a narrower blocklist.

If nothing matches, the query is forwarded normally.

### Lists

Subscriptions to hosted blocklists (and allowlists). Each is fetched, parsed
and compiled into a domain set; the entry count shown is what actually
compiled, not the line count of the file.

Lists refresh on a timer set by `lists.refresh_hours` — **changing that
interval needs a restart** (see [Settings](#settings)).

A list only applies to a group it's assigned to. That assignment is on the
[Groups & clients](#groups--clients) screen, not here.

**Pause** stops blocking without deleting anything — globally or for one group.
A global pause and a group pause can both be in effect; whichever ends later
wins. While paused, filtering is skipped entirely and queries forward as if no
list or rule existed.

### Rules

Your own entries, which beat every list. Four kinds, matching the stages above:
allow or block, exact/wildcard or regex.

Exact and wildcard patterns are matched case-insensitively against the
normalized name, and a pattern covers its subdomains. Regex rules are Go
regular expressions matched against the whole name; **an invalid pattern is
skipped with a warning at compile time rather than failing the whole ruleset**,
so a rule that silently does nothing is usually a regex that didn't compile.

### Groups & clients

A **group** is a filtering policy: a set of assigned lists, plus an on/off
switch. A **client** maps an IP or CIDR to a group.

Matching, when a query arrives:

1. An exact IP match wins.
2. Otherwise the **most specific CIDR** that contains the address wins
   (`/32` before `/24` before `/16`).
3. If nothing matches, the client falls into the **default** group.

That default is why a fresh install filters everything on the network without
you listing a single device — you only add clients to treat some devices
*differently*.

A group with no lists assigned filters nothing. Creating a group from the UI
assigns every existing list by default, because a group that silently protects
nothing is rarely what anyone means by "new group".

Deleting a group is refused with `409 resource in use` while clients still
point at it — move them first.

---

## Zones

An authoritative DNS zone: a suffix you hold, not a set of overrides on top
of the upstream resolver. That distinction has one concrete consequence —
**a name inside a zone you hold is never forwarded.** A name that exists
answers; a name that exists with a different type is `NOERROR` with an empty
answer (NODATA); a name that doesn't exist at all is an authoritative
`NXDOMAIN`. Either negative case carries the zone's SOA, so resolvers cache
the absence instead of asking again on every lookup. Nothing falls through
to the upstream — that's what makes split-horizon work: an internal name
under a domain you also happen to own publicly no longer leaks upstream, and
a public record can no longer shadow an internal one you haven't defined
yet.

Zones are checked before the cache and upstream, same as the old local
records were — the miss behaviour above is the whole point of the change.

Milestone A serves **primary** zones only. `secondary`, `stub` and
`forwarder` exist in the type list for zones to come but are refused on
create or edit with `400` until transfers are built. A fifth type,
`internal`, also exists — see Built-in zones below; it's never created
through this UI, only seeded.

### Records

A record's name is relative to its zone's apex: `@` for the apex itself,
`bifrost` for `bifrost.<zone>`, `*` and `*.nexus` for wildcards. Typing the
fully-qualified name works too — a trailing copy of the zone's own name is
stripped automatically, so `bifrost.home.lan` and `bifrost` land on the same
record in zone `home.lan`.

The record's value (`rdata`) is DNS presentation format — the same syntax a
zone file uses: `192.168.150.28` for an A record, `10 mail.example.com.` for
MX, `0 issue "letsencrypt.org"` for CAA. It's validated by handing it to the
same DNS parser that builds the record dnsaur serves, so a rejected value
comes back with that parser's own error text rather than a made-up message —
and any type the parser understands works without dnsaur itself changing.

**A saved value is rewritten to the server's own spelling.** dnsaur stores
the record the DNS parser produced, not the characters that were typed, so
the value shown after saving may not be the one entered: `nas.home.lan`
becomes `nas.home.lan.`, `hello` becomes `"hello"`, `2001:0db8::0001`
becomes `2001:db8::1`. The record answers the same either way. The reason
to store it this way is export: a name in rdata is read as absolute here,
but a name without a trailing dot in a zone file is *relative*, so only the
rewritten spelling still points where it did once the zone is exported and
loaded somewhere else.

The upshot is that the saved value is worth reading back — it is what the
record actually is.

**Quote TXT values.** Presentation format is not a free-text field: spaces
separate values and `;` starts a comment. Pasted raw, `v=spf1 -all` is
stored as two strings — it reads back `"v=spf1" "-all"` — and
`v=DKIM1; k=rsa; p=...` is truncated at the semicolon without any error,
reading back as just `"v=DKIM1"`. Wrapped in quotes — `"v=spf1 -all"` — the
whole thing is one value, which is what a TXT record almost always means.
Anything over 255 bytes, like a 2048-bit DKIM key, is written as adjacent
quoted strings that the reader joins back together: `"part one" "part two"`.

Three writes are refused, all `409`:

- a CNAME landing beside another record at the same name, in either write
  order (RFC 1034 §3.6.2 — a CNAME must be the only record at its name)
- a CNAME at the zone apex, where the SOA and NS already live (RFC 1912 §2.4)
- a record whose TTL disagrees with others already at that name and type
  (RFC 2181 §5.2 — one TTL per record set)

### Built-in zones

A fixed set of zones exists from the moment this server starts, always
enabled and shown as **Built-in** in the zone list: `localhost` plus every
RFC 6303 §4 reverse zone *except* the private ranges (see below) — the
exact list is [`internal/store/builtins.go`](../internal/store/builtins.go)'s
`BuiltinZones`. Without them, a query for `localhost` or a PTR lookup for
`127.0.0.1` would leave this server and go looking for an answer from the
public root and TLD servers, which don't (and shouldn't) have one. They're
type `internal` and read-only: no add/edit/delete on their records, no
renaming or deleting the zone itself — every such write is refused with
`409`.

RFC 6303 also lists the RFC 1918 private-address reverse ranges —
`10.in-addr.arpa`, `168.192.in-addr.arpa`, and `16.172.in-addr.arpa`
through `31.172.in-addr.arpa` (172.16.0.0/12 needs all sixteen, since the
range isn't byte-aligned) — and dnsaur deliberately does **not** seed
them. An empty authoritative zone answers NXDOMAIN for everything under
it, so seeding `168.192.in-addr.arpa` at install would permanently block
the one thing a homelab wants reverse DNS for: a PTR record on its own
LAN. The built-in zone would already own the whole range and refuse
every write to it, forever. The same reasoning excludes `d.f.ip6.arpa`
(RFC 6303 §4.4): `fd00::/8` is IPv6 Unique Local Addresses (RFC 4193) —
the IPv6 equivalent of RFC 1918 — and what a homelab numbers its own LAN
with if it uses IPv6 at all.

**To get PTR records for your network, create the reverse zone
yourself.** A reverse zone isn't a special kind of zone — it's an
ordinary **primary** zone whose apex happens to be the reversed network
prefix, ending in `in-addr.arpa` (IPv4) or `ip6.arpa` (IPv6). For a
`192.168.0.0/16` network, create a zone named `168.192.in-addr.arpa`; for
just `192.168.1.0/24`, `1.168.192.in-addr.arpa`. An IPv6 ULA network
works the same way — `d.f.ip6.arpa` for all of `fd00::/8`, or a more
specific zone for just your `/48` or `/64`. Create it the same way
as any other zone — the name alone is enough, SOA and apex NS are
generated same as for a forward zone. Once it exists, reverse lookups
for addresses in that range are answered from it, and — see Auto-PTR
below — adding a matching A record starts writing PTRs into it
automatically.

### Auto-PTR

Adding an A or AAAA record writes the matching PTR record automatically,
in whichever reverse zone covers that address, in the same request — no
second write, no separate step. A PTR's rdata is a domain name, not an
address (RFC 1034): the address lives in the record's *name* (reversed,
dotted, under `in-addr.arpa` or `ip6.arpa`), not in its value — the
rdata is the forward name the address belongs to.

The rules, and the ones that are easy to be surprised by:

- **It never creates a zone.** If no reverse zone covers the address,
  nothing is written — no PTR, and no error either: the forward write
  still succeeds exactly as if auto-PTR didn't exist. Create the reverse
  zone first (above) if you want the PTR.
- **First name wins.** If a PTR already exists at that address, it's
  left alone, whichever name is being added or edited now. A PTR is
  effectively one-per-address, so when two names share an address, the
  one that got a PTR first keeps it.
- **A hand-written PTR is never overwritten or deleted by the automatic
  path.** Auto-PTR only ever touches a PTR it can confirm still points
  at the forward name it's currently processing; a PTR you added
  yourself under a different name is left alone no matter what forward
  records come and go.
- **Deleting a zone retires the PTRs its records owned.** The reverse
  zone is a separate zone and outlives the delete, so its PTRs are
  removed one by one under the same "still points at this name" check —
  a PTR you wrote by hand, or one owned by a name in some other zone
  that shares the address, survives untouched.
- **Moving a name orphans its PTR.** If the name that owns a PTR changes
  address, or is deleted, that PTR is removed — and nothing takes its
  place, even if another name still resolves to the old address. The
  address is left with no PTR at all until some record is created or
  updated at that address, which then claims it under first-wins above.
  This is intentional, not a bug: first-wins only ever lets the record
  actually being written claim an address, never a bystander that
  happened to already be pointing there.

### Export and import

**Export** downloads a zone as a standard BIND master file — the format
every other DNS server reads, so the file works unchanged if you move it
there. Disabled records are left out: a master file has no way to mark one
as disabled, and including it would silently turn it on wherever the file
is read next.

**Import replaces the zone.** The file becomes the zone: anything the zone
has that the file doesn't is deleted. This is destructive and cannot be
undone, which is why choosing a file to import shows a diff first — what
would be added, changed, and deleted — and nothing is written until you
apply it.

An invalid file is rejected whole, with every problem it found named, not
just the first — no partial import, and nothing changes.

Applying an import is all-or-nothing too: if storage fails part-way
through, the zone is left exactly as it was, and the same file can be
imported again.

Import does not create PTR records the way adding an A record by hand does
(see Auto-PTR above) — it writes only to the zone being imported into. A
reverse zone gets its PTRs by importing its own file.

Built-in zones (see above) export like any other zone but refuse import,
the same `409` as any other write to one.

### Upgrading from Local DNS records

Existing local records became zones automatically the first time this
version starts. Each record was grouped under a zone by its last two
labels (`nas.home.lan` groups under `home.lan`) and rewritten to a name
relative to that apex.

**Every other name under a migrated apex stops resolving.** A local record
was an override: `nas.home.lan` answered locally and everything else under
`home.lan` went upstream. A zone is a suffix you hold, so once `home.lan`
is a zone, `anything-else.home.lan` gets an authoritative `NXDOMAIN` from
this server and is never forwarded. That is what being authoritative
means, and it is intended — it is also the single most visible change in
this release. Check the zone list after upgrading and delete any zone
whose apex you did not mean to take over.

That matters most when the inferred apex is wrong, and the last-two-labels
rule gets multi-label public suffixes wrong. A record named
`nas.home.example.co.uk` produces a zone with apex **`co.uk`** — with a
generated SOA and apex NS, exactly like any other. From then on this
server answers `NXDOMAIN` for every `.co.uk` name any client asks for.
The same goes for `com.au`, `co.jp`, and the rest. **Delete that zone**,
create one at the apex you actually hold (`example.co.uk`), and re-add the
names under it. Inferring this correctly needs a public-suffix list, which
is a dependency and a download for a conversion that runs once.

A flat table had no rules a zone now enforces, so some data was changed or
dropped in the conversion — check the server log for `WARN` lines naming
the affected records after upgrading:

- **A CNAME at the zone apex was dropped.** The apex now always carries an
  SOA and NS record, so a CNAME there is a conflict that didn't exist before.
- **A CNAME sharing a name with another record was dropped**, keeping the
  other record — losing an alias costs less than losing an address.
- **Records of the same name and type with different TTLs were normalised
  to the lowest TTL among them**, never dropped.
- **A TTL above 2147483647 was clamped** to that value (RFC 2181 §8 — a
  higher value reads back as negative, meaning "never cache").
- **A wildcard label that wasn't leftmost** (`a.*.home.lan`) **was kept
  as-is** — that's a literal name under RFC 4592, not a wildcard — but it's
  almost never what was intended, so it's logged.
- **TXT values were quoted**, so what you stored is what still gets served
  — a local record held its value literally, a zone record holds
  presentation format, and copying one into the other unquoted would have
  truncated every DKIM key at its first `;`. A value over 255 bytes was
  split into adjacent quoted strings (RFC 1035 §3.3.14); readers join them
  back together, and such a value could not be served at all before.

---

## Settings

Server-wide configuration, stored in the database. Most of it applies the
moment you save.

| Group | What it sets |
|---|---|
| **Upstreams** | Where queries are forwarded, and how those upstreams are chosen |
| **Blocking** | How a blocked query is answered, and for how long that answer may be cached |
| **Cache** | TTL floor/ceiling, size, and stale-serving |
| **Query log** | How much per-query detail is recorded, and for how long |
| **Lists** | How often subscriptions refresh |

**Upstream strategy** is one of:

- `race` — ask all of them at once, take the first good answer
- `fastest` — ask the one with the best measured latency
- `failover` — ask in the order listed, falling through on failure

**Blocking mode** is one of:

- `null-ip` — answer `0.0.0.0`, an unroutable address
- `nxdomain` — answer `NXDOMAIN`, "this name doesn't exist"

**Query log privacy** is one of:

- `full` — the client IP as seen
- `anon` — last octet masked (`192.168.11.104` → `192.168.11.0`)
- `none` — nothing recorded; history already stored is kept

### What needs a restart

Everything hot-reloads **except** the five settings read once at startup:

- `cache.min_ttl`, `cache.max_ttl`, `cache.max_entries`, `cache.serve_stale_for`
- `lists.refresh_hours`

The UI marks these, and the save bar tells you when a save contains any of
them. They're saved immediately either way — they just don't take effect until
the process restarts.

---

## TSIG keys

A TSIG key (RFC 8945) authenticates a zone transfer: both ends hold the same
named key and sign every transfer message with it. Its own tab under System,
beside Settings and Account.

Each key is a name, an algorithm and a base64 secret.

**The name is canonicalised on save** — lowercased and given a trailing dot,
so `XFER.e412.IN` is stored as `xfer.e412.in.`. The create row says what it
will be saved as while you type it. That form has to match the name the peer
signs with, which is why the list shows canonical names rather than what was
typed.

**The algorithm** is one of `hmac-sha1`, `hmac-sha224`, `hmac-sha256`,
`hmac-sha384` or `hmac-sha512`. `hmac-md5` is a real RFC 8945 name but isn't
offered: the library dnsaur signs with dropped MD5, so a key made with it
could never sign anything.

**Generating the secret is the default** and the create row opens with one
already filled in — 32 bytes of browser CSPRNG output, base64-encoded. The
server can check that a secret is base64; it cannot tell a strong one from a
weak one, so a hand-typed secret is the risky path. "Paste an existing
secret" is there for the case that matters: the peer already holds a key and
dnsaur is the side being configured to match.

**Secrets are shown, not hidden.** Unlike an API token, a TSIG key's secret
is returned on every read and stored in plaintext — it has to be pasted
unchanged into the matching key on the other server (BIND's `key{}` clause,
Technitium's transfer settings), so a write-only secret would make the
feature unusable. The list masks them anyway, so four secrets aren't sitting
in the open during a screen share: the eye reveals one row at a time, and
Copy puts the real secret on the clipboard without revealing anything. That
is a display choice, not a security boundary — anyone who can reach this
screen can read every secret on it, and anyone who can read the database
file can too.

Changes take effect on the next signed message; nothing here needs a
restart.

Nothing references a key yet. Zones carry a `tsig_key_id` field, but no zone
can be pointed at a key and no transfer runs against one — that arrives with
zone transfers, along with the "in use by N zones" column and the guard that
stops you deleting a key some zone still depends on. Today, deleting a key
just deletes it.

---

## Account & security

### Password

Changing your password from the UI isn't built yet. The section is there and
marked as planned.

### Two-factor authentication

TOTP, enrolled by scanning a QR code (or typing the secret) and confirming one
code. Once on, login asks for a code after the password.

There is no recovery flow and there are no backup codes. Keep the secret
somewhere you can reach without this server — losing it means losing the
account.

### API tokens

Bearer tokens for scripts and other tools. Send as
`Authorization: Bearer <token>`; a bearer token wins over a session cookie if
both are sent.

Two scopes:

- **`read`** — `GET` and `HEAD` only. Anything else is refused with
  `403 read-only token`.
- **`write`** — full access, the same as your own session.

The plaintext token is shown **once, at creation**, and is never recoverable —
only a hash is stored. If you lose it, revoke it and make another.

Tokens don't expire. Revoking is immediate.
