import { describe, expect, test } from "vitest";
import { aclKeyNames, parseACL } from "./acl";

// Mirrors internal/zones/acl_test.go's table — this is a client-side port of
// the same rules (see acl.ts's own comment on why, and on the server being
// the one that actually decides). Not every row of the Go table is repeated
// here; the ones that exercise a genuinely different code path are.

describe("parseACL", () => {
  test("empty is valid and means deny, not an error", () => {
    const result = parseACL("");
    expect(result).toEqual({ ok: true, entries: [] });
  });

  test("only separators is empty, not an error", () => {
    const result = parseACL(" , , ");
    expect(result).toEqual({ ok: true, entries: [] });
  });

  test("a CIDR prefix parses", () => {
    const result = parseACL("10.0.0.0/24");
    expect(result.ok).toBe(true);
    if (!result.ok) return;
    expect(result.entries).toEqual([{ kind: "prefix", value: "10.0.0.0/24" }]);
  });

  test("a bare address parses as a host route", () => {
    const result = parseACL("192.168.1.5");
    expect(result.ok).toBe(true);
    if (!result.ok) return;
    expect(result.entries).toEqual([{ kind: "prefix", value: "192.168.1.5" }]);
  });

  // Host bits are masked off, the way Go's Prefix.Masked() stores the range
  // actually matched rather than keeping bits Prefix.Contains ignores and a
  // reader does not — the one path in this parser that transforms a value
  // rather than merely validating it.
  test("a CIDR with host bits set is masked to the network it names", () => {
    const result = parseACL("10.0.0.5/24");
    expect(result.ok).toBe(true);
    if (!result.ok) return;
    expect(result.entries).toEqual([{ kind: "prefix", value: "10.0.0.0/24" }]);
  });

  // The exact spelling of a v6 value is not asserted: this file doesn't
  // re-canonicalise for display (see its own top comment), so all a bare
  // v6 address has to do is parse.
  test("a bare IPv6 address parses", () => {
    const result = parseACL("2001:db8::5");
    expect(result.ok).toBe(true);
    if (!result.ok) return;
    expect(result.entries).toEqual([{ kind: "prefix", value: expect.any(String) }]);
  });

  test("a key entry is canonicalised: lowercased, trailing dot added", () => {
    const result = parseACL("key:NS2");
    expect(result.ok).toBe(true);
    if (!result.ok) return;
    expect(result.entries).toEqual([{ kind: "key", name: "ns2." }]);
  });

  test("the key: prefix is case-insensitive", () => {
    const result = parseACL("KEY:ns2");
    expect(result.ok).toBe(true);
    if (!result.ok) return;
    expect(result.entries).toEqual([{ kind: "key", name: "ns2." }]);
  });

  test("a mixed list tolerates whitespace around every entry", () => {
    const result = parseACL(" 10.0.0.0/24 , key:ns2 , 192.168.1.5 ");
    expect(result.ok).toBe(true);
    if (!result.ok) return;
    expect(result.entries).toEqual([
      { kind: "prefix", value: "10.0.0.0/24" },
      { kind: "key", name: "ns2." },
      { kind: "prefix", value: "192.168.1.5" },
    ]);
  });

  test("a trailing comma is skipped, not rejected", () => {
    const result = parseACL("10.0.0.0/24,");
    expect(result.ok).toBe(true);
    if (!result.ok) return;
    expect(result.entries).toEqual([{ kind: "prefix", value: "10.0.0.0/24" }]);
  });

  test("a mask past the address family's width is rejected", () => {
    const result = parseACL("10.0.0.0/33");
    expect(result).toEqual({
      ok: false,
      error: 'allow_transfer "10.0.0.0/33": expected an IP address, a CIDR prefix, or key:<name>',
    });
  });

  test("a value that is neither an address nor a key is rejected", () => {
    const result = parseACL("not-an-ip");
    expect(result.ok).toBe(false);
  });

  test("a hostname is refused, not resolved", () => {
    const result = parseACL("ns2.example.com");
    expect(result).toEqual({
      ok: false,
      error:
        'allow_transfer "ns2.example.com": expected an IP address, a CIDR prefix, or key:<name>',
    });
  });

  test("key: with no name is rejected", () => {
    const result = parseACL("key:");
    expect(result).toEqual({
      ok: false,
      error: 'allow_transfer "key:": key name must be a domain name',
    });
  });

  test("a key name containing a space is rejected", () => {
    const result = parseACL("key:ns 2");
    expect(result.ok).toBe(false);
  });

  // A bare v4-mapped address names one host unambiguously, so it is unmapped
  // to that host's IPv4 spelling rather than rejected — mirroring
  // parseACLEntry's own comment on why in internal/zones/acl.go.
  test("a bare v4-mapped address unmaps to its IPv4 spelling", () => {
    const result = parseACL("::ffff:10.0.0.5");
    expect(result.ok).toBe(true);
    if (!result.ok) return;
    expect(result.entries).toEqual([{ kind: "prefix", value: "10.0.0.5" }]);
  });

  // A v4-mapped *prefix*, unlike a bare address, is refused rather than
  // silently rewritten: ::ffff:10.0.0.0/120 and 10.0.0.0/24 are the same
  // range in two spellings.
  test("a v4-mapped prefix is refused, not silently rewritten", () => {
    const result = parseACL("::ffff:10.0.0.0/120");
    expect(result.ok).toBe(false);
  });

  test("one bad entry fails the whole list, even with good entries around it", () => {
    const result = parseACL("10.0.0.0/24, not-an-ip, key:ns2");
    expect(result.ok).toBe(false);
  });
});

describe("aclKeyNames", () => {
  test("returns the canonical name of every key: entry, in order", () => {
    expect(aclKeyNames("10.0.0.0/24, key:NS2, key:other")).toEqual(["ns2.", "other."]);
  });

  test("returns an empty list when there are no key entries", () => {
    expect(aclKeyNames("10.0.0.0/24, 192.168.1.5")).toEqual([]);
  });

  test("returns an empty list for an unparseable value, rather than throwing", () => {
    expect(aclKeyNames("nonsense")).toEqual([]);
  });

  test("returns an empty list for an empty ACL", () => {
    expect(aclKeyNames("")).toEqual([]);
  });
});
