import { expect, test } from "vitest";
import type { Zone } from "../api/types";
import {
  isServing,
  lastTransferError,
  MIN_REFRESH_INTERVAL_MS,
  nextRefreshAt,
  refreshIntervalMs,
  retryIntervalMs,
  transferErrorLead,
  transferState,
  type TransferState,
} from "./zones";

/**
 * These are the derivations both zone screens read their whole vocabulary
 * from — which state a secondary is in, whether it can answer at all, and
 * when it will next be asked. They are tested here rather than only through
 * the pages because a page test can only reach a state it happens to set up:
 * a branch nobody wrote a fixture for reads as covered while being dead.
 *
 * Every fact used is a column, deliberately (see the module's own comment),
 * so every fixture here is just a row.
 */

/** Fixed, so "now" is a number in the test rather than a moving target. */
const NOW = 1_754_000_000_000;
const HOUR = 3_600_000;
const DAY = 24 * HOUR;

/** A healthy secondary: refreshed an hour ago on a 7200s schedule. */
function secondary(): Zone {
  return {
    id: 1,
    name: "e412.in",
    type: "secondary",
    enabled: true,
    soa_ns: "ns1.e412.in",
    soa_mbox: "hostadmin.e412.in",
    soa_serial: 7,
    soa_refresh: 7200,
    soa_retry: 3600,
    soa_expire: 1209600,
    soa_minimum: 900,
    soa_ttl: 900,
    primaries: "203.0.113.9",
    tsig_key_id: 0,
    expires_at: NOW + 7 * DAY,
    refreshed_at: NOW - HOUR,
    last_error: "",
    last_attempt: NOW - HOUR,
    allow_transfer: "",
    last_xfr_at: 0,
    last_xfr_peer: "",
    last_xfr_error: "",
    created_at: NOW - 30 * DAY,
    modified_at: NOW - 30 * DAY,
  } satisfies Zone;
}

function zoneWith(overrides: Partial<Zone>): Zone {
  return { ...secondary(), ...overrides };
}

// ── the schedule ────────────────────────────────────────────────────────────

// A primary can publish a refresh of 0 by accident, and this server clamps
// rather than obeying it (minInterval in internal/zones/refresh.go). Computing
// "next refresh" off the raw SOA value would promise a poll that never
// happens — and, for a zero, promise it continuously.
test("refresh and retry intervals are clamped to the floor the scheduler uses", () => {
  expect(refreshIntervalMs(zoneWith({ soa_refresh: 7200 }))).toBe(7_200_000);
  expect(refreshIntervalMs(zoneWith({ soa_refresh: 0 }))).toBe(MIN_REFRESH_INTERVAL_MS);
  expect(refreshIntervalMs(zoneWith({ soa_refresh: 1 }))).toBe(MIN_REFRESH_INTERVAL_MS);

  expect(retryIntervalMs(zoneWith({ soa_retry: 3600 }))).toBe(3_600_000);
  expect(retryIntervalMs(zoneWith({ soa_retry: 0 }))).toBe(MIN_REFRESH_INTERVAL_MS);
});

test("the next refresh is measured from the last success, not from now", () => {
  const zone = zoneWith({ refreshed_at: NOW - HOUR, soa_refresh: 7200 });
  expect(nextRefreshAt(zone)).toBe(NOW - HOUR + 7_200_000);
});

// ── the recorded failure ────────────────────────────────────────────────────

test("an empty last_error means the last attempt succeeded", () => {
  expect(lastTransferError(zoneWith({ last_error: "" }))).toBeNull();
});

test("a failure comes back with its date, never on its own", () => {
  const failure = lastTransferError(
    zoneWith({ last_error: "connection refused", last_attempt: NOW - 60_000 }),
  );
  expect(failure).toEqual({ message: "connection refused", at: NOW - 60_000 });
});

// The two columns are written together in one statement, so ordinarily they
// cannot disagree with refreshed_at. The exception is a success whose
// bookkeeping write failed — best-effort and logged, see
// Refresher.recordAttempt — which leaves an error older than the last success
// sitting in the column. Without this ordering, a problem that was fixed would
// sit on the screen forever.
test("an error older than the last success is not current, and is dropped", () => {
  expect(
    lastTransferError(
      zoneWith({
        last_error: "connection refused",
        last_attempt: NOW - 9 * HOUR,
        refreshed_at: NOW - HOUR,
      }),
    ),
  ).toBeNull();
});

// ── leading with the failure ────────────────────────────────────────────────
//
// The screens show one opaque string in two places: the part worth reading
// first, and the whole of it. The cut is a presentation split of text nobody
// promised the shape of, so every one of these also pins that a message it
// does not recognise comes back untouched rather than mangled.

test("a wrapped error leads with the failure, not with the context around it", () => {
  expect(
    transferErrorLead(
      'zone "lab.e412.in": every primary failed: 192.168.150.40:9353: dial tcp 192.168.150.40:9353: connect: connection refused',
    ),
  ).toBe("connection refused");
});

// The wording is not a contract. A message with no separator in it at all is
// the shape this will meet the first time the server rephrases itself, and it
// has to survive that by showing the whole thing rather than nothing.
test("a message with nothing to cut on comes back whole", () => {
  expect(transferErrorLead("transfer refused by server")).toBe("transfer refused by server");
  expect(transferErrorLead("")).toBe("");
  // A bare colon with no space is not a wrap — it is an address, and cutting
  // there would strip the port off it.
  expect(transferErrorLead("203.0.113.9:53 unreachable")).toBe("203.0.113.9:53 unreachable");
});

// Both halves have to be real, or there is nothing to gain by cutting: a
// message that trails off leaves no failure to lead with, and one that opens
// with the separator has no context to set aside.
test("a message with an empty half comes back whole", () => {
  expect(transferErrorLead("every primary failed: ")).toBe("every primary failed: ");
  expect(transferErrorLead(": connection refused")).toBe(": connection refused");
});

// ── the five states ─────────────────────────────────────────────────────────

test("a secondary refreshed within its schedule is fresh", () => {
  expect(transferState(secondary(), NOW)).toBe("fresh");
});

test("a secondary that has never transferred is never, whether or not one was tried", () => {
  expect(transferState(zoneWith({ refreshed_at: 0, expires_at: 0 }), NOW)).toBe("never");
  // Tried and failed is still `never`: the zone holds nothing either way, and
  // the failure shows up as the cause beside the state rather than as a state.
  expect(
    transferState(
      zoneWith({
        refreshed_at: 0,
        expires_at: 0,
        last_error: "connection refused",
        last_attempt: NOW - 60_000,
      }),
      NOW,
    ),
  ).toBe("never");
});

test("a secondary past its SOA expiry is expired", () => {
  expect(transferState(zoneWith({ expires_at: NOW - 1 }), NOW)).toBe("expired");
});

// Expired outranks failing because it is the stronger claim: a failing zone is
// still serving the copy it has, an expired one is serving nothing at all.
test("expired outranks failing when a zone is both", () => {
  expect(
    transferState(
      zoneWith({
        expires_at: NOW - 1,
        last_error: "connection refused",
        last_attempt: NOW - 60_000,
      }),
      NOW,
    ),
  ).toBe("expired");
});

test("a secondary whose last attempt failed is failing, and is still serving", () => {
  const zone = zoneWith({ last_error: "connection refused", last_attempt: NOW - 60_000 });
  expect(transferState(zone, NOW)).toBe("failing");
  expect(isServing(zone, NOW)).toBe(true);
});

/**
 * `overdue` is the state this codebase added beyond the design boards, and the
 * one with no analogue in them: the refresh deadline came and went with *no
 * failure recorded against it*, so nothing is claiming to have tried. It is
 * the scheduler not having reached the zone — it is disabled and skipped
 * outright (refresh.go's RefreshDue), or the process has only just started.
 *
 * Without it, both of those read as `fresh`, which is a zone serving a copy of
 * unknown age while the screen calls it current.
 */
test("a secondary past its refresh deadline with nothing recorded is overdue", () => {
  const zone = zoneWith({
    soa_refresh: 7200,
    refreshed_at: NOW - 3 * HOUR,
    last_attempt: NOW - 3 * HOUR,
  });
  expect(transferState(zone, NOW)).toBe("overdue");
  // Overdue is not a reason to stop answering: the copy is old, not invalid.
  expect(isServing(zone, NOW)).toBe(true);
});

// The scheduler asks which zones are due every 30s, so a zone a moment past
// its deadline is waiting for the next tick rather than failing. Without the
// grace the detail page would flicker into "overdue" once per refresh interval
// on a perfectly healthy zone.
test("a zone inside one scheduler tick of its deadline is still fresh", () => {
  const dueNow = zoneWith({ soa_refresh: 7200, refreshed_at: NOW - 7_200_000 });
  expect(transferState(dueNow, NOW)).toBe("fresh");
  expect(transferState(dueNow, NOW + 29_000)).toBe("fresh");
  expect(transferState(dueNow, NOW + 31_000)).toBe("overdue");
});

// Total and mutually exclusive by construction: every row lands in exactly one
// state, and each of the five is reachable.
test("the five states are each reachable and every zone lands in exactly one", () => {
  const cases: [TransferState, Zone][] = [
    ["never", zoneWith({ refreshed_at: 0, expires_at: 0 })],
    ["expired", zoneWith({ expires_at: NOW - 1 })],
    ["failing", zoneWith({ last_error: "connection refused", last_attempt: NOW - 60_000 })],
    ["overdue", zoneWith({ refreshed_at: NOW - 3 * HOUR, last_attempt: NOW - 3 * HOUR })],
    ["fresh", secondary()],
  ];
  expect(cases.map(([, zone]) => transferState(zone, NOW))).toEqual(cases.map(([want]) => want));
});

// ── serving ─────────────────────────────────────────────────────────────────

// The exact negation of Zone.Serving (internal/zones/answer.go): only a
// secondary can fail it, and only in the two states where it holds nothing it
// may speak for.
test("only a secondary can be not-serving, and only when it holds nothing it may speak for", () => {
  for (const type of ["primary", "internal", "stub", "forwarder"] as const) {
    // The same row that would make a secondary not-serving.
    expect(isServing(zoneWith({ type, refreshed_at: 0, expires_at: 0 }), NOW)).toBe(true);
  }
  expect(isServing(zoneWith({ refreshed_at: 0, expires_at: 0 }), NOW)).toBe(false);
  expect(isServing(zoneWith({ expires_at: NOW - 1 }), NOW)).toBe(false);
  expect(isServing(secondary(), NOW)).toBe(true);
});

// expires_at is 0 until a transfer records a deadline, which is why
// Zone.Serving guards the comparison rather than testing `now >= expires_at`
// outright — a zone that has transferred but carries no deadline must not read
// as expired at every moment since 1970.
test("a zone that transferred but carries no expiry deadline is serving", () => {
  expect(isServing(zoneWith({ refreshed_at: NOW - HOUR, expires_at: 0 }), NOW)).toBe(true);
});
