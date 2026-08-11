/**
 * Handing a fetched file to the browser as a download.
 *
 * This is the app's only download path (zone file export, Milestone C). It
 * exists as its own module rather than inline in the hook because the DOM
 * dance below is the part that is easy to get subtly wrong, and keeping it
 * in one place means the hook's test can assert on the request while this
 * stays a single, reviewable function.
 */

/**
 * Saves `blob` under `filename`, via the one mechanism that works without a
 * server round trip: an anchor carrying the `download` attribute.
 *
 * The anchor is appended to the document before it is clicked. That looks
 * redundant — the element already exists — but Firefox ignores a click on a
 * disconnected anchor, so a detached one silently downloads nothing there
 * while working fine in Chromium.
 *
 * The object URL is revoked immediately after the click. The download has
 * already been handed to the browser by then and holds its own reference to
 * the blob; skipping the revoke would pin the whole file in memory until the
 * tab is closed, which for a large zone is exactly the file we just fetched.
 */
export function downloadBlob(blob: Blob, filename: string): void {
  const url = URL.createObjectURL(blob);
  const anchor = document.createElement("a");
  anchor.href = url;
  anchor.download = filename;
  anchor.rel = "noopener";
  document.body.append(anchor);
  anchor.click();
  anchor.remove();
  URL.revokeObjectURL(url);
}

/**
 * The filename a response asked to be saved under, from its
 * `Content-Disposition`, falling back to `fallback` when the header is
 * absent or unparseable.
 *
 * `filename*` (RFC 5987, percent-encoded, charset-tagged) wins over plain
 * `filename` when both are present, which is the order the RFC specifies —
 * the plain one is the ASCII-only fallback for old clients.
 *
 * Any directory part is stripped from whatever comes back. A server is not
 * supposed to send one, but `download` treats the value as a bare name and a
 * header saying `../../x` should never be able to suggest a path.
 */
export function filenameFromDisposition(header: string | null, fallback: string): string {
  if (!header) return fallback;

  const extended = /filename\*\s*=\s*[^']*'[^']*'([^;]+)/i.exec(header);
  if (extended?.[1]) {
    try {
      const decoded = basename(decodeURIComponent(extended[1].trim()));
      if (decoded) return decoded;
    } catch {
      /* a malformed percent-escape falls through to `filename` below */
    }
  }

  const plain = /filename\s*=\s*("([^"]*)"|[^;]+)/i.exec(header);
  const raw = plain?.[2] ?? plain?.[1];
  const name = raw ? basename(raw.trim()) : "";
  return name || fallback;
}

function basename(value: string): string {
  return value.split(/[\\/]/).pop() ?? "";
}
