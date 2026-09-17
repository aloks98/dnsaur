import type { CertificateStatus, ProtocolStatus, ResolverStatus } from "../api/types";
import { formatDuration } from "./format";
import { DHCP_BASE } from "./nav";

/**
 * A protocol's reality, reduced to the three states spec §8 and the
 * Protocols artboard both read off an intent/reality pair:
 *
 * - `off` — the setting itself is false. Reality is moot.
 * - `listening` — enabled, and the socket is actually open.
 * - `failed` — enabled, but the bind didn't happen (a privileged port
 *   already taken, a certificate that stopped reading) — see
 *   ProtocolStatus.error for why.
 *
 * Shared between protocols-field.tsx's per-row line and `statusFacts`'
 * "enabled but not listening" fact, so the two never drift on what counts
 * as broken.
 */
type ServingState = "off" | "listening" | "failed";

export function servingState(status: ProtocolStatus): ServingState {
  if (!status.enabled) return "off";
  return status.listening ? "listening" : "failed";
}

/**
 * One thing that is currently wrong with the running server, as the top
 * bar's status panel lists it (components/top-nav.tsx's StatusCell).
 *
 * `id` is stable per *kind*, not per text: it is what says whether this is
 * the same fact as last poll, so a row keeps its place — and its NEW tag —
 * while its words move ("not seen for 3h" becoming "4h"). A stale replica's
 * carries its instance id, since a main can have several at once.
 *
 * `waitsOnOperator` is the middle of the three ordering groups, and it is a
 * claim about the fact rather than about its tone: a refused DHCP
 * configuration stands until somebody edits the scope, while an unreachable
 * engine ends the moment the engine answers again. Sorting on it puts the
 * rows nobody can wait out above the rows that will clear themselves.
 */
export interface StatusFact {
  id: string;
  tone: "amber" | "red";
  /** The whole fact, in the server's own words wherever they are its own. */
  text: string;
  /** A muted second line — the listener's error under a failed bind. */
  sub?: string;
  /** Where it is fixed: a Settings band anchor, or the DHCP section. */
  target: string;
  waitsOnOperator: boolean;
}

// The Settings bands' own fragments — `settings.tsx` gives every band an id
// from its title (lowercased, spaces hyphenated) and scrolls to the one the
// hash names, which is how the top bar's replica chip already reaches Sync.
const SETTINGS_UPSTREAMS = "/settings#upstreams";
const SETTINGS_PROTOCOLS = "/settings#protocols";
const SETTINGS_SYNC = "/settings#sync";

/**
 * What each destination is called on the row that links to it.
 *
 * Title case in the source, uppercased by CSS in the panel — the same rule
 * lib/nav.ts states for the group labels, and for the same reason: a screen
 * reader should not have to spell out S-Y-N-C.
 */
export const FACT_TARGETS: Record<string, string> = {
  [SETTINGS_UPSTREAMS]: "Settings › Upstreams",
  [SETTINGS_PROTOCOLS]: "Settings › Protocols",
  [SETTINGS_SYNC]: "Settings › Sync",
  [DHCP_BASE]: "DHCP › Scopes",
};

/**
 * Everything wrong with the running server right now, from the one object
 * that reports all of it (`GET /resolver/status`).
 *
 * This replaced a stack of page-wide warning bars — one per fact, up to
 * nine of them across the top of every screen — and it is one function
 * rather than the three it used to be (`syncBanners`, `dhcpBanners`, and
 * serving-banners.tsx's inline protocol lines) because the facts are now
 * ranked, counted and announced together. Three producers with three
 * shapes could not be ordered against each other without a fourth thing
 * knowing all three.
 *
 * Only failures, which is the rule every one of those producers already
 * had: DHCP being off is what most instances are, a healthy HA pair is the
 * configuration working, and being a replica is a state (it reads as the
 * top bar's chip). A panel that also carried the normal case is a panel an
 * operator learns to skip — the whole complaint that got the bars removed.
 *
 * Emission order is by subject — encryption, the two listeners, the
 * certificate, sync, DHCP — and is *not* the order the panel shows. It is
 * the tie-breaker for it: the panel groups by tone and `waitsOnOperator`
 * and then falls back to first-seen, and on a first load first-seen is this
 * order, which is what makes a fresh load come out as the boards' numbered
 * 1–10 rather than in whatever order the endpoint's fields happen to sit.
 *
 * `now` is a parameter for the one line that does arithmetic on the clock,
 * so the durations are testable without pinning the runner's.
 */
export function statusFacts(status: ResolverStatus | undefined, now = Date.now()): StatusFact[] {
  if (!status) return [];
  const facts: StatusFact[] = [];

  if (status.encryption_downgraded) {
    // The one red fact: every query is going out in the clear because the
    // stored `upstreams` value — which asked for DoT or DoH — would not
    // parse. The server's `reason` is the whole of what is useful here, so
    // it is the sentence rather than a second line; the generic tail is
    // what the banner said before this endpoint reported a reason at all,
    // kept for the case where it still doesn't.
    facts.push({
      id: "encryption",
      tone: "red",
      text: `Encryption is off — upstreams could not be parsed, ${status.reason || "falling back to plaintext resolvers"}`,
      target: SETTINGS_UPSTREAMS,
      waitsOnOperator: true,
    });
  }
  // DoT and DoH fail independently — the whole reason ProtocolStatus
  // reports them separately rather than as one bool — so they are two
  // facts, not one. The bind error is the row's muted second line: it is
  // the detail under the fact, not the fact.
  if (servingState(status.serving.dot) === "failed") {
    facts.push({
      id: "dot",
      tone: "amber",
      text: "DNS-over-TLS is enabled but not listening",
      sub: status.serving.dot.error,
      target: SETTINGS_PROTOCOLS,
      waitsOnOperator: true,
    });
  }
  if (servingState(status.serving.doh) === "failed") {
    facts.push({
      id: "doh",
      tone: "amber",
      text: "DNS-over-HTTPS is enabled but not listening",
      sub: status.serving.doh.error,
      target: SETTINGS_PROTOCOLS,
      waitsOnOperator: true,
    });
  }
  if (status.certificate?.expiring_soon) {
    // The server reports a certificate that has already lapsed as ok=true,
    // expiring_soon=true (internal/app/serve.go's CertExpiry), so without
    // the past tense this said "expires in 0 days" about one that expired
    // last week.
    const { days, date, expired } = expiringSoonDetail(status.certificate, new Date(now));
    facts.push({
      id: "certificate",
      tone: "amber",
      text: expired
        ? `TLS certificate expired — ${date}`
        : `TLS certificate expires in ${days} day${days === 1 ? "" : "s"} — ${date}`,
      target: SETTINGS_PROTOCOLS,
      waitsOnOperator: true,
    });
  }

  const sync = status.sync;
  if (sync?.last_error) {
    // The same field, two failures. On a replica it is the last pull, and
    // the next successful one empties it. On a main it is `sync.replicas` —
    // the row the whole registry lives in — unreadable, which has no pull
    // to have failed and stands until somebody repairs the row.
    const replica = sync.role === "replica";
    facts.push({
      id: "sync-error",
      tone: "amber",
      text: replica ? `Last pull failed: ${sync.last_error}` : `Sync: ${sync.last_error}`,
      target: SETTINGS_SYNC,
      waitsOnOperator: !replica,
    });
  }
  for (const replica of sync?.replicas ?? []) {
    if (!replica.stale) continue;
    // How long it has been quiet, not when it was last heard: an operator
    // should not have to subtract a wall-clock stamp from now.
    facts.push({
      id: `replica-stale:${replica.instance_id}`,
      tone: "amber",
      text: `Replica ${replica.instance_id} not seen for ${formatDuration(now - replica.last_seen)}`,
      target: SETTINGS_SYNC,
      waitsOnOperator: false,
    });
  }

  const dhcp = status.dhcp;
  if (dhcp?.enabled) {
    // `config rejected` carries the engine's own words rather than a
    // translation: the message is Kea's refusal of a configuration dnsaur
    // built, and paraphrasing it would leave the operator matching an
    // approximation against the engine's log. It is listed above the
    // unreachable engine because the server already ranks them that way —
    // a configuration the engine would not take is the one somebody has to
    // act on, and it is still true when the engine comes back.
    if (dhcp.engine === "config rejected") {
      facts.push({
        id: "dhcp-config",
        tone: "amber",
        text: `DHCP config rejected: ${dhcp.message ?? ""}`,
        target: DHCP_BASE,
        waitsOnOperator: true,
      });
    }
    if (dhcp.engine === "unreachable") {
      facts.push({
        id: "dhcp-engine",
        tone: "amber",
        text: "DHCP engine unreachable",
        target: DHCP_BASE,
        waitsOnOperator: false,
      });
    }
    if (dhcp.ha?.communication_interrupted ?? false) {
      facts.push({
        id: "dhcp-partner",
        tone: "amber",
        text: "DHCP partner unreachable",
        target: DHCP_BASE,
        waitsOnOperator: false,
      });
    }
  }

  return facts;
}

/**
 * Which of the panel's three bands a fact belongs to: red first, then amber
 * that waits on the operator, then amber that clears on its own.
 *
 * Here rather than beside the sort that uses it (top-nav.tsx's
 * useStatusFacts) so that the ordering the boards specify has one
 * definition — a test that kept a copy of this would pass while the panel
 * showed something else.
 */
export function factBand(fact: StatusFact): number {
  if (fact.tone === "red") return 0;
  return fact.waitsOnOperator ? 1 : 2;
}

/**
 * Whether anything in the resolver's status is worth watching — the one
 * predicate the status poll reads.
 *
 * Defined as "the panel has a row", rather than as a second list of
 * conditions beside `statusFacts`, because the two used to be written out
 * separately and drifted: the poll was gated on `encryption_downgraded`
 * alone, and when a milestone added two more facts to the same endpoint
 * nobody widened it, so a bind failure that later cleared left its banner
 * up until the operator navigated away and back. One definition is what
 * keeps "we show this" and "we keep asking about this" from parting again.
 *
 * `undefined` is not "wrong": a status that has not loaded, or would not
 * load, is unknown, and the retry policy for that belongs to the query, not
 * here.
 */
export function somethingIsWrong(status: ResolverStatus | undefined): boolean {
  return statusFacts(status).length > 0;
}

const MONTHS = [
  "Jan",
  "Feb",
  "Mar",
  "Apr",
  "May",
  "Jun",
  "Jul",
  "Aug",
  "Sep",
  "Oct",
  "Nov",
  "Dec",
] as const;

/**
 * "14 Nov 2026" — the exact shape the Protocols artboard's certificate line
 * and the certificate fact in the status panel both use.
 *
 * Built from UTC calendar fields, not the browser's local timezone: the
 * server's `not_after` is an instant, and reading it through whichever
 * timezone a viewer happens to be in would make two operators watching the
 * same server disagree on the date printed — and would make this
 * untestable without pinning the test runner's TZ. `toLocaleDateString` was
 * also ruled out for the same determinism reason: its short-month spelling
 * of September varies by locale/ICU data (`Sep` vs `Sept`), which the
 * artboard's exact string does not allow for.
 */
export function formatCertDate(date: Date): string {
  return `${date.getUTCDate()} ${MONTHS[date.getUTCMonth()]} ${date.getUTCFullYear()}`;
}

/** Whole days remaining until `notAfter`, floored at 0 so a certificate
 * that has already lapsed reads as "0 days" rather than a negative number.
 *
 * Floor, not round: "whole days remaining" is what the caller renders, and
 * rounding turns 36 hours into "2 days" — a day of grace the operator does
 * not have. A certificate 12 hours from lapsing reads "0 days", which is
 * the honest number. */
export function daysUntil(notAfter: Date, now: Date = new Date()): number {
  return Math.max(0, Math.floor((notAfter.getTime() - now.getTime()) / 86_400_000));
}

/** "Expires in 9 days — 17 Sep 2026", the status panel's certificate fact
 * and (with a different lead-in) the certificate line's shape for an
 * expiring certificate — built once here so the two copies of "N days —
 * DATE" can't drift apart.
 *
 * `expired` is separate from `days === 0`, and both callers need it: the
 * server reports a lapsed certificate as ok=true with expiring_soon=true
 * (internal/app/serve.go's CertExpiry), so without this the screen said
 * "expires in 0 days" about a certificate that expired last week. */
export function expiringSoonDetail(
  certificate: CertificateStatus,
  now: Date = new Date(),
): { days: number; date: string; expired: boolean } {
  const notAfter = new Date(certificate.not_after);
  return {
    days: daysUntil(notAfter, now),
    date: formatCertDate(notAfter),
    expired: notAfter.getTime() <= now.getTime(),
  };
}
