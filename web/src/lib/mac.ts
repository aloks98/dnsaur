import { z } from "zod";

/**
 * One MAC grammar for the dashboard — a TypeScript reading of Go's
 * `net.ParseMAC` narrowed to the 6-byte addresses DHCPv4 actually uses.
 *
 * It exists because two screens write one: a DHCP reservation pins a MAC to
 * an address, and a client matcher (`mac:<address>`) follows that device's
 * lease across renewals. Both post to a server that stores the address
 * **canonically** — lowercase, colon-separated — whatever spelling arrived,
 * so a screen that sent `AA-BB-CC-DD-EE-FF` would get `aa:bb:cc:dd:ee:ff`
 * back and the row would appear to change under the operator. Normalising
 * before the request is what keeps what was typed and what was saved the
 * same value.
 *
 * The three notations are the ones `net.ParseMAC` takes for six bytes; its
 * longer forms (EUI-64, 20-octet InfiniBand) are deliberately refused here
 * because the server refuses them too — a DHCPv4 reservation is keyed on a
 * 6-byte address and one the engine cannot match would silently never fire.
 */
const COLON = /^[0-9a-f]{2}(:[0-9a-f]{2}){5}$/i;
const HYPHEN = /^[0-9a-f]{2}(-[0-9a-f]{2}){5}$/i;
/** Cisco's `aabb.ccdd.eeff`. */
const DOTTED = /^[0-9a-f]{4}(\.[0-9a-f]{4}){2}$/i;

export function isValidMAC(value: string): boolean {
  const mac = value.trim();
  return COLON.test(mac) || HYPHEN.test(mac) || DOTTED.test(mac);
}

/**
 * The one spelling the server stores, from any of the three it accepts.
 * A value this cannot read is returned trimmed and unchanged: refusing it
 * is the schema's job, and silently posting something else would hide which
 * value the server was actually asked about.
 */
export function canonicalMAC(value: string): string {
  const mac = value.trim();
  if (!isValidMAC(mac)) return mac;
  const hex = mac.replace(/[:.-]/g, "").toLowerCase();
  return (hex.match(/../g) ?? []).join(":");
}

/** The message is one for all three notations on purpose: naming which one
 * was nearly right would be longer than showing the shape that works. */
export const macSchema = z
  .string()
  .refine(isValidMAC, "Enter a hardware address, e.g. aa:bb:cc:dd:ee:ff");
