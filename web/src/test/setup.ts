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
