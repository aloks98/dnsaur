import { z } from "zod";

/**
 * A name the server will canonicalise rather than refuse.
 *
 * Zone names and TSIG key names are both put through `dns.CanonicalName` on
 * the other side (`normalizeZoneName` in internal/api/zones_handlers.go,
 * `normalizeTSIGName` in tsigkeys_handlers.go), which lowercases and appends
 * the trailing dot regardless of what was typed. So this catches only what
 * would otherwise be a wasted request — blank, whitespace or path characters,
 * or an empty label (`e412..in`) — and leaves the rest to the server, which
 * is the one that actually decides.
 *
 * The message is the caller's because it carries the example, and a key is
 * not a zone: "example.com" and "xfer.example.com" are what each field's
 * reader is being asked for.
 */
export function dnsNameSchema(message: string) {
  return z
    .string()
    .trim()
    .refine((value) => {
      const name = value.replace(/\.$/, "");
      if (name === "" || /[ \t\r\n/\\]/.test(name)) return false;
      return !name.split(".").some((label) => label === "");
    }, message);
}
