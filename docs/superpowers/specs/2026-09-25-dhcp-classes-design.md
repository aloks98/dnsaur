# DHCP round two: client classes, pools, and the Sync band's engine column

Date: 2026-09-25. Builds on `2026-09-13-dhcp-design.md` (Kea is the engine, dnsaur the control plane). Everything here is rendered into the same `Dhcp4` object, synced in the same bundle, and shown in the same DHCP section.

## 1. Why

One scope treats every client alike: the same options, an address from anywhere in one contiguous pool. Treating a kind of device differently means a reservation per device and a filtering-group entry per MAC. Three things fix that:

- **Client classes** sort clients by what they are (vendor class, MAC prefix) and give them their own options and, optionally, their own pool. A new smart plug lands in the IoT range with the filtered DNS server the moment it powers on; a UEFI machine and a BIOS machine PXE-boot from the same scope with different files.
- **Pools** become a list per scope, each optionally reserved for a class. That is how ranges are carved out of a scope (two pools around the gap) and how a class gets its own addresses, in one construct, which is what Kea has.
- **The Sync band** says which replica runs an engine, because a replica without one is never the DHCP standby and today the only place that says so is the log.

## 2. Terms

- **Class**: a named rule. `name`, `matchers`, options. Global to the instance, synced, referenced by pools by name.
- **Matcher**: one test. `vendor:<prefix>` matches when option 60 (vendor class identifier) starts with the prefix; `mac:<prefix>` matches when the hardware address starts with the 1–6 given bytes. The `mac:` syntax is the filtering groups'. A class matches a client when any matcher matches.
- **Pool**: a `start`–`end` range inside a scope's subnet, with an optional `class`. A pool with a class serves only clients in that class; a pool without one serves anyone (members of a class included, when their own pool is full or absent — Kea's rule).
- **Class options**: the scope's Client-options set (DNS servers, DNS suffix, domain search list, NTP servers, static routes, next server, server hostname, boot file, generic options). Blank means the scope's value.

## 3. Data model

### 3.1 `dhcp_classes` (new, migration 0017, both dialects)

| column | type | notes |
|---|---|---|
| `id` | integer pk | |
| `name` | text unique (case-insensitive) | same rules as a scope name: 1–63 chars, not blank |
| `matchers` | json list of strings | each `vendor:<prefix>` (1–255 chars, no control characters) or `mac:<hex>` (1–6 bytes as `aa` … `aa:bb:cc:dd:ee:ff`; lower-cased on write); at least one |
| `dns_servers`, `domain`, `domain_search`, `ntp_servers`, `static_routes`, `next_server`, `server_hostname`, `boot_file`, `options` | as on `dhcp_scopes` | same validation, same normalisation; all optional |
| `created_at`, `modified_at` | integer ms | |

### 3.2 `dhcp_pools` (new, migration 0017) replaces `pool_start` / `pool_end`

| column | type | notes |
|---|---|---|
| `id` | integer pk | |
| `scope_id` | fk `dhcp_scopes` on delete cascade | |
| `position` | integer | order in the dialog, stable |
| `start`, `end` | text | IPv4, `start <= end`, both inside the scope's subnet, neither the network nor broadcast address |
| `class_id` | fk `dhcp_classes` nullable, on delete **restrict** | a class in use cannot be deleted |

The migration copies every scope's `pool_start`/`pool_end` into one pool at position 0, then drops the two columns. A scope must have at least one pool (a scope with none is refused on write, so the migration's invariant holds). Pools within a scope may not overlap; a reservation's address may sit inside or outside any pool, as before.

### 3.3 Wire shapes

`Scope` gains `pools: [{id, start, end, class_id}]` (ordered) and loses `pool_start`/`pool_end`; the strict decoder refuses the old keys with a message naming `pools`. `Class` is `{id, name, matchers: [string], …options, created_at, modified_at}`. Both travel in the config bundle with the main's ids (classes before scopes, so a pool's `class_id` resolves on import). The lease table's `Table` gains nothing: a lease belongs to a scope by subnet as before.

## 4. Render

Into the same `Dhcp4` object, from the same `RenderInput` (which gains `Classes []store.Class` and reads pools from each scope):

- **`client-classes`**: one entry per class, in id order: `{"name": <name>, "test": <expr>, "option-data": [...], "next-server", "server-hostname", "boot-file-name"}`. The test joins matchers with ` or `: `vendor:PXEClient` → `substring(option[60].text,0,9) == 'PXEClient'` (length is the prefix's byte length; single quotes in the prefix are refused at validation); `mac:a4:cf:12` → `substring(pkt4.mac,0,3) == 0xa4cf12`. Class option-data is rendered from the class's own fields only; blank fields are omitted, never copied from a scope.
- **`subnet4[].pools`**: one entry per pool in position order: `{"pool": "<start> - <end>"}`, plus `"client-class": <name>` when the pool has a class, plus the class's option-data and PXE fields **again at the pool level**. Kea's option precedence is host reservation > pool > subnet > shared network > class > global, so a class's options only beat the scope's own when they sit on the pool; class-level option-data alone loses to any value the scope sets (DNS servers, which every scope renders, included).
- Consequence, documented and shown in the dashboard's docs, not on screen: a class without a pool in a scope changes only options that scope leaves blank (PXE fields, NTP, search list); a class with a pool changes everything it sets. The spike in Task 1 confirms the precedence on Kea 2.6.3 and 3.0 with a real exchange before anything depends on it.
- **DNS names**: a lease from a pool whose class sets `domain` is named under that suffix; otherwise the scope's, otherwise `dhcp.domain`. The table learns each pool's class suffix from the scope snapshot; the middleware is unchanged.
- **Golden files**: `classes.golden.json` (two classes, one with a pool, one options-only) and `pools-multi.golden.json` (three pools around a gap) beside the existing ones; existing goldens change only in the pool spelling if at all.

## 5. API

- `GET/POST /api/v1/dhcp/classes`, `GET/PATCH/DELETE /api/v1/dhcp/classes/{id}`; writes are `managed` on a replica like scopes; every write re-renders via the manager as scope writes do. Delete answers 409 `class "iot" is in use by 2 pools (Office, IoT)` while any pool names it.
- Scope writes accept `pools`; PATCH re-validates the whole scope (overlaps, subnet bounds, class ids exist). A pool referencing an unknown class is 422 naming the field.
- `GET /sync/status` replicas already carry `dhcp`; nothing new server-side for the Sync band.
- openapi.yaml, `docs/api.md`, and the openapi route guard test cover the new routes.

## 6. Dashboard

Per the artboards the user is producing from the round-two prompt (Classes board, scope dialog pools, Sync column):

- **Classes tab** beside Scopes / Leases / Reservations, row-2 readout `N classes`. List: name, matchers, an options summary (`DNS 10.0.0.2 · suffix iot.lan · PXE`), pools using it, Edit / Delete. Dialog with tabs **Matchers** (name, a matchers mini-table with a kind select `vendor` / `mac` and a mono value, "A client matches when any row matches.") and **Client options** (the scope's sections minus Behaviour, plus Names & DNS at the top). Delete is refused inline with the API's message.
- **Scope dialog**, Network tab: a **Pools** mini-table (start, end, class select with `any`) replaces Pool start / Pool end; row errors for overlap, out of subnet, unknown class. The Scopes table's POOL column shows the first range and `+N more`; LEASED counts across pools.
- **Sync band**: a DHCP column per replica, `engine` or `no engine`; one muted line under the table when a replica has none and this main runs DHCP: "A replica without an engine is never the standby."
- Copy is plain and brief; server error text verbatim; docs (`docs/dashboard.md`, `docs/ui-contract.md`, `docs/configuration.md`) carry the explanations.

## 7. Failure modes

- A class referenced by a pool cannot be deleted (restrict at the store, 409 at the API, disabled Delete in the dialog with the reason).
- A pool made empty by reservations is Kea's problem to report; dnsaur renders what was asked.
- Overlapping pools across two classes are refused at write time; Kea would refuse them too, later and less clearly.
- A matcher Kea cannot evaluate (a `vendor:` prefix with a quote) is refused at validation, never rendered.
- A replica imports classes before scopes; a bundle whose pool names a class id that is absent is refused whole, like any other broken bundle.

## 8. Not implemented, and why

- **Option 82 (relay circuit / remote id) matchers**: no relays on the network yet; the matcher list is the one extension point, so it slots in later without a schema change.
- **Known-classes-only per scope**: no guest segment yet; `reservations_only` covers the stricter case today.
- **DHCPv6**: a second engine and socket; its own milestone.
- **Per-scope class overrides** (same class, different options per scope): Kea has no such thing; a second class is the answer.

## 9. Test posture

Store tests on both drivers (migration copies pools, overlaps refused, delete restricted, bundle round trip with classes before scopes); renderer goldens plus a test that class options land on the class's pools; the real-engine test gains a class with a pool and asserts a matching client (by vendor class, using `lease4-add`-free real exchanges from the container) gets the pool's address and options — that is the spike, kept as the regression; API handler tests for the new routes and the 409; dashboard tests for the Classes page, the pools table, and the Sync column; docs updated with every task.

## 10. Task shape

1. Spike + renderer: confirm precedence on real Kea; render classes and pools; goldens.
2. Store + migration + bundle.
3. API + manager wiring + openapi + docs/api.
4. Dashboard: scope dialog pools + Scopes table.
5. Dashboard: Classes page.
6. Dashboard: Sync band column; docs sweep (`configuration.md`, `dashboard.md`, `ui-contract.md`, README row).
