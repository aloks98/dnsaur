import { describe, expect, it } from "vitest";
import { notifyRollup, parseNotifyTo, notifyKeyNames } from "./notify";

describe("parseNotifyTo", () => {
  it("accepts what the Go parser accepts", () => {
    expect(parseNotifyTo("10.0.0.2")).toEqual({
      ok: true,
      targets: [{ host: "10.0.0.2", port: 53, key: "" }],
    });
    expect(parseNotifyTo("ns2.example.com:5353 key:Hetzner-Xfer")).toEqual({
      ok: true,
      targets: [{ host: "ns2.example.com", port: 5353, key: "hetzner-xfer." }],
    });
    expect(parseNotifyTo("")).toEqual({ ok: true, targets: [] });
  });

  // Bracketed IPv6 with an explicit port, and a bare IPv6 literal with none
  // — the two shapes acl.ts's own address parser already has to handle,
  // exercised here through notify_to's host[:port] grammar instead of an
  // ACL's bare address/prefix.
  it("accepts an IPv6 literal, bracketed with a port or bare without one", () => {
    expect(parseNotifyTo("[fd00::2]:5353 key:hetzner-xfer")).toEqual({
      ok: true,
      targets: [{ host: "fd00::2", port: 5353, key: "hetzner-xfer." }],
    });
    expect(parseNotifyTo("fd00::2")).toEqual({
      ok: true,
      targets: [{ host: "fd00::2", port: 53, key: "" }],
    });
  });

  // The brief's own version of this test called `expect(actual, bad)` — a
  // second argument oxlint's vitest(valid-expect) rule rejects outright
  // ("Expect takes at most 1 argument"). it.each keeps the same five cases
  // and the same diagnostic value (which input failed shows in the test
  // name) without tripping the rule.
  it.each(["10.0.0.2:0", "10.0.0.2:70000", "not a host", "10.0.0.2 key:", "key:only"])(
    "rejects %j",
    (bad) => {
      expect(parseNotifyTo(bad).ok).toBe(false);
    },
  );

  // Regression: splitHostPort's non-bracket branch used to check only the
  // *port* substring for a stray ']', not the whole field the way Go's
  // net.SplitHostPort does (it checks from index 0 whenever the bracket
  // branch hasn't moved that start index) — so "abc]:1234" read as
  // {host: "abc]", port: "1234"} instead of being rejected outright. Go
  // itself refuses this ("unexpected ']' in address"), verified against
  // the real net.SplitHostPort, not assumed.
  it("rejects a host carrying a stray ']'", () => {
    expect(parseNotifyTo("abc]:1234").ok).toBe(false);
  });
});

describe("notifyKeyNames", () => {
  it("returns canonical names in order, and none from an unparseable value", () => {
    expect(notifyKeyNames("10.0.0.2 key:B, 10.0.0.3, ns4.example.com key:a")).toEqual(["b.", "a."]);
    expect(notifyKeyNames("10.0.0.2 key:bad name")).toEqual([]);
  });
});

// The one piece of screen logic that can be *wrong* rather than merely ugly.
describe("notifyRollup", () => {
  const current = { state: "current" } as const;
  const behind = { state: "gave_up" } as const;
  const retrying = { state: "retrying" } as const;
  const never = { state: "never" } as const;

  it("says no targets when there are none", () => {
    expect(notifyRollup([])).toEqual({ label: "no targets", behind: 0, total: 0 });
  });

  it("says all N current when nothing is behind", () => {
    expect(notifyRollup([current, current, current, current])).toEqual({
      label: "all 4 current",
      behind: 0,
      total: 4,
    });
  });

  it("counts retrying and gave_up as behind, and never as not", () => {
    expect(notifyRollup([current, behind, retrying, never])).toEqual({
      label: "2 of 4 behind",
      behind: 2,
      total: 4,
    });
  });

  // Decided 2026-09-02 by the design's author, overriding the artboard.
  //
  // The artboard's own arithmetic emits "all N current" whenever nothing is
  // behind — so a zone that has just gained a target reads "all 4 current"
  // while one of them has never been told anything. That is a false
  // statement on a row whose whole job is being scannable, and this
  // project's copy rule is to state the fact and stop.
  //
  // `behind` still takes precedence when both are present: it is the
  // actionable signal, and naming both would make the line a paragraph.
  it("names never-notified targets rather than calling them current", () => {
    expect(notifyRollup([current, current, current, never])).toEqual({
      label: "3 of 4 current, 1 never notified",
      behind: 0,
      total: 4,
    });
  });

  it("still leads with behind when both are present", () => {
    expect(notifyRollup([current, never, behind]).label).toBe("1 of 3 behind");
  });

  it("says all N current only when every target really is", () => {
    expect(notifyRollup([current, current]).label).toBe("all 2 current");
  });

  it("handles a single target without pluralising wrongly", () => {
    expect(notifyRollup([current]).label).toBe("all 1 current");
    expect(notifyRollup([behind]).label).toBe("1 of 1 behind");
  });
});
