# dnsaur Config Sync: design

**Status:** draft for review, 2026-09-11
**Replaces:** the Phase 1.5 sketch in `2026-08-03-dnsaur-design.md` §"High Availability — Config Sync"
**Deployment this is for:** one main box on Postgres, one backup box on SQLite, both handed to clients by DHCP.

---

## 1. Why, and what the earlier sketch got wrong

Two dnsaur instances serve a network so that either can answer when the other is down. DNS failover is the client's job (DHCP hands out both addresses); dnsaur's job is that both boxes *behave the same*. Zone transfers already make authoritative data identical. Nothing makes the rest identical: blocklist subscriptions and their group assignments, allow and block rules, client groups and matchers, upstreams and strategy, blocking mode and TTL, cache clamps, pauses, TSIG keys, forwarder and stub zone definitions. Run two boxes today and that config drifts by hand.

The Phase 1.5 sketch solved this with a join token, a minted certificate pinned by fingerprint, a long-poll push channel, replica UIs that proxy writes to the primary, one-click promotion and federated stats. Every one of those is a subsystem. What parity needs is smaller: one document that describes the config, one endpoint that serves it, one worker that pulls and applies it. The main box never learns anything it does not already know, except that a replica exists.

Decisions taken with the user on 2026-09-11:

- **Zone data stays on AXFR.** The bundle carries zone *definitions*; the replica maintains a secondary zone for each of the main box's primaries. The transfer path, NOTIFY and TSIG were built for exactly this.
- **A replica edits only instance-local settings.** Everything synced is read-only on the replica; clearing the peer is the promotion.
- **HTTPS expected, plain HTTP tolerated with a warning.** No certificate subsystem: a reverse proxy, or nothing on a LAN the operator trusts, is their call and the settings screen says which one they made.

## 2. Terms

- **Main** — the instance whose config is the source of truth. It has no sync settings of its own beyond an API token it issued.
- **Replica** — an instance with `sync.peer_url` set. It pulls, applies, and refuses local writes to synced resources.
- **Bundle** — the JSON document in §4.
- **Synced** — a resource the bundle carries. **Local** — one it does not (§4.3).

An instance is one or the other by the presence of `sync.peer_url`; there is no third mode, and "main" is not a setting — it is any instance a replica points at.

## 3. Data flow

```
replica                                   main
  │ every sync.interval (default 30 s)     │
  ├─ GET /api/v1/sync/version ────────────►│  {config_version, instance_id}
  │◄───────────────────────────────────────┤
  │ unchanged → done                       │
  │ changed:                               │
  ├─ GET /api/v1/sync/bundle ─────────────►│  the bundle (§4), Authorization: Bearer <token>
  │◄───────────────────────────────────────┤
  │ apply in one transaction (§5)          │
  │ reload clients, zones, filters         │
  ├─ POST /api/v1/sync/replicas ──────────►│  {instance_id, dns_addr, version_applied}
  │◄───────────────────────────────────────┤  204; main records the replica (§6)
```

Polling, not push. A version probe is a few hundred bytes; at 30 s the worst-case lag is the interval, which is the same order as the cache TTLs the network already lives with. The SSE channel the old sketch proposed would save nothing an operator can notice.

The replica applies a bundle only when `config_version` moved and the bundle's own version matches the probe (a write landing between the two requests is caught by the next cycle). A pull that fails leaves the last applied config in force; that is the whole failure story for the replica, and it is the behaviour the rest of dnsaur already has for a store that is down.

## 4. The bundle

`GET /api/v1/sync/bundle` answers, for a **write-scope** token (the bundle carries TSIG secrets, which read scope is denied since #54):

```json
{
  "format": 1,
  "config_version": 412,
  "instance_id": "…",
  "settings": { "upstreams": "…", "blocking.mode": "…", "blocking.pauses": "…", "...": "…" },
  "sync_key": 1,
  "groups":   [ { "id": 1, "name": "default", "enabled": true } ],
  "clients":  [ { "id": 3, "name": "nas", "matcher": "10.0.0.5", "group_id": 1 } ],
  "lists":    [ { "id": 2, "name": "…", "url": "…", "kind": "block", "enabled": true, "groups": [1] } ],
  "rules":    [ { "id": 7, "group_id": 1, "action": "allow", "pattern": "…", "regex": false } ],
  "tsig_keys": [ { "id": 1, "name": "xfer.example.", "algorithm": "hmac-sha256.", "secret": "…" } ],
  "zones":    [ { "name": "e412.in", "type": "primary", "soa": {…}, "allow_transfer": "…", "notify_to": "…", "tsig_key_id": 1 },
                { "name": "corp.example", "type": "forwarder", "forward_to": "…" },
                { "name": "lab.example", "type": "stub", "primaries": "…", "tsig_key_id": 0 } ]
}
```

### 4.1 Identity across boxes

Ids are the main box's. The replica keeps them: every synced table is written with the main's ids, so foreign keys in the bundle (`group_id`, `tsig_key_id`, list-to-group assignments) mean the same thing on both sides and the query log's `rule_id`/`list_id` on the replica point at the same rows an operator sees on the main. This works because a replica never creates synced rows of its own (§7), so its id space is free for the main's to occupy. SQLite and Postgres both accept explicit ids on insert; the replica's sequences are advanced past the max on each apply so a later promotion does not collide.

### 4.2 Zones in the bundle

For each zone on the main:

| main's type | replica gets |
|---|---|
| `primary` | a `secondary` of the same name, `primaries = sync.primary_dns`, `tsig_key_id` = the sync key (§6), `allow_transfer` and `notify_to` copied verbatim so the replica serves its own downstreams the same way the main does |
| `secondary` | a `secondary` with the main's own `primaries` and key — the replica pulls from the same upstream primary, not from the main; both boxes are then equal secondaries of it |
| `stub`, `forwarder` | the same definition |
| `internal` | nothing; both boxes seed the built-ins themselves |

Records never ride in the bundle. The replica's secondaries transfer them, on the schedule the SOA gives and promptly on NOTIFY (§6).

### 4.3 What stays local

| Local on every instance | Why |
|---|---|
| bootstrap YAML (`dns_listen`, `http_listen`, `data_dir`, `storage.*`, `log_*`, `trusted_proxies`) | describes the box, not the service |
| `instance.id` | identity; the query log is tagged with it |
| `serve.dot.*`, `serve.doh.*`, `serve.tls.*` | listen addresses and certificate paths are per box |
| `sync.*` | the replica's own relationship to its main |
| `stats.watermark` | bookkeeping |
| users, sessions, API tokens | an admin account per box; the replica's token to the main is the only credential that crosses |
| query log, stats | per instance, as before; a merged view is out of scope |

Everything else in `settings` is synced, including `blocking.pauses` (a pause is a decision about the network, and clients reach either box) and `upstreams` (the replica resolving through different upstreams than the main is the kind of drift this exists to remove).

## 5. Applying a bundle

One transaction, or nothing: `Store.ImportBundle(ctx, b)` diffs each synced table against the bundle by id — insert, update, delete — and bumps `config_version` once. It runs under `RunTx` on both dialects. Partial application is not a state the replica can be in.

Order inside the transaction follows the foreign keys: groups, clients, lists and their assignments, rules, TSIG keys, zones. A zone whose derived secondary already exists keeps its transfer state (`refreshed_at`, `expires_at`, `soa_serial`, records); only the definition columns change, so a config pull never makes a serving secondary forget what it transferred.

After the transaction the replica runs, in order, what the API handlers run after their own writes: `ReloadClients`, `RecompileFilters` (from cache; a list the replica has never downloaded is fetched by the next ticker, exactly as a newly created list is today), `ReloadZones`, then `applySettings`. A newly derived secondary is `RefreshDue` immediately, so its first transfer starts within the scheduler's next pass.

Version bookkeeping: the replica stores `sync.applied_version` and `sync.applied_at`; the status endpoint (§8) reports them beside the main's current version, so "behind by N" is visible.

## 6. Zones: transfers and NOTIFY between the boxes

The replica needs to be allowed to transfer from the main, and the main should NOTIFY the replica. Neither requires the operator to edit ACLs, and neither is done by rewriting zone rows:

- **The sync key.** The main designates one TSIG key as `sync.tsig_key_id` (a setting on the main; it is in the bundle as `sync_key`, and the replica's derived secondaries use it). Creating the key and setting it is the one manual step on the main, and the settings screen's Sync band walks through it.
- **Registered replicas.** `POST /api/v1/sync/replicas` (write scope) records `{instance_id, dns_addr, version_applied, last_seen}` in the main's `sync.replicas` settings row (JSON; a handful of entries). A replica not seen for `3 × sync.interval` is shown as stale, never deleted automatically; the operator removes one from the Sync band.
- **Implicit allow.** `TransferServer.decidePeer` allows an AXFR for a `primary` zone when the request verified under the sync key **and** the peer address is a registered replica's `dns_addr`. It is one extra clause beside the ACL, not an ACL rewrite, so `allow_transfer` still says exactly what the operator wrote.
- **Implicit notify.** `Notifier.Pass` adds every registered replica's `dns_addr` (signed with the sync key) to each `primary` zone's targets. Delivery bookkeeping (`zone_notifies`) treats them like any other target.

A replica's own `secondary` zones (the ones derived from the main's *secondaries*, table in §4.2) are ordinary secondaries of the external primary; nothing above applies to them.

## 7. The replica is read-only for synced resources

With `sync.peer_url` set, every write handler for a synced resource answers **409** `managed by <peer_url>` before touching the store: groups, clients, lists, rules, TSIG keys, zones and records, and `PUT /settings` for any key outside the local set of §4.3. Two writes stay allowed because they are operational, not config: a list's "Refresh now" and a zone's transfer "Refresh now". The dashboard renders those screens with their controls disabled and one line at the top: "Managed by <peer>". Local settings keep their forms.

Promotion is `DELETE` on the peer: clear `sync.peer_url` (the Sync band has one button), the guard lifts, the derived secondaries stay secondaries of a main that may be gone. Turning them into primaries is a per-zone decision the operator makes on the zone page with the existing type change; the Sync band says so in one line after promotion. There is no automatic election and no split-brain to prevent, because only one box ever accepted writes.

## 8. Settings, status and the dashboard

Settings (all local, §4.3):

| key | default | validation |
|---|---|---|
| `sync.peer_url` | `""` | absolute `http`/`https` URL, or empty |
| `sync.token` | `""` | non-empty when `peer_url` is set; never returned by `GET /settings` (write-only, like a secret; the band shows "set" / "not set") |
| `sync.interval_seconds` | `30` | `positiveInt`, minimum 5 |
| `sync.primary_dns` | `""` | `host:port`; when empty, the peer URL's host on port 53 |
| `sync.tsig_key_id` (main only) | `0` | an existing key id or 0 |

`GET /api/v1/sync/status` (authenticated), on both kinds of instance:

```json
{ "role": "replica", "peer_url": "https://…", "peer_version": 412, "applied_version": 412,
  "applied_at": 1757580000000, "last_pull_at": …, "last_error": "", "plain_http": false }
{ "role": "main", "sync_key": "xfer.example.", "replicas": [ { "instance_id": "…", "dns_addr": "10.0.0.6:53",
  "version_applied": 412, "last_seen": …, "stale": false } ] }
```

The warning strip (the `ResolverStatus` pattern from E1/E2) gains three facts: the replica is behind or its last pull failed (with the error), the peer is reached over plain HTTP (persistent while it is), and, on the main, a registered replica is stale. All three clear on their own.

The Settings screen gains a **Sync** band: on a replica, the peer URL, token (set/clear), interval, primary DNS address, the status line and a "Stop following" action; on a main, the sync key select, the replica list with last-seen and applied version, and a remove action per replica. Copy stays plain, per the project rule; the reasoning lives in `docs/dashboard.md`.

## 9. Security

- The bundle carries TSIG secrets and the token authenticating the pull. Over plain HTTP they cross the LAN in the clear on every pull; the persistent warning is the only mitigation offered, by decision.
- The pull token is a write-scope API token minted on the main. It is stored on the replica in `settings` like every other value; the replica's own admin can read the database, which is already true of TSIG secrets today.
- A replica never proxies writes to the main, so a compromised replica gains only the token it already holds; the main's exposure is one write-scope token, revocable from the main's account page (#67).
- `POST /sync/replicas` is what turns on the implicit transfer allow (§6). It requires the same write-scope token, so an unauthenticated peer cannot register itself into an AXFR allow.

## 10. Failure modes

| Situation | Behaviour |
|---|---|
| main unreachable | replica keeps its last applied config indefinitely; status shows `last_error`; DNS unaffected |
| main reachable, bundle rejected (format, validation) | nothing applied; `last_error` names the field; the main's version is not marked applied |
| replica's store fails mid-apply | transaction rolls back; previous config stays |
| token revoked on the main | 401 → `last_error: "peer refused the token"`; config frozen until re-set |
| zone transfer from main refused | the derived secondary's own `last_error` says so (existing behaviour); the Sync band links to the zone page |
| replica promoted while main is up | main keeps listing it as a replica until removed; its pulls stop, so it goes stale in the main's list |
| bundle format bumped | the replica refuses a `format` it does not know and says so; upgrade the replica first |

## 11. Not implemented, and why

- **Push, long-poll, SSE.** Polling at 30 s is within the lag the network already tolerates.
- **Write proxying from the replica UI.** One box accepts writes; the replica says which one.
- **Automatic promotion or election.** Manual, one button; there is no shared state to arbitrate.
- **Federated stats or a merged query log.** Per instance, as today.
- **Multi-main or replica-of-replica.** One main, any number of replicas, one level.
- **A certificate subsystem.** §1.
- **Record replication in the bundle.** AXFR does it.

## 12. RFC and compatibility notes

Nothing on the wire is new DNS. The implicit allow and notify in §6 are policy inputs to the existing RFC 5936 / RFC 1996 / RFC 8945 code paths, and the transfer server still answers exactly the refusals the zones spec §9.5.5 documents for everyone else.

## 13. Test posture

- Store: `ImportBundle` round trip on both drivers via `forEachDriver`; a bundle that deletes a group cascades its clients and rules; a failing insert rolls everything back; ids are preserved and sequences advanced.
- API: the bundle contains exactly the synced set and nothing local (assert the `serve.*` and `sync.*` keys are absent); read-scope tokens get 401 on `/sync/bundle`; the replica guard answers 409 on every write route for synced resources (driven from the route registry like `TestEveryRouteEnforcesAuth`, with an explicit allowlist of the operational writes).
- Worker: version unchanged → no bundle fetched; changed → applied and reloaded; failure keeps the previous config; plain-HTTP peer sets the warning.
- Loopback: a main and a replica as two Apps in one test process (the zones loopback tests show the shape): create a primary zone with records on the main, register the replica, assert the replica transfers it under the sync key with no ACL edit and receives a NOTIFY on the next record write.
- Dashboard: the Sync band on each role; the managed-by notice and disabled controls on a replica; the warning strip facts.

## 14. Task shape

1. Bundle model and `ImportBundle` in `internal/store`, both dialects.
2. `GET /sync/version`, `GET /sync/bundle`, `POST /sync/replicas`, `GET /sync/status`; `sync.*` settings and validation; token write-only.
3. The replica worker in `internal/app` (pull, apply, reload, register) and the read-only guard in `internal/api`.
4. Derived secondaries, the implicit allow in the transfer server and the implicit notify targets.
5. Warning-strip facts and the Sync band.
6. Loopback test, docs (README status row, architecture, configuration, api, ui-contract, dashboard).
