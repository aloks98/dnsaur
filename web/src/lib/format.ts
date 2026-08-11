/** "never" / "just now" / "5m ago" / "3h ago" / "2d ago" from an epoch-ms
 * timestamp — shared by anything that surfaces a last-seen/last-refreshed
 * moment (dashboard health strip, Filtering's lists table). `0` reads as
 * "never" rather than "56 years ago" (the Unix epoch), matching how these
 * APIs use 0 as a sentinel for "hasn't happened yet". */
export function relativeTime(epochMs: number): string {
  if (!epochMs) return "never";
  const diffSec = Math.max(0, Math.floor((Date.now() - epochMs) / 1000));
  if (diffSec < 60) return "just now";
  const diffMin = Math.floor(diffSec / 60);
  if (diffMin < 60) return `${diffMin}m ago`;
  const diffHour = Math.floor(diffMin / 60);
  if (diffHour < 24) return `${diffHour}h ago`;
  const diffDay = Math.floor(diffHour / 24);
  return `${diffDay}d ago`;
}

/** A file size as `812 B` / `4.1 KB` / `2.3 MB` — the size shown beside a
 * chosen zone file's name in the import dialog. Binary units (1024), since
 * the number describes bytes held in memory rather than anything a disk or a
 * transfer rate reports. */
export function formatBytes(bytes: number): string {
  if (bytes < 1024) return `${bytes} B`;
  const kb = bytes / 1024;
  if (kb < 1024) return `${kb.toFixed(1)} KB`;
  return `${(kb / 1024).toFixed(1)} MB`;
}

/** A duration in milliseconds as `m:ss` — the pause control's countdown to
 * `paused_until`. Never negative (clamps to `0:00` once the pause has
 * technically expired but the 30s status poll hasn't caught up yet). */
export function formatCountdown(ms: number): string {
  const totalSec = Math.max(0, Math.ceil(ms / 1000));
  const minutes = Math.floor(totalSec / 60);
  const seconds = totalSec % 60;
  return `${minutes}:${String(seconds).padStart(2, "0")}`;
}
