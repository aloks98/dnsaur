import { z } from "zod";

/**
 * Form validation shared across pages, as zod schemas wired into
 * react-hook-form through `zodResolver` (see any page's `useForm` call).
 *
 * Everything here is a *client-side echo* of a rule the Go server already
 * enforces — the server stays the one source of truth for what's valid, and
 * these only exist so a typo surfaces inline before a round trip instead of
 * as a 400 toast after one. That framing sets the bias throughout: where a
 * rule can't be mirrored exactly in the browser, err toward accepting
 * something the server would reject (a false accept costs one 400 toast)
 * rather than rejecting something it would accept (a false reject silently
 * blocks a legitimate value — the bug an over-strict IPv6 check actually
 * caused here once; see isValidIPv6).
 */

/** A trimmed, non-empty string — the shape of every "just don't leave this
 * blank" field (names, patterns, URLs), each supplying its own message
 * since a specific one ("Name is required") beats a generic one. Trimming
 * is part of the schema rather than the submit handler because the server
 * trims too, so " x " and "x" must be judged — and sent — identically. */
export function requiredText(message: string) {
  return z.string().trim().min(1, message);
}

/** Mirrors Go's net/netip dotted-quad parse: exactly four 1-3 digit
 * octets, 0-255, with no leading zeros (Go rejects "192.168.001.1", which
 * a plain Number() coercion would have accepted). */
export function isValidIPv4(value: string): boolean {
  const match = /^(\d{1,3})\.(\d{1,3})\.(\d{1,3})\.(\d{1,3})$/.exec(value);
  if (!match) return false;
  return match.slice(1).every((octet) => {
    if (octet.length > 1 && octet.startsWith("0")) return false;
    const n = Number(octet);
    return n >= 0 && n <= 255;
  });
}

/**
 * IPv6, validated by handing the address to the WHATWG URL host parser —
 * a pragmatic stand-in for a real IPv6 parser (Go's net/netip on the
 * server) that's good enough to catch typos before the round trip.
 *
 * `allowZone` is the difference between the server's two parsers, and it
 * matters: net/netip.ParseAddr accepts an RFC 4007 zone id ("fe80::1%eth0"
 * scopes a link-local address to an interface) while net.ParseIP and
 * netip.ParsePrefix reject one outright. The URL parser rejects a literal
 * `%` inside IPv6 brackets either way, so a zone is split off and checked
 * separately (non-empty) when the caller's server-side counterpart takes
 * one.
 *
 * A literal "." is deliberately NOT rejected: RFC 4291 §2.2 defines a
 * dotted-quad tail form, and real addresses use it — the NAT64 well-known
 * prefix "64:ff9b::192.0.2.1", say. An earlier version of this check
 * excluded any "." on the theory that "real IPv6 literals never contain a
 * dot"; that premise was wrong and blocked exactly those valid addresses
 * from ever reaching the server.
 */
export function isValidIPv6(value: string, { allowZone = false } = {}): boolean {
  let address = value;
  if (allowZone) {
    const zoneIndex = value.indexOf("%");
    if (zoneIndex !== -1) {
      if (value.slice(zoneIndex + 1) === "") return false;
      address = value.slice(0, zoneIndex);
    }
  }
  if (!address.includes(":")) return false;
  try {
    new URL(`http://[${address}]`);
    return true;
  } catch {
    return false;
  }
}

/** The six digits from an InputOTP — one message for "too short" and
 * "too long" alike, since the control caps input at six anyway. Shared by
 * the login challenge and the account page's enable/disable dialogs. */
export const totpCodeSchema = z.string().length(6, "Enter the 6-digit code");
