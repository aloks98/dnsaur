import type { CertificateStatus, ProtocolStatus, ResolverStatus, SyncStatus } from "../api/types";
import { formatDuration } from "./format";

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
 * Shared between protocols-field.tsx's per-row line and serving-banners.tsx's
 * "enabled but not listening" banner, so the two never drift on what
 * counts as broken.
 */
type ServingState = "off" | "listening" | "failed";

export function servingState(status: ProtocolStatus): ServingState {
  if (!status.enabled) return "off";
  return status.listening ? "listening" : "failed";
}

/**
 * Whether anything in the resolver's status is worth watching — the one
 * predicate the status poll and the shell banners both read.
 *
 * It lives beside `servingState` for the same reason `servingState` exists:
 * the poll used to be gated on `encryption_downgraded` alone, and when this
 * milestone added two more facts to the same endpoint nobody widened it, so
 * a bind failure that later cleared left its banner up until the operator
 * navigated away and back. One predicate is what keeps "we show a banner
 * for this" and "we keep asking about this" from drifting apart again.
 *
 * `undefined` is not "wrong": a status that has not loaded, or would not
 * load, is unknown, and the retry policy for that belongs to the query, not
 * here.
 */
export function somethingIsWrong(status: ResolverStatus | undefined): boolean {
  if (!status) return false;
  return (
    status.encryption_downgraded ||
    servingState(status.serving.dot) === "failed" ||
    servingState(status.serving.doh) === "failed" ||
    (status.certificate?.expiring_soon ?? false) ||
    syncTrouble(status.sync)
  );
}

/**
 * The sync facts a poll can actually clear: a pull that failed, and a
 * replica that has gone quiet. Both end without anyone doing anything — the
 * next successful pull, the next check-in — which is exactly what makes
 * asking again worth the request.
 *
 * `plain_http` is deliberately **not** here even though the strip shows it.
 * It is a reading of the peer URL the operator typed, so nothing but an
 * edit to that URL can change the answer, and polling every five seconds
 * for as long as it stays `http://` would ask a question whose answer
 * cannot move. The banner persists on its own; the poll does not have to.
 */
function syncTrouble(sync: SyncStatus | undefined): boolean {
  if (!sync) return false;
  return Boolean(sync.last_error) || (sync.replicas ?? []).some((r) => r.stale);
}

/**
 * Sync's three facts, as the lines the shell shows (spec §8) — the one
 * definition of what is on screen, so a caller cannot invent a fourth line
 * or spell one of these differently.
 *
 * What is *shown* and what is *polled for* are deliberately not the same
 * set here, which is why this is not the predicate `somethingIsWrong`
 * reads: see `syncTrouble` above for the one fact that is worth a banner
 * and not worth a request.
 *
 * Two of the three clear on their own — the next pull that succeeds empties
 * `last_error`, and a replica that checks in stops being stale — which is
 * why `syncTrouble` below watches those two and not `plain_http`, whose
 * only cure is editing the peer URL.
 *
 * Every line states the fact and stops. A replica that is merely *behind*
 * gets no line at all: that is what a pull interval looks like from the
 * outside, and the Sync band's "applied n of m" already says it on the one
 * screen where the number is worth reading.
 */
export function syncBanners(sync: SyncStatus | undefined, now: number = Date.now()): string[] {
  if (!sync) return [];
  const lines: string[] = [];
  if (sync.last_error) lines.push(`Last pull failed: ${sync.last_error}`);
  // Persistent while it is true — the bundle carries every TSIG secret on
  // the box, so a plaintext peer is not a transient wobble to wait out.
  if (sync.plain_http) lines.push("Peer reached over plain HTTP");
  for (const replica of sync.replicas ?? []) {
    if (!replica.stale) continue;
    // How long it has been quiet, not when it was last heard: an operator
    // should not have to subtract a wall-clock stamp from now.
    lines.push(
      `Replica ${replica.instance_id} not seen for ${formatDuration(now - replica.last_seen)}`,
    );
  }
  return lines;
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
 * and the certificate-expiring shell banner both use.
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

/** "Expires in 9 days — 17 Sep 2026", the shell banner and (with a
 * different lead-in) the certificate line's shape for an expiring
 * certificate — built once here so the two copies of "N days — DATE" can't
 * drift apart.
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
