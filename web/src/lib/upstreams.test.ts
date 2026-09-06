import { readFileSync } from "node:fs";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { describe, expect, it } from "vitest";
import { buildUpstream, parseUpstreams, type UpstreamScheme } from "./upstreams";

// The same file internal/upstream/addr_test.go reads. Two parsers, one
// grammar: a change here fails both suites until both agree, which is the
// only thing keeping the form's rules and the server's rules from drifting.
const fixture = JSON.parse(
  readFileSync(
    resolve(
      dirname(fileURLToPath(import.meta.url)),
      "../../../internal/upstream/testdata/grammar.json",
    ),
    "utf8",
  ),
) as {
  accept: {
    in: string;
    scheme: string;
    addr: string;
    verifyName: string;
    path: string;
    canonical: string;
  }[];
  reject: { in: string; code: string }[];
  acceptList: { in: string; count: number }[];
  rejectList: { in: string; code: string }[];
};

describe("parseUpstreams", () => {
  it.each(fixture.accept)("accepts $in", (c) => {
    const got = parseUpstreams(c.in);
    if (!got.ok) throw new Error(`rejected with ${got.error.code}: ${got.error.message}`);
    expect(got.entries).toHaveLength(1);
    expect(got.entries[0]).toEqual({
      scheme: c.scheme,
      addr: c.addr,
      verifyName: c.verifyName,
      path: c.path,
      canonical: c.canonical,
    });
  });

  it.each([...fixture.reject, ...fixture.rejectList])("rejects $in as $code", (c) => {
    const got = parseUpstreams(c.in);
    if (got.ok) throw new Error(`accepted, but the grammar says ${c.code}`);
    expect(got.error.code).toBe(c.code);
  });

  it.each(fixture.acceptList)("parses $in into $count entries", (c) => {
    const got = parseUpstreams(c.in);
    if (!got.ok) throw new Error(`rejected with ${got.error.code}: ${got.error.message}`);
    expect(got.entries).toHaveLength(c.count);
  });

  // canonical is what gets saved when the form builds an entry from the
  // preset picker, so parsing it again has to produce the same upstream.
  it.each(fixture.accept)("round-trips the canonical form of $in", (c) => {
    const first = parseUpstreams(c.in);
    if (!first.ok) throw new Error(`rejected: ${first.error.message}`);
    const again = parseUpstreams(first.entries[0].canonical);
    if (!again.ok) throw new Error(`canonical form rejected: ${again.error.message}`);
    expect(again.entries[0]).toEqual(first.entries[0]);
  });
});

// buildUpstream is the inverse of the canonical form, used by the preset
// picker (Task 9+) to write an entry into the upstreams field. Its whole
// contract is this invariant: what it builds has to parse back into
// exactly itself, or a saved preset silently drifts from what the picker
// showed. This was found broken for https with no explicit path — the
// builder passed `path` straight through instead of promoting an empty or
// bare "/" to the DoH default the parser applies, so
// buildUpstream("https", addr, name) produced a string that did not
// round-trip. Covering all three schemes, including both a DoH call with
// no path and one with an explicit non-default path, is what pins that.
describe("buildUpstream", () => {
  const cases: {
    name: string;
    scheme: UpstreamScheme;
    addr: string;
    verifyName: string;
    path?: string;
  }[] = [
    { name: "udp", scheme: "udp", addr: "1.1.1.1:53", verifyName: "" },
    { name: "udp with a hostname", scheme: "udp", addr: "resolver.lan:5353", verifyName: "" },
    { name: "tls", scheme: "tls", addr: "1.1.1.1:853", verifyName: "cloudflare-dns.com" },
    {
      name: "tls with an IPv6 address",
      scheme: "tls",
      addr: "[2606:4700:4700::1111]:853",
      verifyName: "cloudflare-dns.com",
    },
    // No explicit path: the builder must promote to defaultDoHPath itself,
    // the same way the parser does, or this drifts from what re-parsing
    // the built string produces.
    {
      name: "https with no explicit path",
      scheme: "https",
      addr: "9.9.9.9:443",
      verifyName: "dns.quad9.net",
    },
    // The default path stated explicitly: must not be doubled or altered.
    {
      name: "https with the default path stated explicitly",
      scheme: "https",
      addr: "1.1.1.1:443",
      verifyName: "cloudflare-dns.com",
      path: "/dns-query",
    },
    // A non-default path: must be kept as-is, not overridden by the default.
    {
      name: "https with an explicit non-default path",
      scheme: "https",
      addr: "8.8.8.8:8443",
      verifyName: "dns.google",
      path: "/resolve",
    },
  ];

  it.each(cases)("round-trips $name", ({ scheme, addr, verifyName, path }) => {
    const built = buildUpstream(scheme, addr, verifyName, path);
    const parsed = parseUpstreams(built);
    if (!parsed.ok)
      throw new Error(
        `buildUpstream output rejected: ${parsed.error.code}: ${parsed.error.message}`,
      );
    expect(parsed.entries[0].canonical).toBe(built);
  });
});
