import type { CertificateStatus, ProtocolStatus, ResolverStatus } from "../api/types";

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
    (status.certificate?.expiring_soon ?? false)
  );
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
