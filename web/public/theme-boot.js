// Applies the theme before the bundle runs, so a dark install doesn't paint
// light for the length of a script fetch and parse. src/lib/theme.ts owns the
// rule; this is the same rule, early: a stored choice wins, otherwise follow
// the OS.
//
// A separate file rather than an inline <script> because the served CSP is
// `script-src 'self'` with no 'unsafe-inline' (internal/api/static.go) — an
// inline block would be silently blocked in production and only work in dev.
(function () {
  var stored = null;
  try {
    stored = localStorage.getItem("dnsaur.theme");
  } catch (e) {
    // localStorage unavailable (private browsing, disabled storage) — fall
    // through to the OS preference, exactly as getStoredTheme() does.
  }
  var dark =
    stored === "dark" ||
    (stored !== "light" && window.matchMedia("(prefers-color-scheme: dark)").matches);
  document.documentElement.classList.toggle("dark", dark);
})();
