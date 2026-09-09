import { describe, expect, test } from "vitest";
import { joinHostPort, splitHostPort, splitHostPortOptional } from "./hostport";

// The case this module exists for: three splitters disagreed about a bare
// IPv6 literal, and one of them read its last group as a port. "::1" is an
// address with no port — reading "1" out of it would send a query to a port
// the operator never named.
test("a bare IPv6 literal has no port", () => {
  expect(splitHostPortOptional("::1")).toEqual({ host: "::1", port: "" });
  expect(splitHostPortOptional("fd00::2")).toEqual({ host: "fd00::2", port: "" });
  expect(splitHostPortOptional("2606:4700:4700::1111")).toEqual({
    host: "2606:4700:4700::1111",
    port: "",
  });
  // Stating one means bracketing it, which is what brackets are for.
  expect(splitHostPortOptional("[::1]:853")).toEqual({ host: "::1", port: "853" });
});

describe("splitHostPort (net.SplitHostPort)", () => {
  test("splits a stated port and strips an IPv6 literal's brackets", () => {
    expect(splitHostPort("1.1.1.1:53")).toEqual({ host: "1.1.1.1", port: "53" });
    expect(splitHostPort("[fd00::2]:5353")).toEqual({ host: "fd00::2", port: "5353" });
    expect(splitHostPort("ns2.example.com:5353")).toEqual({
      host: "ns2.example.com",
      port: "5353",
    });
    // Not validated here — the caller decides what a port may say.
    expect(splitHostPort("1.1.1.1:domain")).toEqual({ host: "1.1.1.1", port: "domain" });
  });

  test("a missing port is an error, exactly as it is in Go", () => {
    expect(splitHostPort("example.com")).toBeNull();
    expect(splitHostPort("[fd00::2]")).toBeNull();
    expect(splitHostPort("fd00::2")).toBeNull();
  });

  test("refuses the shapes net.SplitHostPort refuses", () => {
    expect(splitHostPort("1.1.1.1:80:90")).toBeNull(); // too many colons
    expect(splitHostPort("abc]:1234")).toBeNull(); // unexpected ']'
    expect(splitHostPort("[fd00::2:5353")).toBeNull(); // unclosed '['
    expect(splitHostPort("[[fd00::2]:53")).toBeNull(); // second '['
    expect(splitHostPort("[fd00::2]x53")).toBeNull(); // ']' not followed by ':'
  });
});

describe("splitHostPortOptional", () => {
  test("hands back the whole input as the host when no port is stated", () => {
    expect(splitHostPortOptional("example.com")).toEqual({ host: "example.com", port: "" });
    expect(splitHostPortOptional("")).toEqual({ host: "", port: "" });
    // Brackets still come off a well-formed literal, so a caller that goes on
    // to ask "is this an address?" sees one.
    expect(splitHostPortOptional("[fd00::2]")).toEqual({ host: "fd00::2", port: "" });
  });

  test("keeps a malformed value intact rather than inventing a split", () => {
    expect(splitHostPortOptional("[fd00::2")).toEqual({ host: "[fd00::2", port: "" });
    expect(splitHostPortOptional("abc]:1234")).toEqual({ host: "abc]:1234", port: "" });
  });
});

test("joinHostPort brackets an IPv6 literal and nothing else", () => {
  expect(joinHostPort("1.1.1.1", "53")).toBe("1.1.1.1:53");
  expect(joinHostPort("dns.example.net", "853")).toBe("dns.example.net:853");
  expect(joinHostPort("fd00::2", "5353")).toBe("[fd00::2]:5353");
  // Round-trips with the strict split, which is the invariant every caller
  // that canonicalises an address depends on.
  expect(splitHostPort(joinHostPort("::1", "853"))).toEqual({ host: "::1", port: "853" });
});
