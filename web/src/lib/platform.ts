/**
 * Which modifier this machine's keyboard actually has.
 *
 * The command palette's listener accepts either (`event.metaKey ||
 * event.ctrlKey`, components/command-palette.tsx) because both are the right
 * shortcut on the platform that has them — but the hint in the chrome can
 * only show one, and it showed ⌘ on every platform, naming a key most of
 * them don't have.
 *
 * `navigator.platform` is deprecated but still populated by every browser
 * dnsaur runs in, and it is the only reading that stays honest under an
 * iPadOS user-agent claiming to be a Mac. `userAgent` is the fallback for
 * whatever stops populating it.
 */
const APPLE_PLATFORM = /mac|iphone|ipad|ipod/i;

export function isApplePlatform(): boolean {
  if (typeof navigator === "undefined") return false;
  return APPLE_PLATFORM.test(navigator.platform || navigator.userAgent);
}

/** The command palette's shortcut, spelled for this platform. */
export function paletteShortcut(): string {
  return isApplePlatform() ? "⌘K" : "Ctrl+K";
}
