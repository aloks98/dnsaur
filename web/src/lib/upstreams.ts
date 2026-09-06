/**
 * A TypeScript mirror of internal/upstream/addr.go's grammar for the
 * `upstreams` setting, so the settings form can tell the operator what is
 * wrong while they type instead of after a round trip to the server.
 *
 * Both parsers are held to the same fixture, internal/upstream/testdata/
 * grammar.json (see upstreams.test.ts), and compared on parsed fields and
 * on the rejection `code` alone — never on message text. Go renders one
 * wording and this file renders its own; coupling the two would make copy
 * an interface.
 *
 * The one thing this file must not do, anywhere, is let the `URL`
 * constructor decide a rejection the fixture pins. WHATWG `URL` is far more
 * permissive than Go's net/url for non-special schemes like tls: and
 * https:, so every code the fixture asserts is reached by a rule this
 * module owns — an explicit port check, an explicit IP-literal check — and
 * never by catching a constructor throw.
 */

export type UpstreamScheme = "udp" | "tls" | "https";

export interface Upstream {
  scheme: UpstreamScheme;
  addr: string;
  verifyName: string;
  path: string;
  canonical: string;
}

export type UpstreamErrorCode =
  | "host_not_ip"
  | "missing_name"
  | "name_on_plain"
  | "mixed_schemes"
  | "bad_scheme"
  | "bad_addr"
  | "bad_url"
  | "empty";

export interface UpstreamError {
  code: UpstreamErrorCode;
  entry: string;
  message: string;
}

export type ParseResult = { ok: true; entries: Upstream[] } | { ok: false; error: UpstreamError };

// Default ports and path, applied when the entry omits them — mirrors
// addr.go's defaultPlainPort / defaultDoTPort / defaultDoHPort / defaultDoHPath.
const DEFAULT_PLAIN_PORT = "53";
const DEFAULT_DOT_PORT = "853";
const DEFAULT_DOH_PORT = "443";
const DEFAULT_DOH_PATH = "/dns-query";

function fail(entry: string, code: UpstreamErrorCode, message: string): UpstreamError {
  return { code, entry, message };
}

/**
 * Brackets a host that contains a colon (an IPv6 literal) before joining it
 * with a port — the equivalent of Go's net.JoinHostPort, and the other half
 * of stripping the brackets off one going in (see `bareHost`).
 */
function joinHostPort(host: string, port: string): string {
  return host.includes(":") ? `[${host}]:${port}` : `${host}:${port}`;
}

/**
 * The bracket-free form of a host, whether or not it arrived bracketed.
 *
 * Go's `url.URL.Hostname()` always strips an IPv6 literal's brackets, and
 * this module is written to that contract throughout: `isIPLiteral` and
 * `joinHostPort` both expect a bracket-free host in, and re-bracket only on
 * the way out. The WHATWG `URL.hostname` getter this module actually reads
 * from does *not* strip them (verified against this project's Node and
 * jsdom versions — the URL Standard's host serializer always brackets an
 * IPv6 host, including in `hostname`), so this is the one place that
 * mismatch is absorbed, defensively, before the shared contract begins.
 */
function bareHost(hostname: string): string {
  return hostname.startsWith("[") && hostname.endsWith("]") ? hostname.slice(1, -1) : hostname;
}

/**
 * Splits a hostport that may be missing its port, understanding the
 * bracketed [ipv6]:port form the way net.SplitHostPort does but tolerating
 * an absent port — mirrors addr.go's splitHostPort. Used only for the
 * hand-rolled port check on tls:// and https:// entries, before the URL
 * parser ever sees the entry.
 */
function splitHostPort(hostport: string): { host: string; port: string } {
  if (hostport.startsWith("[")) {
    const end = hostport.indexOf("]");
    if (end < 0) return { host: hostport, port: "" }; // malformed; the caller's host/IP check rejects it
    const host = hostport.slice(1, end);
    const rest = hostport.slice(end + 1);
    const port = rest.startsWith(":") ? rest.slice(1) : "";
    return { host, port };
  }
  const i = hostport.lastIndexOf(":");
  if (i >= 0) return { host: hostport.slice(0, i), port: hostport.slice(i + 1) };
  return { host: hostport, port: "" };
}

/** Digits only, 1-65535 — the same rule addr.go's checkEncryptedPort and
 * parseEncryptedEntry apply by hand, independent of the URL parser. */
function isValidPort(port: string): boolean {
  if (!/^\d+$/.test(port)) return false;
  const n = Number(port);
  return n >= 1 && n <= 65535;
}

/**
 * Validates the port of a tls:// or https:// entry by hand, independent of
 * the URL parser: literal digits, in range 1-65535. Mirrors addr.go's
 * checkEncryptedPort, which runs before url.Parse (here, before `new URL`)
 * is even called, so a malformed port such as ":domain" is rejected as
 * bad_addr by a rule this module owns rather than by whichever inputs the
 * URL parser happens to reject — see the module comment.
 */
function checkEncryptedPort(raw: string): UpstreamError | null {
  let authority = rawAuthority(raw);
  const at = authority.lastIndexOf("@");
  if (at >= 0) authority = authority.slice(at + 1); // drop userinfo; bad_url handles it later
  const { port } = splitHostPort(authority);
  if (port === "") return null; // no port stated: the scheme's default applies
  if (!isValidPort(port)) return fail(raw, "bad_addr", `"${port}" is not a port number`);
  return null;
}

/**
 * The authority component of a `scheme://...` entry, before path, query or
 * fragment — what `checkEncryptedPort` extracts to find the port and
 * `hasUserinfo` extracts to find a bare "@".
 */
function rawAuthority(raw: string): string {
  let authority = raw.slice(raw.indexOf("://") + 3);
  const cut = authority.search(/[/?#]/);
  if (cut >= 0) authority = authority.slice(0, cut);
  return authority;
}

/**
 * Whether the raw authority carries userinfo at all — presence, not
 * content. Mirrors addr.go's `u.User != nil`, which Go's net/url sets the
 * moment an authority contains an unescaped "@", even with nothing in front
 * of it. WHATWG `URL` disagrees: it silently drops a bare "@" with no
 * username or password, so `tls://@1.1.1.1:853#name` round-trips through
 * `new URL()` with empty `.username`/`.password` and would be accepted by a
 * check that trusted those decoded fields. So, like the port check, this is
 * decided by looking at the raw text before the URL parser ever sees it.
 */
function hasUserinfo(raw: string): boolean {
  return rawAuthority(raw).includes("@");
}

/**
 * An IPv4 literal is four dot-separated decimal octets 0-255, each written
 * without a leading zero (an octet is exactly "0", or starts with a nonzero
 * digit) — mirroring `netip.ParseAddr`'s deliberate rejection of leading
 * zeros as octal-ambiguity hardening. Without this, "tls://010.0.0.1" would
 * be accepted here and rejected by Go, because "tls:" is a non-special
 * WHATWG scheme: the URL constructor treats its host as opaque text and
 * never reinterprets or validates the numeric form the way it would for a
 * special scheme like https:.
 */
function isIPv4(host: string): boolean {
  const parts = host.split(".");
  if (parts.length !== 4) return false;
  return parts.every(
    (p) => /^\d{1,3}$/.test(p) && Number(p) <= 255 && (p === "0" || !p.startsWith("0")),
  );
}

/**
 * Whether `host` (as returned by `URL.hostname`, brackets already stripped
 * for IPv6) is an IP literal. An IPv6 literal is anything the URL
 * constructor accepted inside brackets — checked here by re-wrapping it in
 * brackets and asking the URL constructor to parse it as a bracketed host,
 * which is the WHATWG-native way to ask "is this a valid IPv6 literal"
 * without a permissive "contains a colon" test.
 */
function isIPLiteral(host: string): boolean {
  if (isIPv4(host)) return true;
  if (!host.includes(":")) return false;
  try {
    const probe = new URL(`http://[${host}]`);
    // A malformed bracketed host falls back to some other hostname reading
    // in a WHATWG parser rather than throwing, so confirm the brackets
    // round-tripped instead of trusting that construction alone succeeded.
    return probe.hostname === `[${host}]`;
  } catch {
    return false;
  }
}

/**
 * Applies exactly the treatment the plain path always has: split, default
 * the port to 53, bracket a bare IPv6 literal so the appended port parses.
 * Mirrors addr.go's parsePlainEntry. Validates nothing else, on purpose — a
 * plain upstream has always been allowed to be a hostname
 * (`resolver.lan:5353` is in the accept fixture for exactly this reason),
 * and tightening this would reject a working configuration — the failure
 * the settings-page comment (web/src/pages/settings.tsx) already warns
 * about.
 */
function parsePlainEntry(raw: string, hostPort: string): ParseResult {
  const hashIdx = hostPort.indexOf("#");
  if (hashIdx >= 0) {
    const name = hostPort.slice(hashIdx + 1);
    return {
      ok: false,
      error: fail(
        raw,
        "name_on_plain",
        `"#${name}" only applies to tls:// and https:// upstreams, where it names the certificate to check`,
      ),
    };
  }
  let addr = hostPort;
  // Mirror net.SplitHostPort's success test: a bracketed IPv6 literal with a
  // port, or a bare host:port, already splits; anything else gets the
  // default port appended (bracketing first if it is a bare IPv6 literal).
  const alreadyHasPort = (() => {
    if (addr.startsWith("[")) {
      const end = addr.indexOf("]");
      return end >= 0 && addr.slice(end + 1).startsWith(":") && addr.slice(end + 2).length > 0;
    }
    const i = addr.lastIndexOf(":");
    return i >= 0 && addr.slice(0, i).split(":").length === 1;
  })();
  if (!alreadyHasPort) {
    if (addr.includes(":") && !addr.startsWith("[")) {
      addr = `[${addr}]:${DEFAULT_PLAIN_PORT}`;
    } else {
      addr = `${addr}:${DEFAULT_PLAIN_PORT}`;
    }
  }
  return {
    ok: true,
    entries: [{ scheme: "udp", addr, verifyName: "", path: "", canonical: addr }],
  };
}

/**
 * Handles tls:// and https://, which share every rule that makes an
 * encrypted upstream different: the host must be an address, and the
 * certificate name must be stated. Mirrors addr.go's parseEncryptedEntry.
 */
/**
 * The fragment of `u`, percent-decoded — or `null` when the fragment holds a
 * malformed escape such as `%zz`.
 *
 * This exists because the two parsers disagree about *where* such an entry
 * dies, not about whether it does. Go's `url.Parse` refuses the whole URL
 * ("invalid URL escape") and addr.go turns that into `bad_url`. WHATWG `URL`
 * accepts it and hands the fragment back verbatim, so the refusal only
 * happens later, inside `decodeURIComponent`, which *throws* rather than
 * returning an error. An uncaught throw here is not a disagreement about a
 * code — it escapes `parseUpstreams` entirely, and `parseUpstreams` runs
 * inside a `useState` initializer (upstreams-field.tsx's `deriveState`) and
 * inside a zod `superRefine` (settings.tsx's `upstreamsSchema`), so a stored
 * value with a bad escape took the whole settings page down.
 *
 * Returning null lets both call sites answer `bad_url`, which is the code Go
 * answers with. Pinned from both sides by grammar.json's `#a%zz` rows.
 */
function decodeFragment(u: URL): string | null {
  if (u.hash === "") return "";
  try {
    return decodeURIComponent(u.hash.slice(1));
  } catch {
    return null;
  }
}

function parseEncryptedEntry(
  raw: string,
  u: URL,
  scheme: "tls" | "https",
  defaultPort: string,
  defaultPath: string,
): ParseResult {
  // Normalized to bracket-free here (see bareHost); the canonical form
  // puts the brackets back via joinHostPort.
  const host = bareHost(u.hostname);
  const port = u.port;
  if (host === "") {
    return { ok: false, error: fail(raw, "bad_addr", "no address") };
  }
  // Camp 1 (spec §3): the operator supplies the address. Resolving a name
  // here would need DNS to configure DNS.
  if (!isIPLiteral(host)) {
    return {
      ok: false,
      error: fail(
        raw,
        "host_not_ip",
        `"${host}" is a name, and an encrypted upstream needs an address here: write ${scheme}://<address>#${host}`,
      ),
    };
  }
  let effectivePort = port;
  if (effectivePort === "") {
    effectivePort = defaultPort;
  } else if (!isValidPort(effectivePort)) {
    return { ok: false, error: fail(raw, "bad_addr", `"${effectivePort}" is not a port number`) };
  }
  // u.hash includes the leading "#". decodeFragment does the percent-decode
  // and reports a malformed escape as null rather than throwing.
  const verifyName = decodeFragment(u);
  if (verifyName === null) {
    return { ok: false, error: fail(raw, "bad_url", "not a valid URL: malformed percent-escape") };
  }
  if (verifyName === "") {
    return {
      ok: false,
      error: fail(
        raw,
        "missing_name",
        `missing "#name": an encrypted upstream needs the name its certificate must present, e.g. ${raw}#dns.example.net`,
      ),
    };
  }
  const addr = joinHostPort(host, effectivePort);
  let path = "";
  if (defaultPath !== "") {
    path = u.pathname;
    if (path === "" || path === "/") path = defaultPath;
  }
  const canonical = `${scheme}://${addr}${path}#${verifyName}`;
  return {
    ok: true,
    entries: [{ scheme, addr, verifyName, path, canonical }],
  };
}

function parseEntry(raw: string): ParseResult {
  // A bare entry (no "://") takes the plain path: new URL("1.1.1.1:53")
  // reads "1.1.1.1" as the protocol, exactly as Go's url.Parse does, so the
  // URL constructor must not see this input at all.
  if (!raw.includes("://")) {
    return parsePlainEntry(raw, raw);
  }

  // The port has to be validated before the URL constructor ever sees the
  // entry — see the module comment on why this module never lets the URL
  // parser decide a code the fixture pins. Only tls:// and https:// get
  // this check; see parsePlainEntry for the bare/udp:// path.
  const schemeToken = raw.slice(0, raw.indexOf("://")).toLowerCase();
  if (schemeToken === "tls" || schemeToken === "https") {
    const portErr = checkEncryptedPort(raw);
    if (portErr) return { ok: false, error: portErr };
  }

  // Userinfo is rejected by presence, not content, for every scheme — see
  // hasUserinfo. Checked on the raw text before the URL constructor runs,
  // for the same reason the port is: a bare "@" with nothing on either side
  // parses cleanly through `new URL()` into empty username/password fields,
  // which is exactly the input a check on those fields would miss.
  if (hasUserinfo(raw)) {
    return {
      ok: false,
      error: fail(raw, "bad_url", "a username or password has no meaning on a DNS upstream"),
    };
  }

  let u: URL;
  try {
    u = new URL(raw);
  } catch {
    // Deliberately uncovered by any fixture case. A malformed port is
    // already ruled out above, so reaching here means the URL parser
    // refused the entry for some other syntactic reason. Go's net/url and a
    // WHATWG URL parser do not agree on which such inputs are errors at
    // all, so a fixture case pinned to this branch could not be satisfied
    // by one shared rule in both languages — unlike every other code in
    // this grammar, which is decided by a rule this module owns.
    return { ok: false, error: fail(raw, "bad_url", "not a valid URL") };
  }

  if (u.search !== "") {
    return {
      ok: false,
      error: fail(raw, "bad_url", "a query string has no meaning on a DNS upstream"),
    };
  }

  // URL lowercases the protocol and keeps the trailing ":".
  const scheme = u.protocol.slice(0, -1);
  switch (scheme) {
    case "udp": {
      const fragment = decodeFragment(u);
      if (fragment === null) {
        return {
          ok: false,
          error: fail(raw, "bad_url", "not a valid URL: malformed percent-escape"),
        };
      }
      if (fragment !== "") {
        return {
          ok: false,
          error: fail(
            raw,
            "name_on_plain",
            `"#${fragment}" only applies to tls:// and https:// upstreams, where it names the certificate to check`,
          ),
        };
      }
      const trimmedPath = u.pathname.replace(/^\/+|\/+$/g, "");
      if (trimmedPath !== "") {
        return {
          ok: false,
          error: fail(raw, "bad_url", "a path has no meaning on a plain DNS upstream"),
        };
      }
      return parsePlainEntry(raw, u.host);
    }
    case "tls":
      return parseEncryptedEntry(raw, u, "tls", DEFAULT_DOT_PORT, "");
    case "https":
      return parseEncryptedEntry(raw, u, "https", DEFAULT_DOH_PORT, DEFAULT_DOH_PATH);
    default:
      return {
        ok: false,
        error: fail(
          raw,
          "bad_scheme",
          `unknown transport "${scheme}": use udp://, tls:// or https:// (or no scheme for plain DNS)`,
        ),
      };
  }
}

/**
 * Parses the comma-separated `upstreams` setting value. Mirrors
 * internal/upstream/addr.go's ParseUpstreams/parseEntries.
 *
 * Empty entries are dropped rather than rejected — a trailing comma is a
 * typo that costs nothing to forgive. A list that is entirely empty is a
 * different thing: it leaves nowhere to forward, so it is an error.
 */
export function parseUpstreams(value: string): ParseResult {
  const rawEntries = value.split(",");
  const out: Upstream[] = [];
  for (const rawEntry of rawEntries) {
    const trimmed = rawEntry.trim();
    if (trimmed === "") continue;
    const result = parseEntry(trimmed);
    if (!result.ok) return result;
    out.push(...result.entries);
  }
  if (out.length === 0) {
    return { ok: false, error: fail("", "empty", "no upstreams configured") };
  }
  // Mixing transports would mean some queries are encrypted and some are
  // not, with nothing on screen or in the log saying which.
  for (const entry of out.slice(1)) {
    if (entry.scheme !== out[0].scheme) {
      return {
        ok: false,
        error: fail(
          "",
          "mixed_schemes",
          `every upstream must use the same transport, but the list mixes ${out[0].scheme} and ${entry.scheme}`,
        ),
      };
    }
  }
  return { ok: true, entries: out };
}

/**
 * Assembles `scheme://addr[path]#name`, the inverse of the canonical form —
 * what the preset picker calls to build an entry, such that
 * `parseUpstreams(buildUpstream(...)).entries[0].canonical === buildUpstream(...)`.
 *
 * The path handling has to mirror parseEncryptedEntry's exactly, not just
 * pass `path` through: tls never carries one at all (its defaultPath is "",
 * so addr.go's Path field is never touched, and a path here would be
 * silently dropped on the next parse), and https promotes an empty or bare
 * "/" path to defaultDoHPath. Skipping that promotion here was the original
 * bug — buildUpstream("https", addr, name) produced a canonical string that
 * differed from what parsing that same string back produced, which is
 * exactly the invariant this function exists to uphold for the preset
 * picker.
 */
export function buildUpstream(
  scheme: UpstreamScheme,
  addr: string,
  verifyName: string,
  path = "",
): string {
  if (scheme === "udp") return addr;
  const effectivePath =
    scheme === "https" ? (path === "" || path === "/" ? DEFAULT_DOH_PATH : path) : "";
  return `${scheme}://${addr}${effectivePath}#${verifyName}`;
}
