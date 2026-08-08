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

## Local DNS

Names you answer yourself, ahead of any upstream. Four record types: **A**,
**AAAA**, **CNAME**, **TXT**.

Local records are consulted after filtering and before the cache and upstream,
so a local record answers even for a name that an upstream would resolve
differently.

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
