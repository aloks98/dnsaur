import { expect, test } from "vitest";
import { canonicalMAC, isValidMAC } from "./mac";

// The three notations `net.ParseMAC` takes for six bytes, which is what a
// DHCPv4 reservation is keyed on. Anything longer it also takes (EUI-64,
// InfiniBand) the server refuses, so this refuses them too rather than
// letting the form post something the engine could never match.
test("every six-byte notation the server takes is accepted, and nothing longer", () => {
  expect(isValidMAC("a4:83:e7:12:9f:c0")).toBe(true);
  expect(isValidMAC("A4-83-E7-12-9F-C0")).toBe(true);
  expect(isValidMAC("a483.e712.9fc0")).toBe(true);
  expect(isValidMAC("  a4:83:e7:12:9f:c0  ")).toBe(true);

  expect(isValidMAC("")).toBe(false);
  expect(isValidMAC("not-a-mac")).toBe(false);
  expect(isValidMAC("a4:83:e7:12:9f")).toBe(false);
  // EUI-64: eight bytes, which the engine cannot match on a DHCPv4 lease.
  expect(isValidMAC("a4:83:e7:12:9f:c0:11:22")).toBe(false);
  expect(isValidMAC("zz:83:e7:12:9f:c0")).toBe(false);
  // Mixed separators are not a notation.
  expect(isValidMAC("a4:83-e7:12:9f:c0")).toBe(false);
});

// The server stores one spelling whatever arrives, so a screen that posted
// another would get its own value back looking changed.
test("canonicalMAC is the one spelling the server stores", () => {
  expect(canonicalMAC("A4-83-E7-12-9F-C0")).toBe("a4:83:e7:12:9f:c0");
  expect(canonicalMAC("a483.e712.9fc0")).toBe("a4:83:e7:12:9f:c0");
  expect(canonicalMAC(" A4:83:E7:12:9F:C0 ")).toBe("a4:83:e7:12:9f:c0");

  // A value this cannot read is handed back as typed: refusing it is the
  // schema's job, and quietly posting something else would hide which value
  // the server was actually asked about.
  expect(canonicalMAC("not-a-mac")).toBe("not-a-mac");
});
