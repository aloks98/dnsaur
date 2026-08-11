import { afterEach, expect, test, vi } from "vitest";
import { downloadBlob, filenameFromDisposition } from "./download";

afterEach(() => vi.restoreAllMocks());

/**
 * Records every anchor `downloadBlob` builds, by intercepting the one
 * `document.createElement("a")` it makes.
 *
 * The anchor is appended, clicked and removed inside the call, so by the time
 * the function returns there is nothing in the DOM left to assert on — the
 * spy is the only way to see what was actually handed to the browser. `click`
 * is stubbed rather than left real so the assertion is about the anchor being
 * clicked at all, independent of the a[download] navigation suppressor in
 * test/setup.ts.
 */
function captureAnchor() {
  const anchor = document.createElement("a");
  const click = vi.spyOn(anchor, "click").mockImplementation(() => {});
  vi.spyOn(document, "createElement").mockImplementation(((tag: string) =>
    tag === "a"
      ? anchor
      : Reflect.apply(HTMLDocument.prototype.createElement, document, [
          tag,
        ])) as typeof document.createElement);
  return { anchor, click };
}

test("downloadBlob clicks an anchor carrying the blob and the filename", () => {
  const { anchor, click } = captureAnchor();
  const createUrl = vi.spyOn(URL, "createObjectURL").mockReturnValue("blob:test/1");
  const revokeUrl = vi.spyOn(URL, "revokeObjectURL");
  const blob = new Blob(["$ORIGIN e412.in.\n"], { type: "text/dns" });

  downloadBlob(blob, "e412.in.zone");

  // The click is the whole mechanism: without it the anchor is built,
  // attached and removed having downloaded precisely nothing.
  expect(click).toHaveBeenCalledTimes(1);
  expect(createUrl).toHaveBeenCalledWith(blob);
  expect(anchor.getAttribute("href")).toBe("blob:test/1");
  expect(anchor.getAttribute("download")).toBe("e412.in.zone");
  // Revoked, or the whole file stays pinned in memory until the tab closes.
  expect(revokeUrl).toHaveBeenCalledWith("blob:test/1");
});

test("downloadBlob leaves nothing behind in the document", () => {
  vi.spyOn(URL, "createObjectURL").mockReturnValue("blob:test/2");
  vi.spyOn(URL, "revokeObjectURL").mockImplementation(() => {});
  const clicked = vi.fn<(event: Event) => void>();
  document.addEventListener("click", clicked, true);

  downloadBlob(new Blob(["x"]), "x.zone");

  document.removeEventListener("click", clicked, true);
  // The anchor has to be connected when clicked — Firefox ignores a click on
  // a detached one — but must not survive the call.
  expect(clicked).toHaveBeenCalledTimes(1);
  expect(document.querySelector("a[download]")).toBeNull();
});

test("filenameFromDisposition reads the name the server asked for", () => {
  const cases: [string | null, string][] = [
    // What the export handler actually sends.
    ['attachment; filename="e412.in.zone"', "e412.in.zone"],
    // Unquoted, and with the parameters in either order.
    ["attachment; filename=home.lan.zone", "home.lan.zone"],
    ["filename=home.lan.zone; attachment", "home.lan.zone"],
    // Whitespace around the "=" is legal.
    ['attachment; filename = "spaced.zone"', "spaced.zone"],
    // RFC 5987 wins over the ASCII-only fallback when both are present:
    // the plain one exists for clients that cannot read the extended form.
    ["attachment; filename=\"ascii.zone\"; filename*=UTF-8''caf%C3%A9.zone", "café.zone"],
    ["attachment; filename*=UTF-8''caf%C3%A9.zone", "café.zone"],
    // A malformed percent-escape falls back rather than throwing.
    ["attachment; filename=\"ok.zone\"; filename*=UTF-8''bad%zz.zone", "ok.zone"],
    // No header at all, or nothing usable in it.
    [null, "fallback.zone"],
    ["attachment", "fallback.zone"],
    ['attachment; filename=""', "fallback.zone"],
  ];
  for (const [header, want] of cases) {
    expect(filenameFromDisposition(header, "fallback.zone"), `header: ${header}`).toBe(want);
  }
});

// `download` treats its value as a bare filename, so a directory part in the
// header should never be able to suggest a path. The server does not send
// one; this is about what happens if anything ever does.
test("filenameFromDisposition strips any directory part", () => {
  expect(filenameFromDisposition('attachment; filename="../../etc/passwd"', "fallback.zone")).toBe(
    "passwd",
  );
  expect(filenameFromDisposition('attachment; filename="C:\\Windows\\evil.zone"', "f.zone")).toBe(
    "evil.zone",
  );
  expect(filenameFromDisposition("attachment; filename*=UTF-8''%2e%2e%2fesc.zone", "f.zone")).toBe(
    "esc.zone",
  );
  // A value that is nothing but a directory part leaves no name to use.
  expect(filenameFromDisposition('attachment; filename="/"', "fallback.zone")).toBe(
    "fallback.zone",
  );
});
