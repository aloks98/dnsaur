import { expect, test } from "vitest";
import type { ProtocolStatus, ResolverStatus } from "../api/types";
import {
  daysUntil,
  expiringSoonDetail,
  formatCertDate,
  servingState,
  somethingIsWrong,
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
  // A listening protocol and a healthy certificate are not "wrong", or the
  // poll would never stop.
  expect(
    somethingIsWrong({
      ...clean,
      serving: { ...clean.serving, dot: { enabled: true, listening: true, addr: ":853" } },
      certificate: { not_after: "2027-09-17T00:00:00Z", expiring_soon: false },
    }),
  ).toBe(false);
});
