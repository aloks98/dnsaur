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

The **q/s** figure beside Live queries is the one number here that isn't from
the stats tables: it counts rows arriving on the live stream over the last 30
seconds, so it appears only once the page has been watching for ten seconds and
falls back to zero when the stream goes quiet. The page of history the feed
opens with doesn't count towards it.

The **query volume** chart is hourly whatever the window, and the window's
oldest hour is only partly covered — stats buckets are whole hours, so the
server drops that one rather than reporting a fraction of it. A 24-hour window
is therefore 24 bars, not 25, and a 1-hour window is the current hour alone.

---

## Query log

A live tail of queries as they're answered, streamed over SSE, plus search and
filters over what's already stored.

The screen opens with the most recent 100 queries from storage, so a busy
resolver doesn't start on an empty table, and keeps the last 500 rows in view.

**Pause** holds the stream: what's on screen stays, and nothing new is added
until you resume. It's a hold, not a saved view — leaving the screen resumes
it, so coming back always shows a running tail.

Selecting a row explains it in the rail on the right. For a blocked row that a
list decided, the rail names the list and the entry in it that fired, and
**Allow this entry** writes an allow rule for exactly that entry — which
un-blocks everything under it, not just the name in the row. Rows logged before
the upgrade that added this have no entry recorded and show none.

If the stream drops, it reconnects on a widening delay and gives up after about
half a minute, offering **Reconnect**. Giving up also re-checks your session,
since an expired one looks exactly like a dead connection from here: if it has
expired you get the login screen rather than a button that can't work.

How much lands here is set by `qlog.privacy`, and how long it stays is set by
`qlog.retention_days` — a background pruner deletes rows older than the cutoff.
Setting retention to `0` prunes everything on the next pass.

The **client** filter matches the stored `client_ip` exactly, so it is
unavailable unless `qlog.privacy` is `full`: `anon` masks the last octet on the
way in and `none` records nothing, and in both cases no client's address could
ever match. The **hostname** column keeps working either way — it resolves
through the row's client id, not its address.

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

**A blocklist's own exceptions apply to that list only.** A block list may
carry `@@||name^` lines — the list author's carve-outs from their own
entries. Those are honoured at stage 6: a name a list exempts is not blocked
*by that list*. It is not promoted to a global allow, so another blocklist
that names it still blocks it, and a block rule of yours still wins. If you
want a name allowed everywhere, use an allow rule.

If nothing matches, the query is forwarded normally.

### Lists

Subscriptions to hosted blocklists (and allowlists). Each is fetched, parsed
and compiled into a domain set; the entry count shown is what actually
compiled, not the line count of the file.

Lists refresh on a timer set by `lists.refresh_hours` — **changing that
interval needs a restart** (see [Settings](#settings)). Each row says when it
was last checked and how long until the next timed run, so a list that is
about to fix itself is distinguishable from one that has been failing for a
week. Downloading and compiling are separate: editing a rule, a list or an
assignment recompiles from the copies already on disk and answers
immediately, while **Refresh all**, a row's own **Refresh now**, adding a
list, enabling one and the timer are what actually fetch. A
restart compiles from those copies before it starts answering queries, so the
network is filtered from the first query rather than from the end of the
first download.

Two limits apply to a fetch, and a list that hits either is reported
`failed` with the reason on its row:

- **64 MiB per list.** The body is written into the data directory and then
  turned into a domain set, so an endless response is refused rather than
  filling the disk.
- **Public addresses only.** A list URL is yours to choose but the server is
  what fetches it, from inside your network — so a URL (or a redirect) that
  resolves to loopback, a link-local address such as `169.254.169.254`, or a
  private range like `10.0.0.0/8` is refused. Host list files on something
  reachable from the internet; there is no local-file source.

A list only applies to a group it's assigned to. That assignment is on the
[Groups & clients](#groups--clients) screen, not here.

**Pause** stops blocking without deleting anything — globally, for one group,
or for one device. Pauses at different levels can all be in effect at once;
whichever ends later wins, so a device pause can outlast its group's and a
group pause can outlast a device's. While paused, filtering is skipped
entirely and queries forward as if no list or rule existed.

Resume clears only the pause you resume. A device showing a countdown its
group set says **Paused for this group** and offers no resume, because
resuming the device would leave the group's pause running.

Pauses survive a restart. One that ends while the server is down does not
come back with it.

### Rules

Your own entries, which beat every list. Four kinds, matching the stages above:
allow or block, exact/wildcard or regex.

Exact and wildcard patterns are matched case-insensitively against the
normalized name, and a pattern covers its subdomains. A pattern has to be a
domain — `example.com`, `*.example.com` (stored as `example.com`, which
already covers the subdomains) or a single label like `localhost`. Anything
else is refused at 400: `||example.com^`, `ads.*.example.com` and a bare `*`
are patterns no query can ever carry, so storing them would create a rule
that silently matches nothing. Unicode names are converted to punycode, since
that is the form a query arrives in.

Regex rules are Go regular expressions matched **unanchored** against the
lowercased name — a rule `ads` matches `roads.example` too, so anchor with
`^`/`$` when you mean the whole name. **An invalid pattern is skipped with a
warning at compile time rather than failing the whole ruleset**, so a rule
that silently does nothing is usually a regex that didn't compile.

Go syntax is what counts, including the parts your browser's own regex engine
has no equivalent for — inline flags like `(?i)ads` and the `(?P<name>…)`
capture spelling are both accepted.

### Groups & clients

A **group** is a filtering policy: a set of assigned lists, plus an on/off
switch. A **client** maps an IP or CIDR to a group.

Matching, when a query arrives:

1. An exact IP match wins.
2. Otherwise the **most specific CIDR** that contains the address wins
   (`/32` before `/24` before `/16`).
3. If nothing matches, the client falls into the **default** group.

A matcher is stored in the one spelling that matching uses: a CIDR is masked
(`10.0.0.1/24` is saved as `10.0.0.0/24`) and an IPv4-mapped IPv6 form is
unmapped (`::ffff:192.168.1.0/120` becomes `192.168.1.0/24`), so what the row
shows is what will be compared. An address carrying an interface zone
(`fe80::1%eth0`) is refused: the request side never carries one, so such a
matcher could only ever match nothing.

That default is why a fresh install filters everything on the network without
you listing a single device — you only add clients to treat some devices
*differently*.

A group with no lists assigned filters nothing. Creating a group from the UI
assigns every existing list by default, because a group that silently protects
nothing is rarely what anyone means by "new group".

Deleting a group is refused with `409 resource in use` while clients still
point at it — move them first. The screen says so up front: Delete is
unavailable on a group with clients, and the row says how many to move.

Every group row and every client row carries the same **Pause** control the
top bar does, for that group or that one device. See [Lists](#lists) above
for how the levels combine.

Disabling a group stops filtering for every client in it. For the **default**
group that is every device you haven't pinned somewhere else, not just the
clients listed under it.

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

Four types can be created, and they split in two. **Primary** (a zone you
author here) and **secondary** (a copy of one held elsewhere) hold records
and answer from them. **Forwarder** and **stub** hold nothing: they claim
the suffix and send every query beneath it somewhere else — see Forwarder
zones and Stub zones below. A fifth type, `internal`, also exists — see
Built-in zones below; it's never created through this UI, only seeded.

The type is chosen in the create row at the top of the zones list, and the
one field beside it changes with it: **Primary servers** for a secondary or
a stub, **Forward to** for a forwarder, nothing for a primary. A secondary
or a stub may also name a TSIG key there; a forwarder may not, because it
signs nothing.

Each row's name is a link into that zone's own page, and everything else it
can be told to do is behind the **⋮** at the end of the row: **Disable** (or
**Enable**), **Delete zone**, and — on a secondary or a stub that is not
pulling cleanly — **Retry transfer** or **Fetch now**. Disabling takes
effect at once and is one click to undo, so it isn't confirmed; deleting is.
A built-in zone has no menu at all: its row says `BUILT-IN` instead.

### Secondary zones

A secondary is not yours to edit. It is pulled whole from another server
over AXFR and re-pulled on the schedule that server's SOA asks for, so the
records, the SOA and the serial are all its primary's. The zone page
reflects that: no **Add record**, no per-row edit or delete, no **Import**,
and the SOA form is replaced by a transfer band. **Export** stays, because
reading a secondary is fine.

Creating one needs **Primary servers** — one or more `host[:port]`,
comma-separated, port 53 assumed — and optionally a **TSIG key** to sign the
transfer requests with (see the TSIG keys page). A hostname is stored as
written and resolved at transfer time, so a primary that moves keeps
working.

A hostname is resolved through the upstreams on the settings page, not
through whatever this machine resolves through. Two consequences worth
knowing: the lookup is encrypted if those upstreams are, and a primary named
*inside the zone it serves* — `ns1.corp.example` for `corp.example` — works,
because it is answered from the upstreams rather than from the zone that has
nothing in it yet.

The transfer band across the top says how current the copy is: the serial it
last adopted, when it last refreshed and when it next will. **Refresh now**
pulls immediately rather than waiting for the schedule.

Underneath it, **Primaries** is where the zone pulls from, with the TSIG key
it signs with beside it — both editable in place. A primary that changes
address is one edit, not a delete and a rebuild: recreating the zone would
take its `allow_transfer` and `notify_to` with it. Clearing the field is
refused, here and by the server: a secondary with nowhere to pull from
claims its suffix and answers SERVFAIL for it forever.

Status on a secondary is not the same question as enabled/disabled, and the
list says so:

- **Refreshed 2h ago** — transferred within its schedule.
- **Transfer failing** — the last attempt failed. A second line inside the
  same status cell dates the attempt and gives the failure in the server's
  own words.
- **Serving, transfer overdue** — its refresh came and went with nothing
  recorded against it. Usually the zone is disabled, so nothing is trying.
- **Not answering** — either it has never transferred, or it is past the
  SOA expiry. Both mean the same thing: it holds nothing it can vouch for,
  so it answers `SERVFAIL` for its whole suffix rather than claiming names
  don't exist.

A row in either of the last two states is marked at its edge and tinted, and
**hovering its status** names the state — `NEVER TRANSFERRED`, `EXPIRED`,
`LAST TRANSFER` — and says what it means for what is being answered: which
names go unanswered, or which copy is still being served and how old it is.
The hover is the explanation only; everything that says the row needs a
human is on the row itself. **Retry transfer**, in the row's own actions
menu, asks for a transfer now rather than waiting for the schedule.

The failure reason is stored, not merely remembered, so restarting dnsaur
does not make a zone that has been failing for a week look healthy. It is
always shown with when the attempt was made — "connection refused" four
minutes ago and the same words four days ago are not the same situation.

A transfer error is one long line from the server, and it usually repeats
the zone's name and the primary's address before getting to what actually
went wrong. So the zone page leads with the end of it — the failure itself —
and prints the whole message under it, dim, hover for any of it that doesn't
fit. The zones list shows the same short form with the full text on hover.
Nothing is rewritten: the short form is a piece of the message, not a
paraphrase of it, and a message with no such shape is shown whole.

None of this needs a reload. A zone that pulls from a master — a secondary,
or a stub — is the one thing here that changes with nobody touching it, so
while one is on screen the zones list and the zone page re-read it every 30
seconds — the same tick the scheduler decides what is due on, so the screen
is never more than one of its decisions behind — and again when you come
back to the tab. A zone that recovers, starts failing or crosses its expiry
says so on its own. The record list is not on a timer: it is re-read when a
transfer actually lands, which is the only thing that changes it. A screen
with no secondary and no stub on it does not poll at all — a primary's
records and a forwarder's upstreams change only when you change them.

### Forwarder zones

A forwarder holds no records at all. It claims a suffix and sends every
query beneath it to the resolvers named in **Forward to** — a
comma-separated list of `host[:port]`, port 53 assumed — instead of to the
upstreams in Settings. That is what makes it a zone rather than a setting:
one row claims the suffix, and there is nowhere else it can be claimed from.

**A forwarder's page is short, and that is not a page that failed to load.**
It carries the header, the **Forward to** row, and the zone actions. There
is no SOA band, no records grid, no import or export, no allow-transfer and
no notify row, because a forwarder has nothing to put in any of them. There
is no **Refresh now** either: a forwarder has no master, so there would be
nothing to fetch.

The line on the page under the upstreams says the thing worth knowing:

> This zone claims `corp.example` outright. With every upstream unreachable,
> queries for it get SERVFAIL — they do not fall through to the default
> resolvers.

**That is deliberate, and it is the most surprising thing about the type.**
A forwarder is not an override with a fallback. If the resolvers you named
are down, or you saved the zone without naming any, every name under that
suffix stops resolving instead of being answered by the public internet.
For a split-horizon zone the fall-through would be the failure: an internal
name would silently resolve to whatever the outside world says it is.
[`architecture.md`](architecture.md#conditional-routing-forwarder-and-stub-zones)
has the full reasoning.

Two ways out of that state: fix the upstreams, or **disable the zone**. A
disabled forwarder releases its suffix back to the upstreams in Settings,
the same way a disabled primary gives its names back to the public answer.
Either takes effect on the zone at once, but a name that already got a
SERVFAIL keeps getting one for up to 30 seconds afterwards — failures are
cached per name and type (RFC 9520) — so give it half a minute before
concluding the fix didn't work.

In the zones list a forwarder's status reads **Enabled**, and means only
that. Whether its upstreams are answering is true or false *right now* and
nothing about it is stored, so the list has nothing to report and does not
invent it — hovering the status says as much. A forwarder gets no failure
line, no fetch state and no retry action anywhere on the list.

Answers routed this way are cached exactly like any other forwarded answer,
and they show in the query log as **forwarded**, with the conditional
upstream's address — not as `authoritative`, because no zone answered them.

**Saving the zone clears what the cache already held for that suffix**, and
so does disabling, deleting or retargeting it. The name you are claiming is
usually one that resolves publicly right now — that is why you are claiming
it — so without this the public answer would go on being served from the
cache for its whole TTL, and longer still if the resolvers you named were
also down. The clearing is scoped to the suffix whose routing changed, so
editing records elsewhere does not cost you the cache.

What is *not* cleared is a suffix's own answers when nothing about its
routing changed. If the resolvers you named go down after they have been
answering, queries for names they already answered are served from the cache
past their TTL, marked at 30 seconds, for as long as **Serve stale for** in
Settings allows — the same rule as any other forwarded name, and the answer
is still your resolvers' own, never the public internet's.

### Stub zones

A stub claims a suffix the same way a forwarder does and routes it the same
way. The difference is where the addresses come from: it **fetches** them.
It asks its master two ordinary questions — the zone's SOA, for the serial
and the schedule, and its NS records, with the nameservers' addresses in the
reply — and routes to the nameservers that come back, re-asking on the
schedule that SOA publishes. The master is typed into **Primary servers** in
the create row, and the zone page calls the same field **Primaries** —
editable there, with the TSIG key beside it.

**It is not a transfer**, and that is the reason to use one: the master
needs no `allow-transfer` entry for dnsaur, so a stub works against a server
that will not hand its zone to anybody. A TSIG key can still be named, for a
master that requires signed queries.

A stub's page drops the same bands a forwarder's does — no SOA, no create
row, no record filter, no allow-transfer, no notify — but keeps two things
a forwarder has no use for. The **records grid** stays, read-only, because
the fetched NS set is worth seeing, and the header marks it
**Fetched · Read-only**. And because a stub has a master, **Refresh now**
fetches immediately rather than waiting for the schedule, and **Export**
renders the NS set as a zone file. A forwarder has neither: nothing to
fetch, and no records to export.

Three states. Two carry a date, because a date is what makes them mean
anything: **fetched *n* minutes ago**, and, on a failure, **the error in the
server's own words under when it was attempted**, with what is still being
served beside it. The third — **no NS set yet** — has no date and needs
none: nothing has landed for a date to be about, and the primaries row above
it already carries the failure's own date if there has been one.

In the zones list a stub reads the same column a secondary does, in the
words its own mechanism needs: **Fetched 5d ago**, **Fetch failing** — the
set it already has is still being routed on — or **Not answering** when
nothing has ever landed. That last one is the state worth spelling out. **A
stub that has never fetched is not idle, it is a suffix-wide outage**: it
holds no NS set, keeps its claim regardless, and answers `SERVFAIL` for
every name beneath it. The list refuses to call that Enabled. Hovering the
status names the state (`NEVER FETCHED`, `LAST FETCH`) and says what it
means; the status cell dates the last attempt underneath and shows what the
master said. **Fetch now**, in the row's actions menu, asks for one
immediately.

**Expired never appears on a stub**, in the list or anywhere else — see the
third rule below. A zone retyped from secondary keeps the expiry stamp its
last transfer wrote, and that number stops meaning anything the moment the
type changes.

Four rules are worth knowing before creating one:

- **A nameserver inside the zone must arrive with its address.** For zone
  `corp.example`, a nameserver called `ns1.corp.example` can only be reached
  if the master sends its address alongside (DNS calls this *glue*) — looking
  the name up would send the query straight back into this same zone and
  need the answer it was looking for. One that arrives without an address is
  skipped, and the server logs which. A nameserver *outside* the zone can't
  loop like that, so it is looked up normally.
- **If every nameserver is skipped, the suffix stops resolving.** Same rule
  as a forwarder with no upstreams: the zone keeps its claim and answers
  SERVFAIL rather than letting the public internet answer.
- **A stub never expires.** A secondary past its SOA expiry stops answering,
  because it can no longer vouch for the records it holds. A stub holds no
  records it answers from — its NS set says *where to ask* — so it goes on
  routing to the last set it fetched however old that is. An old but working
  nameserver beats a self-inflicted SERVFAIL, and if the nameservers really
  are gone the query fails anyway.
- **Its upstreams are always port 53.** Neither glue nor an ordinary
  address lookup carries a port, so there is nowhere for one to come from.
  **Primary servers** still accepts `host:port` — that port is for the
  fetch, so a master on 5353 is fine — but a stub cannot route to a
  nameserver on a non-standard port.

Its records are read-only for a stronger reason than a secondary's. A stub
answers from none of them, but the routing table is rebuilt *from* them, so
a hand-written NS record would redirect the whole claimed suffix until the
next fetch overwrote it.

### Records

A record's name is relative to its zone's apex: `@` for the apex itself,
`bifrost` for `bifrost.<zone>`, `*` and `*.nexus` for wildcards. Typing the
fully-qualified name works too — a trailing copy of the zone's own name is
stripped automatically, so `bifrost.home.lan` and `bifrost` land on the same
record in zone `home.lan`.

The name has to be a domain name, and one the zone's own export can carry:
no spaces, no `;`, `"`, `(`, `)`, `\` or `/`, no empty label, and no leading
`$`. Every one of those means something else in a zone file — `;` starts a
comment, `$` opens a directive — so a record named that way would be written
into the export and then refuse to load back. Underscores are fine
(`_dmarc`, `_acme-challenge`, `_sip._tcp`).

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

**Otherwise a name in a value is absolute.** `nas` in a CNAME's value is the
name `nas.`, not `nas.<zone>` — the record's own name is relative to the zone
and its value is not, which is why the name-valued types carry a `FULL NAME`
marker beside the value.

**`@` in a value is the zone itself.** `10 @` on an MX is the zone apex, the
same as in a zone file, and saves as `10 home.lan.` — so pointing at the apex
does not mean spelling the zone name out. In a value that is text rather than
a name — a TXT — `@` is just the character.

**The SOA belongs to the zone, not to its records.** It is edited on the
zone, and a record of type `SOA` is refused: a zone has exactly one, and a
second one written here would be exported beside the real one and make the
file unloadable.

**Types dnsaur cannot serve are refused**, including RFC 3597's `TYPE65280`
form for a type it does not know. Anything the DNS parser understands works
— that list already runs well past what a homelab needs.

**The type list is not a limit either.** The select offers the nine types
most zones are made of, but a zone imported from elsewhere can hold an
`SSHFP`, `HTTPS` or `TLSA`. Editing one of those keeps its type: the record's
own type is offered as an extra option rather than the row silently rewriting
it to `A`.

An empty zone says **No records yet** and nothing more. **Add record** opens
the row that fills it; until then the zone answers `NXDOMAIN` for every name
beneath it, authoritatively, which is the whole point of holding the suffix.

**Quote TXT values.** Presentation format is not a free-text field: spaces
separate values and `;` starts a comment. Pasted raw, `v=spf1 -all` is
stored as two strings — it reads back `"v=spf1" "-all"` — and
`v=DKIM1; k=rsa; p=...` is truncated at the semicolon without any error,
reading back as just `"v=DKIM1"`. Wrapped in quotes — `"v=spf1 -all"` — the
whole thing is one value, which is what a TXT record almost always means.
Anything over 255 bytes, like a 2048-bit DKIM key, is written as adjacent
quoted strings that the reader joins back together: `"part one" "part two"`.

**A disabled record is shown as one.** A record can be disabled through the
API or by importing a zone file that leaves it out of its enabled set; it is
dropped when the zone's served snapshot is built, so it answers nothing —
indistinguishable from never having been written. Its row is muted and
marked `DISABLED` for exactly that reason. Editing one from the grid leaves
it disabled, and leaves its comment alone: neither is editable here, and
both survive a change to the name, type, TTL or value.

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
just the first — no partial import, and nothing changes. A file describing
more than 100,000 records is rejected the same way: `$GENERATE` expands one
line into up to 65,536 records, so a small file can otherwise ask for a very
large zone.

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
| **Query log** | How much per-query detail is recorded, how long the rows are kept, and how long the hourly totals behind the dashboard outlive them |
| **Lists** | How often subscriptions refresh |
| **Protocols** | Whether clients can reach dnsaur over DNS-over-TLS / DNS-over-HTTPS, and the certificate both present |

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

### Protocols

Each protocol is a checkbox, a listen address, and a line underneath saying
what is **actually** true right now — the two are separate facts and are
allowed to disagree:

- `○ off` — the setting is false.
- `● listening on :853` — enabled, and the socket is open.
- `● not listening — <reason>` — enabled, and the bind failed. The reason
  is the server's, verbatim. dnsaur retries every 30 seconds, so this
  clears itself once the cause is gone.
- `◌ status unavailable` — the server could not be asked. Nothing is
  claimed about the listener either way, which is the point: it may well be
  serving. Do not untick the box to "fix" it.

Underneath, one line for the certificate: `Expires 14 Nov 2026`, `Expires
in 9 days — 17 Sep 2026` once inside the 14-day warning window, `Expired —
14 Nov 2026` once past it, the server's own rejection if a save was
refused, `Status unavailable.` when the server could not be asked, or `No
certificate loaded.` when there is nothing to describe.

**Set the certificate before ticking a box.** Both paths and the checkbox
can go in one save — the form orders the writes for you — but the server
validates each write against what is already stored, so the certificate has
to be valid for the protocol to enable. Clearing either path needs *both*
protocols off first — while either one is enabled, an incomplete keypair is
the live configuration, and accepting it would take the listener down at
the next reconcile.

The three states that mean something is wrong — a failed bind, an expiring
certificate, and the upstream-encryption downgrade — also appear as banners
across the top of every screen, not just this one.

### Saving

Nothing on this page is written until **Save changes**. The bar at the top
counts what is unsaved and says whether it applies on save or waits for a
restart; **Discard** puts every field back to what the server last said.

Because a save is explicit, leaving is the one action that can lose work —
so navigating away with unsaved changes asks first: **Leave without saving?**,
with **Stay** and **Leave**.

A save writes one key per request, and they can fail one at a time. A key
that saved stops being counted as unsaved; one that was rejected keeps the
value that was typed, stays marked unsaved and can be retried, and the toast
names which. **Discard** after a partial save puts back what the server now
holds, not what it held before the save.

The page also re-reads the settings in the background, so a key saved from
another tab arrives here on its own. A field you are editing is never taken
away by one of those: only the fields you have not touched move.

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

**Used by** counts the zones that name each key — the secondaries and stubs
that sign with it, plus any zone naming it in `allow_transfer` or
`notify_to`. A key nothing uses reads `—` and can be deleted; a key some zone
still depends on cannot, and the delete confirmation says how many zones to
release it from first. Deleting it would leave those secondaries unable to
authenticate their transfers with nothing on the zone to say why, so the
server refuses it outright — the disabled button is only the polite version
of the same answer.

**A key in use cannot be renamed either.** `allow_transfer` and `notify_to`
name a key by its name rather than its id, so a rename detaches it from every
zone that named it. The name field is disabled on those rows, with the count
beside it; the algorithm and the secret stay editable, which is what rotating
a secret needs.

---

## Account & security

### When the server can't be reached

`Can't reach dnsaur` replaces the whole screen when the API doesn't answer —
including while it's restarting, which is not the same thing as being signed
out. **Retry** re-asks both the session and the setup state, so a server that
has come back drops you where you were, with the session you already had.

The login screen only appears when the server answers and says the session is
gone.

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
