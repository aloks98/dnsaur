/**
 * A client-side port of `notify_to`'s grammar — the rules
 * `internal/zones/notifyto.go`'s `ParseNotifyTo` applies to the same
 * string, ported so the zone detail page's Notify out field can reject a
 * malformed entry before a round trip, and so the TSIG keys screen can
 * count a key's usage (`notifyKeyNames`) without a second endpoint for it.
 *
 * **This is not the source of truth.** `internal/zones/notifyto.go`'s
 * `ParseNotifyTo` and `ValidateNotifyTo` validate `notify_to` independently
 * at write time, and the notifier (`internal/zones/notifier.go`) is what
 * actually decides who gets told and when — nothing here is consulted by
 * the DNS server at all. A bug in this file can make the field too strict
 * (blocking a value the server would accept) or too lax (letting a bad one
 * reach the server for a 400), but it can never change who is actually
 * notified. Where the two disagree, the Go parser is right and this file
 * has a bug — see that file for the format itself:
 *
 *   10.0.0.2                              default port, unsigned
 *   10.0.0.2:5353                         explicit port, unsigned
 *   ns2.example.com key:ns2-xfer          resolved at send time, signed
 *   [fd00::2]:5353 key:hetzner-xfer       an IPv6 literal takes brackets with a port
 *
 * comma-separated, whitespace tolerated, empty meaning notify nobody.
 */

import { canonicalDomainName, isValidACLKeyName, parseAddress } from "./acl";
import { splitHostPort } from "./hostport";

const NOTIFY_KEY_PREFIX = "key:";
/** notifyto.go's own default — `zones.DefaultPrimaryPort`, the port any
 * other DNS server is reached on, reused for a target that names none. */
const DEFAULT_NOTIFY_PORT = 53;
/** RFC 1035 §3.1's ceiling on one label, mirrored the same way acl.ts's own
 * `MAX_LABEL_LENGTH` does. */
const MAX_LABEL_LENGTH = 63;

/** One parsed target — mirrors Go's `NotifyTarget`. `port` always has the
 * default applied; `key` is `""` for an unsigned target, otherwise already
 * canonical (lowercase, trailing dot). */
type NotifyTarget = { host: string; port: number; key: string };

type NotifyToParseResult = { ok: true; targets: NotifyTarget[] } | { ok: false; error: string };

type TargetResult = { ok: true; target: NotifyTarget } | { ok: false; error: string };

/**
 * Parses `s` into the targets a NOTIFY would be sent to. An empty string is
 * valid and means notify nobody — the field's own empty state, same as
 * `parseACL`'s.
 *
 * A blank field between commas is skipped rather than rejected, mirroring
 * `ParseNotifyTo`: it names no target, so there is nothing to be wrong
 * about.
 */
export function parseNotifyTo(s: string): NotifyToParseResult {
  const targets: NotifyTarget[] = [];
  for (const raw of s.split(",")) {
    const field = raw.trim();
    if (field === "") continue;
    const result = parseNotifyTarget(field);
    if (!result.ok) return result;
    targets.push(result.target);
  }
  return { ok: true, targets };
}

/** Every canonical TSIG key name `s` names, in the order they appear — the
 * TSIG keys screen's usage count. An unparseable value names none: the
 * field's own `parseNotifyTo` already has the error to show, and this one
 * has nothing to add to it — mirrors `NotifyToKeys`'s own reasoning in
 * notifyto.go. */
export function notifyKeyNames(s: string): string[] {
  const result = parseNotifyTo(s);
  if (!result.ok) return [];
  const names: string[] = [];
  for (const target of result.targets) {
    if (target.key !== "") names.push(target.key);
  }
  return names;
}

function parseNotifyTarget(field: string): TargetResult {
  // The key is a suffix on the entry, so it comes off before the host is
  // looked at — otherwise a host:port split would see the whole string.
  let hostPart = field;
  let key = "";
  const spaceIndex = indexOfSpaceOrTab(field);
  if (spaceIndex >= 0) {
    hostPart = field.slice(0, spaceIndex).trim();
    const rest = field.slice(spaceIndex).trim();
    // Exactly one trailing token, and it must be the key. Anything else is
    // refused rather than ignored: a second token is either a typo or a
    // syntax this format does not have, and silently dropping it would
    // store a target the operator believes is signed and is not.
    if (!rest.toLowerCase().startsWith(NOTIFY_KEY_PREFIX)) {
      return {
        ok: false,
        error: `notify target "${field}": expected \`key:<name>\` after the host`,
      };
    }
    const name = rest.slice(NOTIFY_KEY_PREFIX.length).trim();
    if (/[ \t]/.test(name)) {
      return {
        ok: false,
        error: `notify target "${field}": only one \`key:<name>\` is allowed, and it must be the last token`,
      };
    }
    if (!isValidACLKeyName(name)) {
      return { ok: false, error: `notify target "${field}": key name must be a domain name` };
    }
    key = canonicalDomainName(name);
  }

  // A field that is only a key names no target — caught here, before the
  // host:port split, because that split would otherwise read "key:ns2-xfer"
  // as host "key" with port "ns2-xfer" and report a bad port number, which
  // is both wrong and unactionable (parseNotifyTarget's own comment in
  // notifyto.go).
  if (hostPart.toLowerCase().startsWith(NOTIFY_KEY_PREFIX)) {
    return { ok: false, error: `notify target "${field}": needs a host before the key` };
  }

  // `net.SplitHostPort` never returns anything but a `*net.AddrError`, and
  // `parseNotifyTarget` catches every one of those the same way — the whole
  // field becomes the host, with no port. That includes the two legal
  // no-port shapes: a bare host with no colon, and a bare IPv6 literal.
  const split = splitHostPort(hostPart);
  const host = split ? split.host : hostPart;
  const portStr = split ? split.port : "";

  let port = DEFAULT_NOTIFY_PORT;
  if (portStr !== "") {
    if (!/^\d+$/.test(portStr) || Number(portStr) === 0 || Number(portStr) > 65535) {
      return { ok: false, error: `notify target "${field}": port must be between 1 and 65535` };
    }
    port = Number(portStr);
  }

  if (!isValidNotifyHost(host)) {
    return {
      ok: false,
      error: `notify target "${field}": host must be an IP address or a domain name`,
    };
  }
  return { ok: true, target: { host, port, key } };
}

function indexOfSpaceOrTab(s: string): number {
  for (let i = 0; i < s.length; i++) {
    if (s[i] === " " || s[i] === "\t") return i;
  }
  return -1;
}

/**
 * Mirrors `validPrimaryHost` (primaries.go, reused by notifyto.go): an IP
 * literal, or a domain name under the same guards `isValidACLKeyName`
 * applies for the same reason (`dns.IsDomainName` alone is too liberal).
 * The one difference from that function's denylist is `,`, which is
 * irrelevant here — `parseNotifyTo` has already split on every comma
 * before `host` is ever reached.
 */
function isValidNotifyHost(host: string): boolean {
  if (parseAddress(host) !== null) return true;
  if (host === "" || /[ \t\r\n/\\:]/.test(host)) return false;
  const trimmed = host.endsWith(".") ? host.slice(0, -1) : host;
  const labels = trimmed.split(".");
  return labels.every((label) => label !== "" && label.length <= MAX_LABEL_LENGTH);
}

// ── The roll-up ──────────────────────────────────────────────────────────

/** The four states `GET /zones/{id}/notifies` reports, derived server-side
 * (`notifyStateOf`, internal/api/notifies_handlers.go) — never left to the
 * client to infer from the raw columns, so two clients can never disagree
 * about it. */
type NotifyState = "never" | "current" | "retrying" | "gave_up";

/**
 * Whether a target's state counts as *behind* — the roll-up's own count,
 * and the same rule that decides which targets earn a row of their own
 * before the caret is opened.
 *
 * `never` is deliberately not behind. A target added a moment ago has not
 * failed at anything — it just hasn't been tried yet — and rolling it into
 * "behind" would make adding a secondary look like breaking something.
 */
export function isNotifyBehind(state: NotifyState): boolean {
  return state === "retrying" || state === "gave_up";
}

interface NotifyRollup {
  /** "no targets" / "all 4 current" / "3 of 4 current, 1 never notified" /
   * "2 of 4 behind". */
  label: string;
  behind: number;
  total: number;
}

/** How many of `targets` have never been notified — the one fact that
 * splits a behind-free group into "all current" and "current, but not all
 * of it". Shared by `notifyRollup`'s own resting branch and
 * `notifyRestLabel` below, which describes the same kind of group (never
 * a behind target in it — those already have a row of their own). */
function neverNotifiedCount(targets: readonly { state: NotifyState }[]): number {
  return targets.filter((t) => t.state === "never").length;
}

/**
 * The zone detail page's whole answer to "is NOTIFY working here?" in one
 * line — the glance that stands in for four rows saying "current" four
 * times. The one piece of screen logic that can be *wrong* rather than
 * merely ugly, so it is unit tested apart from the component (see
 * notify.test.ts).
 *
 * `behind` leads whenever there is any — it is the actionable signal, and
 * naming both behind *and* never-notified targets on one line would turn
 * a scannable row into a paragraph. Failing that, a never-notified target
 * is still named rather than folded into "current": "all 4 current" said
 * of a zone that just gained a target — and has told it nothing yet — is a
 * false statement on a row whose whole job is being scannable. Decided
 * 2026-09-02 by the design's author, overriding the artboard's own
 * arithmetic, which conflated the two.
 */
export function notifyRollup(targets: readonly { state: NotifyState }[]): NotifyRollup {
  const total = targets.length;
  if (total === 0) return { label: "no targets", behind: 0, total: 0 };
  const behind = targets.filter((t) => isNotifyBehind(t.state)).length;
  if (behind > 0) return { label: `${behind} of ${total} behind`, behind, total };
  const never = neverNotifiedCount(targets);
  const label =
    never === 0
      ? `all ${total} current`
      : `${total - never} of ${total} current, ${never} never notified`;
  return { label, behind: 0, total };
}

/**
 * "+N current" / "+N never notified" / "+N current, M never notified" —
 * the row list's own hint for whatever the caret hasn't opened yet. Always
 * called with a behind-free set: a behind target already has a row of its
 * own outside this hint, so what is left to describe here is never one —
 * the same reasoning `notifyRollup`'s own resting branch uses, applied to
 * a subset instead of the whole list.
 */
export function notifyRestLabel(targets: readonly { state: NotifyState }[]): string {
  const never = neverNotifiedCount(targets);
  const current = targets.length - never;
  if (never === 0) return `+ ${current} current`;
  if (current === 0) return `+ ${never} never notified`;
  return `+ ${current} current, ${never} never notified`;
}
