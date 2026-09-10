/**
 * A client-side port of `allow_transfer`'s grammar — the rules
 * `internal/zones/acl.go`'s `ParseACL` applies to the same string, ported so
 * the zone detail page's Allow transfer field can reject a malformed entry
 * before a round trip, and so the TSIG keys screen can count a key's usage
 * (`aclKeyNames`) without a second endpoint for it.
 *
 * **This is not the source of truth.** `internal/zones/acl.go`'s `ParseACL`
 * and `ValidateACL` validate `allow_transfer` independently at write time,
 * and `ACLAllows` is what actually decides whether a transfer request
 * matches an entry — nothing here is consulted by the DNS server at all. A
 * bug in this file can make the field too strict (blocking a value the
 * server would accept) or too lax (letting a bad one reach the server for a
 * 400), but it can never open or close a transfer the Go parser would
 * decide differently. Where the two disagree, the Go parser is right and
 * this file has a bug — see that file for the format itself:
 *
 *   10.0.0.0/24        a prefix
 *   192.168.1.5        a single address
 *   key:secondary-ns2  a request signed under that TSIG key
 *
 * comma-separated, whitespace tolerated, empty meaning deny.
 */

const ACL_KEY_PREFIX = "key:";
/** RFC 1035 §3.1's ceiling on a domain name, which `validACLKeyName`
 * mirrors via `dns.IsDomainName` on the Go side. */
const MAX_DOMAIN_NAME_LENGTH = 255;
const MAX_LABEL_LENGTH = 63;

/** One parsed entry: an address prefix or a TSIG key name, never both —
 * mirrors Go's `ACLEntry`, whose `Prefix`/`Key` fields are the same
 * either/or. `name`/`value` are already canonical (lowercase, trailing dot
 * for a key; masked for a prefix). */
type ACLEntry = { kind: "key"; name: string } | { kind: "prefix"; value: string };

type ACLParseResult = { ok: true; entries: ACLEntry[] } | { ok: false; error: string };

type EntryResult = { ok: true; entry: ACLEntry } | { ok: false; error: string };

/** The one message every unparseable field falls back to — mirrors
 * `parseACLEntry`'s own final `fmt.Errorf` in acl.go, which every branch
 * that doesn't return its own more specific error reaches too. */
function genericError(field: string): EntryResult {
  return {
    ok: false,
    error: `allow_transfer "${field}": expected an IP address, a CIDR prefix, or key:<name>`,
  };
}

/**
 * Parses `s` into the entries a transfer request would be matched against.
 * An empty string is valid and means deny — the field's own empty state.
 *
 * A blank field between commas (a trailing comma, or repeated ones) is
 * skipped rather than rejected, mirroring `ParseACL`: it names no peer, so
 * there is nothing to be wrong about.
 */
export function parseACL(s: string): ACLParseResult {
  const entries: ACLEntry[] = [];
  for (const raw of s.split(",")) {
    const field = raw.trim();
    if (field === "") continue;
    const result = parseACLEntry(field);
    if (!result.ok) return result;
    entries.push(result.entry);
  }
  return { ok: true, entries };
}

/** Every canonical TSIG key name `s` names, in the order they appear — the
 * TSIG keys screen's usage count, and this field's own "does this ACL name
 * a key at all" check. An unparseable value names none: both of those
 * callers already have their own account of the error (the field's own
 * `parseACL`, the store's 409), and this one has nothing to add to it —
 * mirrors `ACLKeys`'s own reasoning in acl.go. */
export function aclKeyNames(s: string): string[] {
  const result = parseACL(s);
  if (!result.ok) return [];
  const names: string[] = [];
  for (const entry of result.entries) {
    if (entry.kind === "key") names.push(entry.name);
  }
  return names;
}

function parseACLEntry(field: string): EntryResult {
  if (field.slice(0, ACL_KEY_PREFIX.length).toLowerCase() === ACL_KEY_PREFIX) {
    return parseKeyEntry(field);
  }
  if (field.includes("/")) return parseCIDREntry(field);
  return parseAddressEntry(field);
}

function parseKeyEntry(field: string): EntryResult {
  const name = field.slice(ACL_KEY_PREFIX.length).trim();
  if (!isValidACLKeyName(name)) {
    return { ok: false, error: `allow_transfer "${field}": key name must be a domain name` };
  }
  return { ok: true, entry: { kind: "key", name: canonicalDomainName(name) } };
}

/**
 * A stricter version of the guards `keyNameSchema` (pages/tsig-keys.tsx)
 * applies, for the same underlying reason `validACLKeyName` states in
 * acl.go: a liberal domain-name check alone accepts "ns 2". `,` and `:` are
 * refused too — this format's own delimiters — even though a `,` can never
 * actually reach here (`parseACL` already split on it): failing closed
 * rather than open if that splitting ever changes.
 *
 * Exported for `lib/notify.ts`, whose own key check reuses this rather than
 * a second copy — `validNotifyKeyName` in notifyto.go is a byte-for-byte
 * duplicate of `validACLKeyName` in acl.go (its own comment says so), so the
 * TS side mirrors that by sharing one function instead of two that could
 * drift apart from each other even while each stays faithful to its Go
 * original.
 */
export function isValidACLKeyName(name: string): boolean {
  if (name === "" || name.length > MAX_DOMAIN_NAME_LENGTH) return false;
  if (/[ \t\r\n/\\,:]/.test(name)) return false;
  const trimmed = name.endsWith(".") ? name.slice(0, -1) : name;
  const labels = trimmed.split(".");
  return labels.every((label) => label !== "" && label.length <= MAX_LABEL_LENGTH);
}

/** Lowercase, trailing dot — `dns.CanonicalName`'s spelling, the same
 * normalisation `lib/tsig.ts`'s `canonicalKeyName` already gives a typed
 * TSIG key name (see `normalizeTSIGName`, internal/api/tsigkeys_handlers.go).
 * `name` is already validated non-empty by the caller.
 *
 * Exported for `lib/notify.ts`'s own key names — `dns.CanonicalName` is the
 * one normalisation, reused rather than reimplemented a third time. */
export function canonicalDomainName(name: string): string {
  const lower = name.toLowerCase();
  return lower.endsWith(".") ? lower : `${lower}.`;
}

// ── Addresses and prefixes ──────────────────────────────────────────────
//
// There is no IP-address type in the standard library, so this is a small,
// direct port of net/netip's two calls acl.go actually uses:
// ParseAddr (a bare host) and ParsePrefix (address + "/" + mask length).

type AddressResult =
  | { family: "v4"; bytes: [number, number, number, number] }
  | {
      family: "v6";
      groups: number[]; // 8 groups of 16 bits each, network order
    };

function parseAddressEntry(field: string): EntryResult {
  const addr = parseAddress(field);
  if (!addr) return genericError(field);
  return { ok: true, entry: { kind: "prefix", value: formatAddress(addr) } };
}

function parseCIDREntry(field: string): EntryResult {
  const cidr = parseCIDR(field);
  if (!cidr) return genericError(field);
  // A v4-mapped *prefix* is refused rather than silently rewritten:
  // ::ffff:10.0.0.0/120 and 10.0.0.0/24 are the same range in two
  // spellings, and unmapping one into the other would mean the value read
  // back is not the value written — mirrors parseACLEntry's own comment.
  if (cidr.family === "v6" && isV4Mapped(cidr.groups)) {
    return {
      ok: false,
      error: `allow_transfer "${field}": write this as an IPv4 prefix (e.g. 10.0.0.0/24), not an IPv4-mapped IPv6 one`,
    };
  }
  return { ok: true, entry: { kind: "prefix", value: formatCIDR(cidr) } };
}

/**
 * Exported for `lib/notify.ts`'s own host check — `validPrimaryHost`
 * (primaries.go, reused by notifyto.go) tries `netip.ParseAddr` first on
 * exactly the same terms ACL's bare-address branch does, so this is that
 * one `ParseAddr` port shared rather than written twice.
 */
export function parseAddress(s: string): AddressResult | null {
  const v4 = parseIPv4(s);
  if (v4) return { family: "v4", bytes: v4 };
  const groups = parseIPv6Groups(s);
  if (groups) return { family: "v6", groups };
  return null;
}

function parseCIDR(s: string): (AddressResult & { prefixLen: number }) | null {
  const slash = s.lastIndexOf("/");
  if (slash === -1) return null;
  const addrPart = s.slice(0, slash);
  const lenPart = s.slice(slash + 1);
  // A leading zero is refused here for the same reason parseIPv4 refuses one
  // in an octet: netip.ParsePrefix rejects "/08" rather than reading it as 8.
  if (!/^\d{1,3}$/.test(lenPart)) return null;
  if (lenPart.length > 1 && lenPart[0] === "0") return null;
  const prefixLen = Number(lenPart);
  const addr = parseAddress(addrPart);
  if (!addr) return null;
  const maxLen = addr.family === "v4" ? 32 : 128;
  if (prefixLen > maxLen) return null;
  return { ...addr, prefixLen };
}

/** A dotted-quad, each octet 0-255 with no leading zero — netip.ParseAddr
 * rejects "010" as ambiguous with octal, and this mirrors that refusal
 * rather than guessing which base was meant. */
function parseIPv4(s: string): [number, number, number, number] | null {
  const parts = s.split(".");
  if (parts.length !== 4) return null;
  const bytes: number[] = [];
  for (const part of parts) {
    if (!/^\d{1,3}$/.test(part)) return null;
    if (part.length > 1 && part[0] === "0") return null;
    const n = Number(part);
    if (n > 255) return null;
    bytes.push(n);
  }
  return bytes as [number, number, number, number];
}

/**
 * The eight 16-bit groups of an IPv6 address, expanding a single `::` run
 * and an optional trailing embedded IPv4 tail (`::ffff:10.0.0.5`). `null`
 * for anything that isn't a well-formed address — including a bare "1.2.3.4"
 * shaped input, which the caller only reaches for here after `parseIPv4`
 * has already failed on it.
 */
function parseIPv6Groups(s: string): number[] | null {
  if (s === "" || s.includes("%")) return null;

  // An embedded IPv4 tail is only ever the address's last group, so it is
  // expanded to the two hextets it represents up front — the rest of this
  // function then only ever has to deal in plain hex groups.
  let working = s;
  const lastColon = working.lastIndexOf(":");
  if (lastColon !== -1 && working.slice(lastColon + 1).includes(".")) {
    const v4 = parseIPv4(working.slice(lastColon + 1));
    if (!v4) return null;
    const hi = ((v4[0] << 8) | v4[1]).toString(16);
    const lo = ((v4[2] << 8) | v4[3]).toString(16);
    working = `${working.slice(0, lastColon + 1)}${hi}:${lo}`;
  }

  const doubleColonParts = working.split("::");
  if (doubleColonParts.length > 2) return null; // "::" may appear at most once

  let groups: string[];
  if (doubleColonParts.length === 2) {
    const left = doubleColonParts[0] === "" ? [] : doubleColonParts[0].split(":");
    const right = doubleColonParts[1] === "" ? [] : doubleColonParts[1].split(":");
    const missing = 8 - left.length - right.length;
    if (missing < 0) return null;
    groups = [...left, ...(Array(missing).fill("0") as string[]), ...right];
  } else {
    groups = working.split(":");
  }

  if (groups.length !== 8) return null;
  const nums: number[] = [];
  for (const g of groups) {
    if (!/^[0-9a-fA-F]{1,4}$/.test(g)) return null;
    nums.push(Number.parseInt(g, 16));
  }
  return nums;
}

/** ::ffff:0:0/96 — the standard IPv4-mapped range: the first five groups
 * zero, the sixth all ones. */
function isV4Mapped(groups: number[]): boolean {
  return (
    groups[0] === 0 &&
    groups[1] === 0 &&
    groups[2] === 0 &&
    groups[3] === 0 &&
    groups[4] === 0 &&
    groups[5] === 0xffff
  );
}

/** The IPv4 spelling a v4-mapped address's last two groups carry — used
 * only for the bare-address case, which unmaps rather than refuses (see
 * parseCIDREntry's comment on why a prefix does the opposite). */
function unmapToV4(groups: number[]): [number, number, number, number] {
  const g6 = groups[6];
  const g7 = groups[7];
  return [(g6 >> 8) & 0xff, g6 & 0xff, (g7 >> 8) & 0xff, g7 & 0xff];
}

function formatAddress(addr: AddressResult): string {
  if (addr.family === "v4") return addr.bytes.join(".");
  // A bare v4-mapped address names one host unambiguously, so — unlike a
  // v4-mapped prefix — it is unmapped to that host's IPv4 spelling rather
  // than refused, mirroring parseACLEntry's ParseAddr branch.
  if (isV4Mapped(addr.groups)) return unmapToV4(addr.groups).join(".");
  return formatIPv6(addr.groups);
}

function formatCIDR(cidr: AddressResult & { prefixLen: number }): string {
  if (cidr.family === "v4") {
    // A single address prints without its all-ones mask (10.0.0.5, not
    // 10.0.0.5/32) — matches FormatACL's own case for a host route.
    if (cidr.prefixLen === 32) return cidr.bytes.join(".");
    return `${maskV4(cidr.bytes, cidr.prefixLen).join(".")}/${cidr.prefixLen}`;
  }
  if (cidr.prefixLen === 128) return formatIPv6(cidr.groups);
  return `${formatIPv6(cidr.groups)}/${cidr.prefixLen}`;
}

function maskV4(
  bytes: [number, number, number, number],
  prefixLen: number,
): [number, number, number, number] {
  const mask = prefixLen === 0 ? 0 : (0xffffffff << (32 - prefixLen)) >>> 0;
  const value = ((bytes[0] << 24) | (bytes[1] << 16) | (bytes[2] << 8) | bytes[3]) >>> 0;
  const masked = (value & mask) >>> 0;
  return [(masked >>> 24) & 0xff, (masked >>> 16) & 0xff, (masked >>> 8) & 0xff, masked & 0xff];
}

/** Lowercase hex groups, uncompressed. Not RFC 5952's canonical
 * (compressed) spelling — nothing here re-canonicalises what the server
 * already stores (see this file's own top comment), so an entry's `value`
 * only has to be valid, not pretty. */
function formatIPv6(groups: number[]): string {
  return groups.map((g) => g.toString(16)).join(":");
}
