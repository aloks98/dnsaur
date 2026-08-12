/**
 * The two things about a TSIG key that the API and the design disagree
 * about, in one place: what an algorithm is called, and where a secret comes
 * from.
 */

/**
 * The algorithms the server accepts, as it accepts them — miekg/dns's own
 * constants, trailing dot included (`dns.HmacSHA256` is the literal string
 * "hmac-sha256."; see internal/api/tsigkeys_handlers.go's `tsigAlgorithms`,
 * which is the exact accepted set). `hmac-md5.sig-alg.reg.int.` is a real
 * RFC 8945 registry name and is deliberately absent: miekg removed MD5 from
 * its signer, so a key created with it would store fine and then fail on the
 * first transfer, and the server rejects it at write.
 *
 * These are *wire* values. Nothing renders them directly — see
 * `algorithmLabel`.
 */
export const TSIG_ALGORITHMS = [
  "hmac-sha1.",
  "hmac-sha224.",
  "hmac-sha256.",
  "hmac-sha384.",
  "hmac-sha512.",
] as const;

export type TSIGAlgorithm = (typeof TSIG_ALGORITHMS)[number];

/** What a new key gets unless the admin says otherwise. */
export const DEFAULT_TSIG_ALGORITHM: TSIGAlgorithm = "hmac-sha256.";

/**
 * The wire value as the design shows it: "hmac-sha256", not "hmac-sha256.".
 *
 * This is the whole of the display half of the mapping, and it is a
 * *label*, never a value. The select's options are valued with the wire
 * string and only labelled with this, so a fetched algorithm always matches
 * an option verbatim. Translating in the other direction — parsing a label
 * back into a wire value at submit time — is the shape that produces the bug
 * this exists to avoid: an unmatched value leaves a native select showing
 * (and submitting) its first option, so opening a hmac-sha512 key and
 * pressing Save would quietly store hmac-sha1.
 */
export function algorithmLabel(wire: string): string {
  return wire.endsWith(".") ? wire.slice(0, -1) : wire;
}

/**
 * Everything the select must be able to show for `current`, which is the
 * known five plus — if the stored value isn't one of them — the stored value
 * itself.
 *
 * The API can't produce an unknown algorithm (it validates against the same
 * five at write), but the database can: a row written by hand, or a value
 * from a future release this build predates. Carrying it as an extra option
 * means editing that key changes only what was edited. Dropping it would
 * silently rewrite the algorithm on the next Save, which is precisely the
 * failure the mapping is here to prevent.
 */
export function algorithmOptions(current: string | undefined): string[] {
  const known: string[] = [...TSIG_ALGORITHMS];
  if (current === undefined || current === "" || known.includes(current)) return known;
  return [...known, current];
}

/**
 * What the server will store the typed name as: lowercase and fully
 * qualified, matching `dns.CanonicalName` (normalizeTSIGName in
 * internal/api/tsigkeys_handlers.go).
 *
 * Shown while the name is being typed rather than applied to it — the name
 * is sent as typed and canonicalised server-side, so this is a preview of
 * that, not a second implementation of it.
 */
export function canonicalKeyName(raw: string): string {
  const name = raw.trim().toLowerCase();
  if (name === "") return "";
  return name.endsWith(".") ? name : `${name}.`;
}

/** 32 bytes — the note under the create row's secret field says so. */
export const SECRET_BYTES = 32;

/**
 * A fresh secret: 32 bytes of CSPRNG output, base64-encoded.
 *
 * `crypto.getRandomValues`, never `Math.random` — the server validates that
 * a secret is base64 but has no way to tell a strong one from a weak one, so
 * a predictable secret would be accepted and would sign transfers happily
 * forever. 32 bytes is the block size of the default HMAC-SHA256 and the
 * length RFC 8945 keys are conventionally generated at.
 */
export function generateSecret(): string {
  const bytes = new Uint8Array(SECRET_BYTES);
  crypto.getRandomValues(bytes);
  let binary = "";
  for (const byte of bytes) binary += String.fromCharCode(byte);
  return btoa(binary);
}

/**
 * Whether the browser can decode this the way the server will
 * (`base64.StdEncoding.DecodeString`, checked at write time because that is
 * how the secret is decoded again at sign time).
 *
 * `atob` is the looser of the two — it tolerates missing padding, which Go
 * refuses — and that is the direction to err in, per lib/schemas.ts: a false
 * accept costs one 400 toast, a false reject blocks a secret the peer is
 * already configured with.
 */
export function isBase64(value: string): boolean {
  try {
    atob(value);
    return true;
  } catch {
    return false;
  }
}
