import { afterEach, expect, test, vi } from "vitest";
import { generateSecret, SECRET_BYTES } from "./tsig";

afterEach(() => vi.restoreAllMocks());

/**
 * The one property of a generated secret that nothing else can observe.
 *
 * A TSIG key signs zone transfers, so where its bytes came from is the whole
 * point — and it is invisible from the outside: `Math.random` produces a
 * string of exactly the same shape, passes the same base64 regex, and looks
 * identical in every other test and on screen. The API can only check that a
 * secret is base64; it has no way to tell a strong one from a weak one. So
 * the source is asserted directly.
 *
 * Not merely "getRandomValues was called": the returned string is checked to
 * be the base64 of the exact buffer the CSPRNG filled, which is what stops a
 * decorative call alongside a `Math.random` value that actually ships.
 */
test("the generated secret is base64 of bytes drawn from the CSPRNG", () => {
  const draw = vi.spyOn(crypto, "getRandomValues");

  const secret = generateSecret();

  expect(draw).toHaveBeenCalledTimes(1);
  const filled = draw.mock.calls[0]![0] as Uint8Array;
  expect(filled).toBeInstanceOf(Uint8Array);
  expect(filled).toHaveLength(SECRET_BYTES);
  // getRandomValues fills in place, so `filled` now holds the very bytes the
  // secret must encode.
  expect(secret).toBe(btoa(String.fromCharCode(...filled)));
});

test("a generated secret is 32 bytes, base64", () => {
  expect(generateSecret()).toMatch(/^[A-Za-z0-9+/]{43}=$/);
  expect(atob(generateSecret())).toHaveLength(SECRET_BYTES);
});

test("two secrets are not the same secret", () => {
  expect(generateSecret()).not.toBe(generateSecret());
});
