import "@testing-library/jest-dom";
import { afterAll, afterEach, beforeAll } from "vitest";
import { server } from "./msw-server";

beforeAll(() => server.listen({ onUnhandledRequest: "error" }));
afterEach(() => server.resetHandlers());
afterAll(() => server.close());

// jsdom does not implement matchMedia. Stub it so components that read
// prefers-color-scheme (theme) or use rnui's mobile-breakpoint hook
// (Sidebar) don't crash under test.
if (typeof window.matchMedia !== "function") {
  window.matchMedia = (query: string): MediaQueryList =>
    ({
      matches: false,
      media: query,
      onchange: null,
      addListener: () => {},
      removeListener: () => {},
      addEventListener: () => {},
      removeEventListener: () => {},
      dispatchEvent: () => false,
    }) as MediaQueryList;
}

// jsdom implements neither URL.createObjectURL nor its revoke half (they
// belong to the file/blob layer it deliberately leaves out). lib/download.ts
// needs both to hand a fetched zone file to the browser, so stub them to the
// smallest thing that keeps the round trip honest: a real, unique URL string
// per blob, and a revoke that forgets it. Tests assert createObjectURL was
// handed a Blob (pages/zones/detail.test.tsx) — the point being that the
// response body actually reaches the download, not that any particular URL
// comes back.
if (typeof URL.createObjectURL !== "function") {
  let n = 0;
  URL.createObjectURL = () => `blob:dnsaur/${++n}`;
  URL.revokeObjectURL = () => {};
}

// jsdom implements no navigation and no downloads, so clicking the anchor
// lib/download.ts builds raises "Not implemented: navigation to another
// Document" on the virtual console for every export.
//
// Cancelling the event in the capture phase is what a real browser's own
// behaviour amounts to here: an anchor carrying `download` saves the file
// instead of navigating, so the one thing that must NOT happen on this click
// is a navigation. Suppressing the activation behaviour rather than stubbing
// HTMLAnchorElement.prototype.click keeps the click itself real — it still
// dispatches, still bubbles, and a listener could still observe it.
document.addEventListener(
  "click",
  (event) => {
    const target = event.target as Element | null;
    if (target?.closest?.("a[download]")) event.preventDefault();
  },
  true,
);

// jsdom does not implement ResizeObserver. rnui's InputOTP (via the
// input-otp library) observes its container to size itself, so stub it out
// under test.
if (typeof window.ResizeObserver !== "function") {
  window.ResizeObserver = class ResizeObserver {
    observe() {}
    unobserve() {}
    disconnect() {}
  };
}

// jsdom does not implement elementFromPoint. input-otp polls it (to detect
// password-manager badges overlapping the input) on a timer while focused,
// which otherwise throws an unhandled "not a function" error after the
// test that focused an InputOTP has already finished.
if (typeof document.elementFromPoint !== "function") {
  document.elementFromPoint = () => null;
}

// jsdom does not implement 2D canvas rendering (getContext('2d') returns
// null — the real implementation needs the native `canvas` npm package,
// which this project deliberately doesn't install just for tests). rnui's
// chart components (ECharts, via zrender's canvas renderer) grab a 2D
// context on mount and call a broad set of drawing methods on it; without
// this, any page that renders a chart (e.g. the dashboard's timeline)
// throws inside a layout effect and gets swallowed by the nearest
// ErrorBoundary instead of actually rendering. Stub getContext('2d') with
// a permissive no-op proxy — nothing under test asserts on actual pixels
// (chart data is asserted on props/text, not canvas output; see
// pages/dashboard.test.tsx), so a no-op context is sufficient to let
// mount/update/dispose run without throwing.
//
// The one thing this stub deliberately does NOT no-op is gradient color
// stops. Canvas2D cannot resolve CSS custom properties: a real browser
// throws `SyntaxError: ... 'var(--chart-1)' could not be parsed as a color`
// out of CanvasGradient.addColorStop, which React's commit-phase error
// propagation turns into the ErrorBoundary fallback replacing the whole
// page. An `addColorStop: () => {}` stub swallowed exactly that, so the
// dashboard shipped crashing on any instance that had served a single
// query while 139 tests stayed green. Rejecting the same values the browser
// rejects is what makes pages/dashboard-chart.test.tsx a real guard.
function assertCanvasColor(_offset: number, color: string): void {
  if (typeof color === "string" && color.includes("var(")) {
    throw new SyntaxError(
      `Failed to execute 'addColorStop' on 'CanvasGradient': The value provided ('${color}') could not be parsed as a color.`,
    );
  }
}

// The same rule, one level up, for the *solid* paint properties — and here
// the stub is deliberately STRICTER than a browser. A real Canvas2D silently
// ignores an unparseable `fillStyle`, keeping whatever was set before, so a
// bar chart handed `var(--chart-blocked)` doesn't crash: it just paints every
// bar the wrong colour, with nothing anywhere saying so. That's a worse
// failure than the gradient one, not a better one — it ships looking almost
// right. Since nothing in this app ever has a legitimate reason to put a
// custom property into canvas paint, refusing it outright turns a silent
// mispaint into a failing test.
const PAINT_PROPS = new Set(["fillStyle", "strokeStyle", "shadowColor"]);

function assertPaintValue(prop: string, value: unknown): void {
  if (PAINT_PROPS.has(prop) && typeof value === "string" && value.includes("var(")) {
    throw new SyntaxError(
      `canvas ${prop} was given an unresolved CSS custom property ('${value}'). ` +
        "Resolve tokens to concrete values before handing them to a chart.",
    );
  }
}

if (typeof HTMLCanvasElement !== "undefined") {
  const noopCanvasContext = (): CanvasRenderingContext2D => {
    const state: Record<string | symbol, unknown> = {};
    return new Proxy(state, {
      get(target, prop) {
        if (prop in target) return target[prop];
        if (prop === "measureText") return () => ({ width: 0 });
        if (
          prop === "createLinearGradient" ||
          prop === "createRadialGradient" ||
          prop === "createPattern"
        ) {
          return () => ({ addColorStop: assertCanvasColor });
        }
        if (prop === "getImageData") return () => ({ data: new Uint8ClampedArray(4) });
        return () => undefined;
      },
      set(target, prop, value) {
        if (typeof prop === "string") assertPaintValue(prop, value);
        target[prop] = value;
        return true;
      },
    }) as unknown as CanvasRenderingContext2D;
  };

  HTMLCanvasElement.prototype.getContext = ((contextId: string) =>
    contextId === "2d"
      ? noopCanvasContext()
      : null) as typeof HTMLCanvasElement.prototype.getContext;
}

// jsdom does not implement Element.getAnimations() (used by base-ui's
// ScrollArea — which rnui's dropdown menus, Filters, and DataGrid all use
// internally — to detect thumb-visibility fade animations via a delayed
// timeout callback). Without this, the callback throws asynchronously
// after the triggering test has already finished, surfacing as an
// "unhandled error" that pollutes unrelated test output. A no-op empty
// list is sufficient — no test asserts on actual scrollbar animations.
if (typeof Element !== "undefined" && typeof Element.prototype.getAnimations !== "function") {
  Element.prototype.getAnimations = () => [];
}

// jsdom does not implement Element.scrollIntoView (cmdk calls it whenever
// the command palette's active item changes, including on first mount).
// A no-op is sufficient: no test asserts on scroll position, only on which
// item is selected.
if (typeof Element !== "undefined" && typeof Element.prototype.scrollIntoView !== "function") {
  Element.prototype.scrollIntoView = () => {};
}
