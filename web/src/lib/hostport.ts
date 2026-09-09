/**
 * One `host:port` grammar for the whole dashboard — a TypeScript reading of
 * Go's `net.SplitHostPort`/`net.JoinHostPort`, which is what the server side
 * of every field that uses this eventually runs.
 *
 * It exists because there were three hand-rolled splitters (the upstreams
 * grammar, the notify_to grammar, the upstreams editor) and they had already
 * drifted: `portOf("::1")` answered `""` in one and `"1"` in another, i.e.
 * one of them invented a port for an address the operator never gave one to.
 * A bare IPv6 literal has no port — that is the whole reason the bracketed
 * form exists — and it is pinned by this module's own test.
 *
 * Two readings of the same grammar, because the callers genuinely differ:
 *
 *   - `splitHostPort` is `net.SplitHostPort`: a missing port is an *error*,
 *     which is what the notify_to parser needs, since Go's failure there is
 *     what makes the whole field the host.
 *   - `splitHostPortOptional` treats a missing port as an answer, which is
 *     what a field that applies a default port needs.
 */

export interface HostPort {
  host: string;
  /** "" only from `splitHostPortOptional`, and only when none was stated. */
  port: string;
}

/**
 * The shared parse. `hadPort` distinguishes "no port" (which one caller
 * forgives and the other doesn't) from a shape the grammar refuses outright,
 * which is `null` for both.
 *
 * The bracket rules are Go's, including the two that look redundant: a stray
 * `[` anywhere after the first one, and a stray `]` after the pair, are both
 * refusals. Checking only the port half for `]` let `abc]:1234` through as
 * `{host: "abc]", port: "1234"}`, which `net.SplitHostPort` refuses with
 * "unexpected ']' in address".
 */
function parse(hostport: string): (HostPort & { hadPort: boolean }) | null {
  if (hostport.startsWith("[")) {
    const end = hostport.indexOf("]");
    if (end < 0) return null;
    if (hostport.slice(1).includes("[")) return null;
    if (hostport.slice(end + 1).includes("]")) return null;
    const host = hostport.slice(1, end);
    const rest = hostport.slice(end + 1);
    if (rest === "") return { host, port: "", hadPort: false };
    if (!rest.startsWith(":")) return null;
    const port = rest.slice(1);
    if (port.includes(":")) return null; // too many colons
    return { host, port, hadPort: true };
  }
  if (hostport.includes("[") || hostport.includes("]")) return null;
  const lastColon = hostport.lastIndexOf(":");
  if (lastColon < 0) return { host: hostport, port: "", hadPort: false };
  const host = hostport.slice(0, lastColon);
  // More than one colon with no brackets: a bare IPv6 literal (or genuinely
  // too many colons). Either way the last colon is not a port separator.
  if (host.includes(":")) return null;
  return { host, port: hostport.slice(lastColon + 1), hadPort: true };
}

/**
 * Split `host:port`, exactly as `net.SplitHostPort` does — `null` for every
 * input it returns an `*net.AddrError` for, a missing port included. The
 * brackets come off an IPv6 literal; `joinHostPort` puts them back.
 */
export function splitHostPort(hostport: string): HostPort | null {
  const parsed = parse(hostport);
  return parsed && parsed.hadPort ? { host: parsed.host, port: parsed.port } : null;
}

/**
 * The same grammar with a missing port as an answer rather than an error:
 * `port` is "" and `host` is everything that was given. A shape the grammar
 * refuses (an unclosed bracket, a stray one) is handed back whole as the
 * host, so a caller defaulting a port neither loses the operator's text nor
 * has to re-derive what "no port" looks like.
 */
export function splitHostPortOptional(hostport: string): HostPort {
  const parsed = parse(hostport);
  return parsed ? { host: parsed.host, port: parsed.port } : { host: hostport, port: "" };
}

/**
 * `net.JoinHostPort`: brackets a host that contains a colon (an IPv6
 * literal), which is the other half of taking them off on the way in.
 */
export function joinHostPort(host: string, port: string): string {
  return host.includes(":") ? `[${host}]:${port}` : `${host}:${port}`;
}
