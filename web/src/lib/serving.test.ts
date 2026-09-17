import { expect, test } from "vitest";
import type {
  DHCPHAStatus,
  DHCPStatus,
  ProtocolStatus,
  ResolverStatus,
  SyncReplica,
} from "../api/types";
import {
  daysUntil,
  dhcpBanners,
  expiringSoonDetail,
  formatCertDate,
  servingState,
  somethingIsWrong,
  syncBanners,
} from "./serving";

test("servingState reads off intent and reality, not just one bool", () => {
  const off: ProtocolStatus = { enabled: false, listening: false, addr: "" };
  const failed: ProtocolStatus = {
    enabled: true,
    listening: false,
    addr: ":853",
    error: "bind: permission denied",
  };
  const listening: ProtocolStatus = { enabled: true, listening: true, addr: ":853" };

  expect(servingState(off)).toBe("off");
  expect(servingState(failed)).toBe("failed");
  expect(servingState(listening)).toBe("listening");

  // Disabled always reads "off", even if `listening` were somehow true —
  // enabled is the gate, not a fact read in isolation.
  expect(servingState({ enabled: false, listening: true, addr: ":853" })).toBe("off");
});

test("formatCertDate matches the artboard's exact shape, in UTC, regardless of locale month spelling", () => {
  expect(formatCertDate(new Date("2026-11-14T00:00:00Z"))).toBe("14 Nov 2026");
  // September's short form is the one month whose locale-formatted
  // spelling varies ("Sep" vs "Sept") — the reason formatCertDate doesn't
  // use toLocaleDateString at all. Pinned specifically.
  expect(formatCertDate(new Date("2026-09-17T00:00:00Z"))).toBe("17 Sep 2026");
  // A single-digit day gets no leading zero — "3 Jan 2027", not "03".
  expect(formatCertDate(new Date("2027-01-03T00:00:00Z"))).toBe("3 Jan 2027");
});

test("formatCertDate reads the UTC calendar day, not the host's local one", () => {
  // 23:30 UTC on the 13th is still the 14th in timezones ahead of UTC, and
  // still the 13th in timezones behind it — using local getters would make
  // this test's answer depend on the machine running it.
  expect(formatCertDate(new Date("2026-11-13T23:30:00Z"))).toBe("13 Nov 2026");
});

test("daysUntil floors at 0 for a certificate that has already lapsed", () => {
  const now = new Date("2026-09-08T00:00:00Z");
  expect(daysUntil(new Date("2026-09-17T00:00:00Z"), now)).toBe(9);
  expect(daysUntil(new Date("2026-09-01T00:00:00Z"), now)).toBe(0);
});

test("expiringSoonDetail pairs the day count with the formatted date, from one not_after", () => {
  // `now` is passed rather than left to the real clock: daysUntil floors,
  // so an expiry built as "exactly 5 days from Date.now()" lands on 4 the
  // moment any wall-clock time passes between the two calls. Callers in
  // the app still let it default, which is what they want.
  const now = new Date("2026-09-08T00:00:00Z");
  const notAfter = new Date("2026-09-13T00:00:00Z");
  const detail = expiringSoonDetail(
    { not_after: notAfter.toISOString(), expiring_soon: true },
    now,
  );
  expect(detail.days).toBe(5);
  expect(detail.date).toBe(formatCertDate(notAfter));
});

test("daysUntil floors rather than rounds, so a part-day is not a whole one", () => {
  const now = new Date("2026-09-08T00:00:00Z");
  // 36 hours out is one whole day remaining, not two: rounding gave the
  // operator a day of grace they did not have.
  expect(daysUntil(new Date("2026-09-09T12:00:00Z"), now)).toBe(1);
  expect(daysUntil(new Date("2026-09-08T23:59:00Z"), now)).toBe(0);
  expect(daysUntil(new Date("2026-09-10T00:00:00Z"), now)).toBe(2);
});

test("expiringSoonDetail says whether the certificate has already lapsed", () => {
  const now = new Date("2026-09-08T00:00:00Z");
  // The server reports a lapsed certificate as ok=true, expiring_soon=true
  // (internal/app/serve.go's CertExpiry), so the caller cannot tell "about
  // to expire" from "expired" without this.
  expect(
    expiringSoonDetail({ not_after: "2026-09-01T00:00:00Z", expiring_soon: true }, now),
  ).toEqual({ days: 0, date: "1 Sep 2026", expired: true });
  expect(
    expiringSoonDetail({ not_after: "2026-09-17T00:00:00Z", expiring_soon: true }, now),
  ).toEqual({ days: 9, date: "17 Sep 2026", expired: false });
});

// somethingIsWrong is what the status poll and the shell banners both read.
// It was `encryption_downgraded` alone, which was every fact the endpoint
// carried when the poll was written and is now one of three.
test("somethingIsWrong covers every fact the status endpoint reports", () => {
  const clean: ResolverStatus = {
    encryption_downgraded: false,
    reason: "",
    serving: {
      dot: { enabled: false, listening: false, addr: "" },
      doh: { enabled: false, listening: false, addr: "" },
    },
  };
  expect(somethingIsWrong(clean)).toBe(false);
  // An unknown status is not a wrong one — the retry policy for a failed
  // fetch belongs to the query, not here.
  expect(somethingIsWrong(undefined)).toBe(false);

  expect(somethingIsWrong({ ...clean, encryption_downgraded: true })).toBe(true);
  expect(
    somethingIsWrong({
      ...clean,
      serving: { ...clean.serving, dot: { enabled: true, listening: false, addr: ":853" } },
    }),
  ).toBe(true);
  expect(
    somethingIsWrong({
      ...clean,
      serving: { ...clean.serving, doh: { enabled: true, listening: false, addr: ":443" } },
    }),
  ).toBe(true);
  expect(
    somethingIsWrong({
      ...clean,
      certificate: { not_after: "2026-09-17T00:00:00Z", expiring_soon: true },
    }),
  ).toBe(true);
  // Sync's two *watchable* facts: a failed pull, and a replica gone quiet.
  // Both end by themselves — the next successful pull, the next check-in —
  // so the poll is what clears them.
  expect(somethingIsWrong({ ...clean, sync: { role: "replica", last_error: "boom" } })).toBe(true);
  expect(
    somethingIsWrong({
      ...clean,
      sync: { role: "main", replicas: [replica({ stale: true })] },
    }),
  ).toBe(true);

  // `plain_http` is not one of them. It is a fact about the peer URL the
  // operator typed, and nothing but editing that URL will change it — so
  // polling for it every five seconds asks a question whose answer cannot
  // move. The banner still shows (see syncBanners below); only the poll
  // stays off.
  expect(somethingIsWrong({ ...clean, sync: { role: "replica", plain_http: true } })).toBe(false);

  // DHCP's three, on the same terms: all of them end without anyone doing
  // anything, so the poll is what takes their lines down.
  expect(somethingIsWrong({ ...clean, dhcp: dhcp({ engine: "unreachable" }) })).toBe(true);
  expect(
    somethingIsWrong({ ...clean, dhcp: dhcp({ engine: "config rejected", message: "no" }) }),
  ).toBe(true);
  expect(
    somethingIsWrong({
      ...clean,
      dhcp: dhcp({ ha: ha({ communication_interrupted: true }) }),
    }),
  ).toBe(true);

  // A listening protocol, a healthy certificate, a replica that checked in
  // and a healthy engine are not "wrong", or the poll would never stop.
  expect(
    somethingIsWrong({
      ...clean,
      serving: { ...clean.serving, dot: { enabled: true, listening: true, addr: ":853" } },
      certificate: { not_after: "2027-09-17T00:00:00Z", expiring_soon: false },
      sync: { role: "main", replicas: [replica()] },
      dhcp: dhcp({ ha: ha() }),
    }),
  ).toBe(false);
  // And a box with no engine is the normal case, not a fault: `enabled`
  // false is the whole answer most instances give.
  expect(
    somethingIsWrong({ ...clean, dhcp: { enabled: false, table_age_seconds: 0, scopes: [] } }),
  ).toBe(false);
});

function dhcp(overrides: Partial<DHCPStatus> = {}): DHCPStatus {
  return {
    enabled: true,
    engine: "ok",
    engine_version: "2.6.3",
    message: "",
    table_age_seconds: 3,
    scopes: [],
    ...overrides,
  };
}

function ha(overrides: Partial<DHCPHAStatus> = {}): DHCPHAStatus {
  return {
    mode: "hot-standby",
    local_state: "hot-standby",
    peer: "backup-box",
    remote_state: "hot-standby",
    communication_interrupted: false,
    unacked_clients: 0,
    ...overrides,
  };
}

// The three lines spec §8.4 names, verbatim. Only failures: DHCP being off
// is what most instances are, and a healthy pair is the configuration
// working — a strip that warned about either is a strip nobody reads.
test("dhcpBanners states each failure once, in the engine's own words", () => {
  expect(dhcpBanners(undefined)).toEqual([]);
  expect(dhcpBanners({ enabled: false, table_age_seconds: 0, scopes: [] })).toEqual([]);
  expect(dhcpBanners(dhcp())).toEqual([]);
  expect(dhcpBanners(dhcp({ ha: ha() }))).toEqual([]);

  expect(dhcpBanners(dhcp({ engine: "unreachable" }))).toEqual(["DHCP engine unreachable"]);
  expect(
    dhcpBanners(
      dhcp({
        engine: "config rejected",
        message: "subnet4[1]: pool 192.168.150.100-192.168.150.199 is not in subnet",
      }),
    ),
  ).toEqual([
    "DHCP config rejected: subnet4[1]: pool 192.168.150.100-192.168.150.199 is not in subnet",
  ]);
  expect(dhcpBanners(dhcp({ ha: ha({ communication_interrupted: true }) }))).toEqual([
    "DHCP partner unreachable",
  ]);

  // Independent facts, shown together: the engine can be gone while the
  // partner is also unreachable, and one line would hide the other.
  expect(
    dhcpBanners(dhcp({ engine: "unreachable", ha: ha({ communication_interrupted: true }) })),
  ).toEqual(["DHCP engine unreachable", "DHCP partner unreachable"]);

  // Where the page shows the engine's own line, the strip does not repeat
  // it; the partner fact is not on that line, so it stays.
  expect(dhcpBanners(dhcp({ engine: "unreachable" }), true)).toEqual([]);
  expect(dhcpBanners(dhcp({ engine: "config rejected", message: "no" }), true)).toEqual([]);
  expect(
    dhcpBanners(dhcp({ engine: "unreachable", ha: ha({ communication_interrupted: true }) }), true),
  ).toEqual(["DHCP partner unreachable"]);
});

function replica(overrides: Partial<SyncReplica> = {}): SyncReplica {
  return {
    instance_id: "eve-2",
    dns_addr: "10.0.0.6:53",
    version_applied: 412,
    last_seen: Date.now() - 60_000,
    stale: false,
    dhcp: true,
    ...overrides,
  };
}

// The exact lines the shell renders, pinned here rather than only through
// the shell, because the durations are arithmetic and a banner test that
// matched loosely would not have caught "56 years".
test("syncBanners states each fact once, and says how long a stale replica has been quiet", () => {
  const now = Date.UTC(2026, 8, 12, 12, 0, 0);
  expect(syncBanners(undefined, now)).toEqual([]);
  expect(syncBanners({ role: "main" }, now)).toEqual([]);
  expect(
    syncBanners(
      {
        role: "replica",
        peer_url: "http://main.lan",
        last_error: "dial tcp: connection refused",
        plain_http: true,
      },
      now,
    ),
  ).toEqual(["Last pull failed: dial tcp: connection refused"]);
  // A main's `last_error` is not a pull: it is `sync.replicas`, the one row
  // its whole registry lives in, unreadable — so the line may not say the
  // box failed to pull from a main it does not have.
  expect(
    syncBanners(
      { role: "main", last_error: "sync.replicas is not the JSON this build wrote" },
      now,
    ),
  ).toEqual(["Sync: sync.replicas is not the JSON this build wrote"]);
  // A plaintext peer is a reading of the URL in the Sync band, so it is
  // stated there and nowhere else — the strip is for the two facts that
  // clear on their own.
  expect(
    syncBanners({ role: "replica", peer_url: "http://main.lan", plain_http: true }, now),
  ).toEqual([]);
  expect(
    syncBanners(
      {
        role: "main",
        replicas: [
          replica({ instance_id: "eve-2", stale: true, last_seen: now - 95 * 60_000 }),
          replica({ instance_id: "eve-3", stale: false, last_seen: now - 30_000 }),
        ],
      },
      now,
    ),
  ).toEqual(["Replica eve-2 not seen for 1h 35m"]);
});
