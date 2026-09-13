# dnsaur DHCP: design

**Status:** draft for review, 2026-09-13
**Replaces:** the Phase 2 sketch in `2026-08-03-dnsaur-design.md` §"Phases" (own DHCP implementation)
**Depends on:** config sync (`2026-09-11-config-sync-design.md`): scopes and reservations are synced config, the main/replica pair is what Kea's HA pair is rendered from.
**Deployment this is for:** one flat LAN today, VLANs with a relaying router later; a main box and a backup box, both handing out DNS.

---

## 1. Decision: Kea is the engine, dnsaur is the control plane

dnsaur does not implement DHCP. ISC Kea (`kea-dhcp4`) serves the protocol; dnsaur owns everything an operator touches: scopes, reservations, the names leases get in DNS, the client identity a lease gives a device, and the status of the whole thing.

Why not our own server: the requirements are relay and VLAN scopes, reservations, hostname registration, and a backup box that takes over when the main is down. The last one alone is a lease-replication and partition-handling protocol; Kea ships it (`libdhcp_ha`, hot-standby with lease sync) and everything else besides (option 82, client classes, PXE, DHCPv6) for the price of a second daemon. A spike on 2026-09-13 confirmed the two pieces this design leans on:

- Debian trixie packages Kea 2.6.3 with `libdhcp_ha.so` and `libdhcp_lease_cmds.so` in `kea-dhcp4-server`; Alpine edge packages Kea 3.0.3 with every hook as its own package (`kea-hook-ha`, `kea-hook-lease-cmds`, and `kea-hook-host-cmds`, which 3.0 made open source).
- Over the unix control socket, `config-set` applies a whole `Dhcp4` object live (a new pool and a new reservation took effect without a restart), `lease4-add`/`lease4-get-page` read and write leases, and `status-get` reports the HA state.

What it costs, stated once: Kea is a second process the operator installs and keeps in step with dnsaur's documented version range; in a container both run with host networking; a rejected config is Kea's message, which dnsaur shows verbatim.

## 2. Terms

- **Scope**: one subnet dnsaur serves: CIDR, one pool range, gateway, lease time, DNS suffix, optional DNS-server override. Rendered as one Kea `subnet4`.
- **Reservation**: a fixed address for a MAC inside a scope, with an optional hostname. Rendered as a Kea host reservation inside the subnet.
- **Lease**: an address Kea has handed out. Never a dnsaur table; read from Kea.
- **Engine**: the local `kea-dhcp4` process dnsaur talks to over its control socket.
- **HA pair**: the main's Kea and the first registered replica's Kea, in Kea hot-standby, rendered by both dnsaur instances from the same synced config.

## 3. Data flow

```
operator ──► dnsaur API ──► store (scopes, reservations, settings)   [synced config, both boxes]
                               │
                               ▼  on every change, and at start
                          renderer ──► {"command":"config-set","arguments":{"Dhcp4":…}} ──► kea-dhcp4 (unix socket)
                                                                                              │  serves :67, holds leases,
                                                                                              │  HA-syncs with the peer Kea
                          lease poller ◄── {"command":"lease4-get-page"} ◄────────────────────┘
                               │ every dhcp.lease_poll_seconds (default 10)
                               ▼
                    in-memory lease table ──► DNS stage (A/PTR)  ──► client identity (mac matcher) ──► API (Leases page, query log names)
```

Both boxes run the same loop against their own Kea. Kea's HA keeps the two lease sets identical; dnsaur never copies leases between boxes.

## 4. Configuration

### 4.1 Bootstrap (per box, in `dnsaur.yaml` / env)

| key | default | meaning |
|---|---|---|
| `kea_socket` (`DNSAUR_KEA_SOCKET`) | `""` | path of `kea-dhcp4`'s unix control socket; empty means DHCP is off and none of the below runs |

Everything else is a setting, because it is either config both boxes share or a local fact the dashboard should be able to change.

### 4.2 Settings

Synced (spec §4.3 of config sync: every key not under a local prefix):

| key | default | validation |
|---|---|---|
| `dhcp.domain` | `""` | a DNS suffix (`home.lan`); scopes default to it; empty means leases get no names |
| `dhcp.lease_seconds` | `3600` | `positiveInt`, minimum 300; the default for scopes that set none |
| `dhcp.lease_poll_seconds` | `10` | `positiveInt`, minimum 2 |
| `dhcp.ha_port` | `8000` | port each box's Kea HA listener answers on |
| `dhcp.ha_standby` (internal) | `""` | written by the main's renderer: the `instance.id` of the replica it chose as standby; replicas read it to know whether they are in the pair |

Local (`serve.` prefix, never in a bundle):

| key | default | validation |
|---|---|---|
| `serve.dhcp_interfaces` | `""` | comma-separated interface names Kea binds (`eth0`, `eth0.10`); empty means Kea's `interfaces: ["*"]` |

### 4.3 Scopes (`dhcp_scopes`, synced with the main's ids)

| column | validation |
|---|---|
| `id`, `name` | name unique, non-empty |
| `cidr` | IPv4 CIDR; no two enabled scopes overlap |
| `pool_start`, `pool_end` | inside `cidr`, start ≤ end, neither the network nor broadcast address |
| `gateway` | inside `cidr`, or empty (no router option) |
| `dns_servers` | comma-separated addresses, or empty = automatic (§5.3) |
| `domain` | suffix, or empty = `dhcp.domain` |
| `lease_seconds` | ≥ 300, or 0 = `dhcp.lease_seconds` |
| `enabled` | disabled scopes are not rendered |
| `domain_search` | comma-separated suffixes, or empty (option 119) |
| `ntp_servers` | comma-separated IPv4 addresses, or empty (option 42) |
| `static_routes` | list of `{destination CIDR, router inside cidr}`, or empty (option 121) |
| `next_server`, `server_hostname`, `boot_file` | PXE: an IPv4 address, a hostname, a file name; each optional (siaddr, sname/66, file/67) |
| `options` | list of `{code 1–254 not already rendered by name, hex value}`; the generic escape hatch (WINS 44, CAPWAP 138, TFTP 150, vendor info 43 without class matching) |
| `match_client_id` | default true; false makes Kea key leases on the MAC and ignore option 61 (cloned VMs) |
| `reservations_only` | default false; true renders the subnet with no pool, so only reserved devices get addresses |

### 4.4 Reservations (`dhcp_reservations`, synced)

| column | validation |
|---|---|
| `id`, `scope_id` | scope must exist (FK) |
| `mac` | canonical `aa:bb:cc:dd:ee:ff`, unique per scope |
| `ip` | inside the scope's `cidr`, not the gateway, unique per scope; may sit inside or outside the pool (Kea excludes reserved addresses from dynamic allocation either way) |
| `hostname` | RFC 1123 label or empty; unique per scope when set |
| `comment` | free text |

Both tables are in the config bundle, bump `config_version` on write, and are read-only on a replica (409 `managed by <peer>`), exactly like groups and zones.

## 5. Rendering the engine's config

### 5.1 When

At start, after every write to `dhcp_scopes`, `dhcp_reservations`, or a `dhcp.*`/`serve.dhcp_interfaces` setting (the settings watcher already wakes on every synced write), and after a bundle is applied on a replica. The renderer builds the whole `Dhcp4` object and sends one `config-set`. Kea answers `result: 0` with a hash, or `result: 1` with its message.

A refused config leaves Kea on its previous one. dnsaur keeps the message in memory (it is per box, and a restart renders again anyway; `dhcp.` is a synced prefix, so a setting would travel), cleared by the next accepted render, and shows it (§8). It does not retry on a timer; the next change, or "Apply again" on the DHCP page, re-renders.

### 5.2 What

```
Dhcp4:
  interfaces-config.interfaces: serve.dhcp_interfaces or ["*"]; dhcp-socket-type raw
  control-socket (below 2.7.2) / control-sockets: [ unix ] (2.7.2+): the bootstrap path echoed back; never an http entry (§6)
  lease-database: {type: memfile, persist: true}                # Kea's default file path
  valid-lifetime: dhcp.lease_seconds; renew-timer/rebind-timer: 50% / 87.5% of it
  hooks-libraries:
    libdhcp_lease_cmds.so
    libdhcp_ha.so with high-availability: [ {this-server-name, mode: hot-standby, peers: [...]} ]   # §6
  subnet4: one per enabled scope:
    id: scope id, subnet: cidr, pools: [start - end]
    valid-lifetime: scope override when set
    option-data: routers (gateway, if set); domain-name-servers (§5.3); domain-name (scope suffix, if any);
                 domain-search (119); ntp-servers (42); classless-static-route (121, "dest - router, …");
                 every generic option as {code, csv-format: false, data: hex}
    next-server, server-hostname, boot-file-name when set; match-client-id: false when the scope says so
    pools omitted when reservations_only
    reservations: [{hw-address, ip-address, hostname?}]
```

Hook library paths come from Kea itself: at start dnsaur sends `config-get` once and reuses the `hooks-libraries` directory it finds, so Debian's `/usr/lib/x86_64-linux-gnu/kea/hooks/` and Alpine's `/usr/lib/kea/hooks/` both work without a setting. If neither hook is present Kea refuses the config and the message says which file is missing.

### 5.3 DNS servers handed to clients

Automatic unless the scope overrides. Each box's own DNS address is the host part of its first `dns_listen`; when that host is unspecified (`0.0.0.0`), it is the first non-loopback IPv4 address inside the scope's CIDR among the host's interfaces (`net.Interfaces`), and when there is none the renderer refuses with `scope <name>: set dns_servers, no local address is inside <cidr>`. On a main the list is its own address followed by the chosen standby's `dns_addr` host; on the standby, the main's DNS host (`sync.primary_dns` or the peer host) followed by its own. Both boxes therefore hand out the same two addresses in the same order, which is what DHCP-level failover needs from DNS.

## 6. Failover: Kea hot-standby, rendered from the pairing

The HA hook needs each Kea to reach the other over HTTP, and it brings its own transport: with multi-threading on (the default on 2.6 and 3.0) the hook opens a dedicated HTTP listener at the address and port of this server's own peer entry and answers `ha-heartbeat` and lease updates there. Verified on 2026-09-13 on both versions with nothing but a unix control socket configured. So dnsaur renders no HTTP control socket at all, and no `kea-ctrl-agent` is needed; `dhcp.ha_port` is simply the port in the peer URLs. (An HTTP `control-sockets` entry on the same port collides with the hook's listener, and a config carrying both `control-socket` and `control-sockets` is refused by 3.0 — both verified.)

The only version-dependent rendering is the spelling of the unix control socket: `control-socket` (singular) below 2.7.2, `control-sockets: [ { unix } ]` from 2.7.2 on. The renderer learns the version once at start (`version-get`).

Peer rendering: the main chooses its standby as the non-stale registered replica with the lexicographically smallest `instance.id` (deterministic, no timestamps involved) and writes that id to the synced setting `dhcp.ha_standby` before rendering, so the choice travels in the next bundle. On the main, `this-server-name` is its `instance.id`, role `primary`; the standby peer is `http://<replica dns_addr host>:<ha_port>/`. A replica whose `instance.id` equals `dhcp.ha_standby` renders the same two peers with the roles unchanged and `this-server-name` its own id; the main's url is `http://<peer host>:<ha_port>/`. Both boxes render the same pair, which is the HA hook's requirement. Any other replica renders no HA section and serves DNS only; its DHCP page says `DHCP: not in the HA pair`.

Kea handles what dnsaur used to plan by hand: lease synchronisation, the standby answering only when the primary is unreachable (`max-unacked-clients` plus heartbeats), and reconciliation afterwards. dnsaur renders the hook's timers with Kea's defaults except `heartbeat-delay: 10000` and `max-response-delay: 60000`, so takeover happens within about a minute.

Without a replica, dnsaur renders no HA section at all; a single box runs plain Kea.

## 7. What dnsaur reads back

### 7.1 Leases

Every `dhcp.lease_poll_seconds` dnsaur pages through `lease4-get-page` and replaces its in-memory table: address, MAC, hostname (Kea's, from the client's option 12 or FQDN), subnet id → scope, `cltt` + `valid-lft` → expiry, state (0 = active; declined and expired-reclaimed entries are dropped). The table also carries every reservation's hostname, so a reserved device has a name before its first lease.

If Kea stops answering, the table stays as it was and `dhcp.status` reports `engine unreachable` with the age of the table; it is cleared only when Kea answers again with a new page.

### 7.2 Status

`status-get` gives Kea's uptime, the HA state (`local.state`, `remote.state`, `communication-interrupted`, `unacked-clients`) and the loaded hooks. dnsaur reads it on the same poll and exposes it (§8).

## 8. DNS, client identity, API, dashboard

### 8.1 DNS from leases

A `dhcp` pipeline stage sits before `zones`: for `A <hostname>.<scope suffix>` it answers the lease's address; for `PTR` inside a scope's reverse range it answers `<hostname>.<suffix>.`. Hostnames are sanitised to RFC 1123 labels (lowercased, invalid runs collapsed to `-`); a lease with nothing usable gets no name. When two leases in one scope sanitise to the same name, the newer keeps it and the older is logged. TTL is `min(300, remaining lease time)`.

A static record in a zone that holds the same name wins: the stage asks the zone table for an exact record before answering, so explicit configuration beats inferred state. Answers carry `matched = dhcp` in the query log.

### 8.2 Client identity

Clients gain a third matcher kind, `mac`. The client registry resolves a `mac` matcher to the MAC's current lease address (and its reservation address, when reserved) from the lease table and re-resolves whenever the table changes, so a device keeps its group across renewals. A MAC with no lease matches nothing.

The query log and the client list show the lease hostname beside an address when the lease table has one; that is a read-time join, nothing is stored.

### 8.3 API

| route | notes |
|---|---|
| `GET/POST /api/v1/dhcp/scopes`, `PATCH/DELETE /api/v1/dhcp/scopes/{id}` | synced writes; `managed` on a replica |
| `GET/POST /api/v1/dhcp/reservations`, `PATCH/DELETE /api/v1/dhcp/reservations/{id}` | same |
| `GET /api/v1/dhcp/leases` | the table: `[{scope_id, ip, mac, hostname, expires_at, reserved}]` |
| `DELETE /api/v1/dhcp/leases/{ip}` | `lease4-del` on the local engine; not `managed` — a lease belongs to the engine, not to config, and HA propagates the release |
| `POST /api/v1/dhcp/leases/{ip}/reserve` | creates a reservation from a lease (scope, MAC, address, hostname) |
| `GET /api/v1/dhcp/status` | `{enabled, engine: "ok" \| "unreachable" \| "config rejected", engine_version, message, table_age_seconds, ha: {mode, local_state, remote_state, communication_interrupted}, scopes: [{id, pool_size, leased}]}` |
| `POST /api/v1/dhcp/apply` | re-render and `config-set` now; `managed` on a replica |

`ResolverStatus` gains a `dhcp` block equal to the status object, so the warning strip can read it.

### 8.4 Dashboard: states and copy (layout from the artboards)

A **DHCP** section in the nav with three screens.

- **Scopes**: a table (name, subnet, pool, leased / pool size, gateway, suffix) with create, edit, enable/disable, delete. Above it a status line: `Engine <version> · hot-standby with <peer> · <local state>` on a paired box, `Engine <version> · single` otherwise, `Engine unreachable` or `Config rejected: <message>` with an `Apply again` action when so.
- **Leases**: a live table (address, MAC, hostname, scope, expires in, `reserved` marker) refreshed on the poll interval; per row `Reserve` and `Release`.
- **Reservations**: a table (scope, address, MAC, hostname, comment) with create, edit, delete.

Warning strip facts (they clear on their own): `DHCP engine unreachable`, `DHCP config rejected: <message>`, `DHCP partner unreachable` (HA `communication-interrupted`).

Replica mode: the three screens follow the config-sync rule — write controls disabled with `Managed by the main`; `Release` stays live (a lease belongs to the engine, not to config).

Copy stays plain, per the project rule; the reasoning lives in `docs/dashboard.md`.

## 9. Security

- DHCP has no authentication; a rogue server on the LAN is the switch's problem (DHCP snooping), not dnsaur's.
- The control socket is local; dnsaur needs read/write on it, which means running as a user in Kea's group or the socket directory permitted to it. The docs state the exact `chmod`/group line for Debian and Alpine.
- The HA channel between the two Keas is HTTP on the LAN. It carries leases (addresses, MACs, hostnames), not credentials. Kea supports TLS and basic auth on that channel; dnsaur renders neither in this milestone and says so.
- `config-set` carries no secrets. Reservations and scopes are LAN topology, visible to any authenticated dashboard user, as today's zones are.

## 10. Failure modes

| what | behaviour |
|---|---|
| `dhcp.kea_socket` empty | nothing DHCP runs; the section is hidden; `dhcp/status` says `enabled: false` |
| socket missing or Kea down at start | status `engine unreachable`; the poller retries every poll interval; scopes stay editable; the first successful connection renders |
| Kea refuses the rendered config | previous config stays in force; `dhcp.last_error` set; strip and Scopes page show the message; `Apply again` re-sends |
| hook library missing | same as refused; the message names the file |
| Kea restarts | leases come back from its memfile; dnsaur's next poll refreshes the table; the last rendered config is sent again when the socket reappears |
| HA partner unreachable | Kea handles it (the primary keeps serving; the standby takes over after `max-response-delay`); the strip shows `DHCP partner unreachable` |
| both boxes partitioned from each other but reachable by clients | Kea's hot-standby rules: the standby serves only after unacked clients exceed the threshold; on reconnection leases reconcile by Kea's rules |
| a replica pairs later or is forgotten | the next render on both boxes adds or drops the standby peer |
| scope edit that shrinks a pool below live leases | Kea keeps existing leases until expiry; the Scopes page shows leased > pool size in the muted style |
| two devices claim one hostname | newer lease keeps the name; logged |

## 11. Not implemented, and why

- Managing the Kea process (start, stop, upgrade): it is the operator's service unit. dnsaur documents the unit and the container image variant.
- DHCPv6 and router advertisements: Kea 6 exists; nothing in this milestone needs it.
- Client classes, vendor-class matching, option 82 policies, ping check (the `ping_check` hook is not in Debian's 2.6 package), exclusions (multiple pools per scope): later. Generic options and the PXE trio are in (§4.3), after a 2026-09-14 comparison with Technitium's scope form.
- More than one standby: Kea HA supports it in load-balancing mode; not needed for a main and a backup.
- Reservations outside a scope's CIDR, multiple pools per scope.

## 12. Compatibility notes

- Kea versions: 2.6 (Debian trixie) and 3.0 (Alpine edge) are the tested range; the renderer emits only keys present in both, except the control-socket spelling (§6).
- Debian's build restricts paths: sockets under `/run/kea`, logs under `/var/log/kea`, lease files under `/var/lib/kea`; the docs' unit files respect that.
- Containers: host networking for both processes; one image variant ships Kea beside dnsaur with a supervisor, the plain image assumes Kea on the host.

## 13. Test posture

- Renderer: golden tests from scopes/reservations/settings to the `Dhcp4` JSON, including the HA section for a paired main, a paired replica, and an unpaired box; Alpine and Debian hook directories.
- Engine client: a fake control socket (unix listener speaking `{"command":…}` / `{"result":…}`) covering `config-set` accept and refuse, `lease4-get-page` paging, `status-get`, and a socket that disappears mid-poll.
- Lease table, DNS stage, and `mac` matcher: seeded tables, both drivers where a store is involved.
- Real engine: one test gated on `DNSAUR_TEST_KEA_SOCKET`, run in CI on the medium runner with Kea from the Debian package in a container, driving the real `config-set` and `lease4-add`/`lease4-get-page` the spike drove by hand.
- Loopback main+replica: both render the same HA pair from one pairing, and forgetting the replica drops it on both.

## 14. Task shape

1. Store: `dhcp_scopes`, `dhcp_reservations` (migration 0016), bundle export/import, version bumps, validators.
2. Engine client and renderer (`internal/dhcp`): socket client, `Dhcp4` rendering, hook directory discovery, `config-set` with error capture.
3. Lease poller and table; status; `dhcp` DNS stage; `mac` matcher.
4. API routes, `ResolverStatus.dhcp`, replica guards, openapi, docs/api.
5. Dashboard (after artboards): Scopes, Leases, Reservations, status line, strip facts.
6. Docs: configuration (install Kea on Debian/Alpine, socket permissions, HA transport per version, container variant), architecture, README row; CI job with real Kea.
