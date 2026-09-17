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
  expiringSoonDetail,
  FACT_TARGETS,
  factBand,
  formatCertDate,
  servingState,
  somethingIsWrong,
  statusFacts,
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

// somethingIsWrong is what the status poll reads, defined as "statusFacts
// found something".
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
  // move. It is not a panel row either — it is stated once, in the Sync
  // band beside the peer it describes.
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

// The exact rows the status panel renders, pinned here rather than only
// through the panel, because the durations are arithmetic and a row test
// that matched loosely would not have caught "56 years", and because the
// ids are what decide whether a row keeps its place between polls.

const NOW = Date.UTC(2026, 8, 16, 12, 0, 0);

/** Everything the endpoint can report going wrong at once, so one call
 * answers for all ten facts. `engine` can only hold one value, so the
 * rejected configuration and the unreachable engine are separate cases. */
function allWrong(overrides: Partial<ResolverStatus> = {}): ResolverStatus {
  return {
    encryption_downgraded: true,
    reason: 'unexpected character ";" at position 14',
    serving: {
      dot: {
        enabled: true,
        listening: false,
        addr: ":853",
        error: "listen tcp :853: bind: permission denied",
      },
      doh: { enabled: true, listening: false, addr: ":443", error: "listen tcp :443: bind: x" },
    },
    certificate: { not_after: "2026-09-28T00:00:00Z", expiring_soon: true },
    sync: {
      role: "main",
      last_error: "sync.replicas is not the JSON this build wrote",
      replicas: [replica({ instance_id: "2IB4ABB4", stale: true, last_seen: NOW - 3 * 3_600_000 })],
    },
    dhcp: dhcp({
      engine: "config rejected",
      message: "subnet4[1]: pool 192.168.150.100-192.168.150.199 is not in subnet",
      ha: ha({ communication_interrupted: true }),
    }),
    ...overrides,
  };
}

test("statusFacts states each fact once, in the server's own words", () => {
  expect(statusFacts(undefined, NOW)).toEqual([]);

  const facts = statusFacts(allWrong(), NOW);
  expect(facts.map((f) => [f.id, f.text, f.sub])).toEqual([
    [
      "encryption",
      'Encryption is off — upstreams could not be parsed, unexpected character ";" at position 14',
      undefined,
    ],
    [
      "dot",
      "DNS-over-TLS is enabled but not listening",
      "listen tcp :853: bind: permission denied",
    ],
    ["doh", "DNS-over-HTTPS is enabled but not listening", "listen tcp :443: bind: x"],
    ["certificate", "TLS certificate expires in 11 days — 28 Sep 2026", undefined],
    ["sync-error", "Sync: sync.replicas is not the JSON this build wrote", undefined],
    ["replica-stale:2IB4ABB4", "Replica 2IB4ABB4 not seen for 3h", undefined],
    [
      "dhcp-config",
      "DHCP config rejected: subnet4[1]: pool 192.168.150.100-192.168.150.199 is not in subnet",
      undefined,
    ],
    ["dhcp-partner", "DHCP partner unreachable", undefined],
  ]);
});

// The listener's error is the row's muted second line, not part of the
// sentence: the fact is one line an operator recognises, and three lines of
// Go under it is detail.
test("a bind error is the row's second line, and a listener with none still gets a row", () => {
  const status = allWrong({
    serving: {
      dot: { enabled: true, listening: false, addr: ":853" },
      doh: { enabled: false, listening: false, addr: "" },
    },
  });
  const dot = statusFacts(status, NOW).find((f) => f.id === "dot");
  expect(dot?.text).toBe("DNS-over-TLS is enabled but not listening");
  expect(dot?.sub).toBeUndefined();
});

// The one red fact, and the only one that is not amber: every query is
// going out in the clear. With no reason from the server the sentence keeps
// the tail the banner carried before the endpoint reported one.
test("encryption is the red fact and carries the server's reason, or the old tail without one", () => {
  const facts = statusFacts(allWrong(), NOW);
  expect(facts.filter((f) => f.tone === "red").map((f) => f.id)).toEqual(["encryption"]);
  expect(statusFacts(allWrong({ reason: "" }), NOW)[0].text).toBe(
    "Encryption is off — upstreams could not be parsed, falling back to plaintext resolvers",
  );
});

test("every fact links to the band that fixes it", () => {
  const targets = Object.fromEntries(statusFacts(allWrong(), NOW).map((f) => [f.id, f.target]));
  expect(targets).toEqual({
    encryption: "/settings#upstreams",
    dot: "/settings#protocols",
    doh: "/settings#protocols",
    certificate: "/settings#protocols",
    "sync-error": "/settings#sync",
    "replica-stale:2IB4ABB4": "/settings#sync",
    "dhcp-config": "/dhcp",
    "dhcp-partner": "/dhcp",
  });
  // Every target the facts use has a name for the row that links to it.
  for (const fact of statusFacts(allWrong({ dhcp: dhcp({ engine: "unreachable" }) }), NOW)) {
    expect(FACT_TARGETS[fact.target]).toBeTruthy();
  }
});

// The middle of the panel's three bands: a fact that stands until somebody
// edits something outranks one that will end on its own. It is a claim
// about the fact, not about its tone — which is why a refused DHCP
// configuration and an unreachable engine part company here.
test("waitsOnOperator splits the facts nobody can wait out from the ones that clear", () => {
  const waits = (status: ResolverStatus) =>
    Object.fromEntries(statusFacts(status, NOW).map((f) => [f.id, f.waitsOnOperator]));

  expect(waits(allWrong())).toEqual({
    encryption: true,
    dot: true,
    doh: true,
    certificate: true,
    "sync-error": true,
    "replica-stale:2IB4ABB4": false,
    "dhcp-config": true,
    "dhcp-partner": false,
  });
  // The same `last_error` field, two facts: a main's registry row waits for
  // somebody to repair it, a replica's failed pull ends at the next one
  // that succeeds.
  const asReplica = allWrong({ sync: { role: "replica", last_error: "dial tcp: refused" } });
  expect(statusFacts(asReplica, NOW).find((f) => f.id === "sync-error")).toEqual({
    id: "sync-error",
    tone: "amber",
    text: "Last pull failed: dial tcp: refused",
    target: "/settings#sync",
    waitsOnOperator: false,
  });
  // An engine that is not there comes back by itself; the configuration it
  // refused does not.
  expect(waits(allWrong({ dhcp: dhcp({ engine: "unreachable" }) }))["dhcp-engine"]).toBe(false);
});

// Emission order is not what the panel shows — it groups by tone and
// waitsOnOperator first — but it is the tie-breaker inside a group, so on a
// fresh load it is what makes the rows come out in the boards' order.
test("emission order is by subject, and the panel's grouping turns it into the boards' order", () => {
  const facts = statusFacts(allWrong({ dhcp: dhcp({ engine: "unreachable" }) }), NOW);
  // The panel's own band function, not a copy of it: a copy would keep this
  // test passing while the rows on screen came out in another order.
  const shown = facts
    .map((fact, seq) => ({ fact, seq }))
    .sort((a, b) => factBand(a.fact) - factBand(b.fact) || a.seq - b.seq)
    .map(({ fact }) => fact.id);
  expect(shown).toEqual([
    "encryption",
    "dot",
    "doh",
    "certificate",
    "sync-error",
    "replica-stale:2IB4ABB4",
    "dhcp-engine",
  ]);
});

// The strip used to take a flag saying "the Scopes page is showing the
// engine's own line, so drop the two facts that line repeats". The panel is
// one list on every screen — nothing stacks above the page any more — so
// the flag is gone and the row for a refused configuration links to that
// line instead of hiding for it.
test("the facts do not depend on which page is open", () => {
  const rejected = allWrong({
    dhcp: dhcp({ engine: "config rejected", message: "no local address is inside 10.0.0.0/16" }),
  });
  expect(statusFacts(rejected, NOW).map((f) => f.id)).toContain("dhcp-config");
});

// Only failures. DHCP being off is what most instances are, a healthy pair
// is the configuration working, and being a replica is a state — a panel
// that also carried the normal case is a panel an operator learns to skip.
test("nothing that is merely the normal case becomes a fact", () => {
  const clean: ResolverStatus = {
    encryption_downgraded: false,
    reason: "",
    serving: {
      dot: { enabled: true, listening: true, addr: ":853" },
      doh: { enabled: false, listening: false, addr: "" },
    },
    certificate: { not_after: "2027-09-17T00:00:00Z", expiring_soon: false },
    sync: { role: "replica", peer_url: "http://main.lan", plain_http: true, replicas: [replica()] },
    dhcp: dhcp({ ha: ha() }),
  };
  expect(statusFacts(clean, NOW)).toEqual([]);
  expect(
    statusFacts({ ...clean, dhcp: { enabled: false, table_age_seconds: 0, scopes: [] } }, NOW),
  ).toEqual([]);
});
