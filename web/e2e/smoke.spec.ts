import { expect, test } from "@playwright/test";

// One end-to-end path through the real embedded build (see
// ../playwright.config.ts's webServer): first-run setup creates the admin
// account, then a real login (not just the wizard's own silent sign-in),
// then a concrete CRUD action (create a zone and add a record to it), a
// persisted preference (dark mode surviving a reload) and a write whose
// wire format only the real server can judge (a TSIG key, whose algorithm
// values differ between what the API accepts and what the screen shows).
// Deliberately a single spec, not a suite — this is a ship gate ("does the
// real build actually work end to end"), not page-by-page coverage; that's
// what the Vitest component tests are for.
const USERNAME = "e2e-admin";
const PASSWORD = "correct horse battery staple";

test("first-run setup, login, create a zone and add a record, dark mode persists across reload, create a TSIG key", async ({
  page,
}) => {
  // --- first-run setup: create the admin account -----------------------
  await page.goto("/");
  await expect(page.getByRole("heading", { name: "Set up dnsaur" })).toBeVisible();

  // The wizard opens on a welcome screen; the account form is behind it.
  await page.getByRole("button", { name: "Get started" }).click();
  await expect(page.getByRole("heading", { name: "Create your admin" })).toBeVisible();

  await page.getByLabel("Username").fill(USERNAME);
  await page.getByLabel("Password", { exact: true }).fill(PASSWORD);
  await page.getByLabel("Confirm password").fill(PASSWORD);
  await page.getByRole("button", { name: "Create account" }).click();

  // Skip the starter-blocklists step (finishing would kick off real network
  // fetches for the StevenBlack/hagezi/OISD URLs) — this smoke test only
  // needs an admin account and a session, not a configured filter setup.
  await page.getByRole("button", { name: "Skip for now" }).click();
  await expect(page.getByRole("heading", { name: "dnsaur is ready" })).toBeVisible();
  await page.getByRole("button", { name: /open the dashboard/i }).click();
  await expect(page.getByRole("heading", { name: "Dashboard" })).toBeVisible();

  // The dashboard is composed from rnui: four StatCards in the hairline
  // grid, each naming what it actually counts, and the chart's three bands
  // legended below them. A fresh instance has served nothing, so the
  // numbers are zeroes — the labels are the part that has to be there.
  const statStrip = page.getByRole("region", { name: /^Query stats/ });
  for (const stat of ["Queries", "Blocked", "Cached", "Client IPs seen"]) {
    // `exact` because each title is also a substring of its own
    // description ("Blocked" / "0 blocked").
    await expect(statStrip.getByText(stat, { exact: true })).toBeVisible();
  }
  await expect(page.getByText("distinct client_ip, not client rows")).toBeVisible();
  await expect(page.getByText("hourly buckets · stats lag the log by up to 60s")).toBeVisible();

  // The shell is two chrome rows, not a sidebar (see
  // src/components/top-nav.tsx): row 1 holds the four nav *group* menus,
  // row 2 the active group's pages as tabs. Account, theme and log out all
  // live under System.
  const topNav = page.locator('[data-slot="top-nav"]');
  const systemMenu = topNav.getByRole("button", { name: "System" });

  // --- log out, then log back in through the real Login page -----------
  // The wizard's own silent sign-in (see pages/setup.tsx) already holds a
  // session at this point; it never exercises pages/login.tsx, so log out
  // and back in explicitly to cover the real login path the brief asks for.
  await systemMenu.click();
  // The signed-in account is named in the menu, not in the (avatar-less) bar.
  await expect(page.getByRole("menu").getByText(USERNAME)).toBeVisible();
  await page.getByRole("menuitem", { name: "Log out" }).click();
  await expect(page.getByRole("heading", { name: "Log in to dnsaur" })).toBeVisible();

  await page.getByLabel("Username").fill(USERNAME);
  await page.getByLabel("Password").fill(PASSWORD);
  await page.getByRole("button", { name: "Log in" }).click();
  await expect(page.getByRole("heading", { name: "Dashboard" })).toBeVisible();

  // --- the row-1 search cell opens the ⌘K palette -----------------------
  // The shortcut is a real <kbd>, not two more characters of label.
  const search = topNav.getByRole("button", { name: /search/i });
  await expect(search.locator("kbd")).toHaveText("⌘K");
  await search.click();
  await expect(page.getByPlaceholder("Jump to a page…")).toBeVisible();
  await page.keyboard.press("Escape");

  // --- the query log's row-2 cells belong to the shell ------------------
  // One readout of what the table is doing and one filled toggle, both in
  // row 2 (an earlier revision had the page render its own pair as well,
  // which put four readouts of one flag on this screen).
  await topNav.getByRole("navigation", { name: "Monitor" }).getByText("Query Log").click();
  const modeCell = topNav.getByRole("status", { name: "Query log" });
  await expect(modeCell).toHaveText(/live tail|reconnecting/i);
  const tailToggle = topNav.getByRole("button", { name: /pause tail/i });
  await tailToggle.click();
  await expect(modeCell).toHaveText(/paused/i);
  await expect(topNav.getByRole("button", { name: /resume tail/i })).toBeVisible();
  await topNav.getByRole("button", { name: /resume tail/i }).click();
  await expect(topNav.getByRole("button", { name: /pause tail/i })).toBeVisible();

  // --- create a zone, open it, add a record, see it listed --------------
  // Zones is the group's only page and now its name too, so it's reached
  // through that group's menu — and once there, row 2 must still give it a
  // tab of its own.
  await topNav.getByRole("button", { name: "Zones", exact: true }).click();
  await page.getByRole("menuitem", { name: "Zones" }).click();
  // The list page carries no heading of its own — the chrome names it, so
  // row 2's marked tab is both the label and the proof we landed here.
  await expect(
    topNav.getByRole("navigation", { name: "Zones" }).getByRole("link", { name: "Zones" }),
  ).toHaveAttribute("aria-current", "page");

  // A fresh instance seeds 15 RFC 6303 built-in zones (localhost, the
  // reverse-mapping arpa zones, …) at migration, but the list groups them
  // behind their own collapsed disclosure and never counts them as the
  // user's own — so this is still the empty state, with both the header's
  // and the empty state's own "New zone" visible. `.first()` picks either.
  await page.getByRole("button", { name: "New zone" }).first().click();
  await page.getByLabel("Zone name").fill("home.lan");
  await page.getByRole("button", { name: "Add", exact: true }).click();

  // The zone name is a link straight into its detail page (Task 12). Exact,
  // since the row's own Edit icon-button renders as a same-target link too
  // ("Edit home.lan").
  await page.getByRole("link", { name: "home.lan", exact: true }).click();

  // The 15 built-ins stay behind their collapsed disclosure (never
  // expanded in this test), so the list page shows exactly one row —
  // home.lan — at this point. But a bare page-wide "Enabled" text query is
  // still ambiguous: the click above resolves the URL before React
  // actually swaps the DOM, leaving a window where the list page's own
  // "Enabled" is still rendered alongside the detail page's header, which
  // says it too. Scope to the element for *this* zone specifically
  // instead: "home.lan" is always a unique text node (it's the only zone
  // with that name, on either page), and its immediate parent is exactly
  // the row or header band that also holds its own status text — never
  // another zone's.
  const zoneName = page.getByText("home.lan", { exact: true });
  await expect(zoneName).toBeVisible();
  const zoneEntry = zoneName.locator("xpath=..");
  await expect(zoneEntry.getByText("Enabled", { exact: true })).toBeVisible();

  // The add form is the records grid's own row, not a dialog — but it is
  // closed until Add record opens it, so that comes first. Data is one text
  // input regardless of record type — the DNS parser validates it
  // server-side, there is no per-type form.
  await page.getByRole("button", { name: "Add record" }).click();
  await page.getByLabel("Zone record name").fill("bifrost");
  await page.getByLabel("Data", { exact: true }).fill("10.0.0.9");
  await page.getByRole("button", { name: "Add", exact: true }).click();

  // Rows are a CSS grid rather than a table, so they are found by the slot
  // the page marks them with.
  const row = page.locator('[data-testid="zone-record-row"]', { hasText: "bifrost" });
  await expect(row).toBeVisible();
  await expect(row.getByText("10.0.0.9")).toBeVisible();
  // Adding leaves the row empty and ready for the next record.
  await expect(page.getByLabel("Zone record name")).toHaveValue("");

  // --- export the zone, then re-import the file it produced -------------
  // A round trip, so the imported file is guaranteed to be one this
  // server's own renderer emits rather than a fixture hand-written against
  // a guess at the parser. It also puts a real download and a real
  // multipart-free file read through Chromium, neither of which jsdom can
  // do: the Vitest suite stubs URL.createObjectURL and suppresses the
  // anchor's navigation, so this is the only place the download is real.
  const [download] = await Promise.all([
    page.waitForEvent("download"),
    page.getByRole("button", { name: "Export" }).click(),
  ]);
  expect(download.suggestedFilename()).toBe("home.lan.zone");
  const exported = await download.path();

  await page.getByLabel("Zone file").setInputFiles(exported);
  const importDialog = page.getByRole("dialog");
  await expect(importDialog).toBeVisible();

  // The file *is* the zone, so the diff is empty in all three buckets.
  await expect(importDialog.getByText(/this file matches the zone/i)).toBeVisible();

  // The dialog's width is the assertion jsdom structurally cannot make: the
  // DOM is correct in both cases and only the computed cascade differs.
  // rnui's base DialogContent carries `sm:max-w-sm`, which tailwind-merge
  // cannot strip from a `cn()` override because it sits in a different
  // variant scope — so without an `sm:`-scoped max-width of our own, this
  // 900px panel silently renders at 384px on any viewport ≥640px, with the
  // counts strip and the three diff groups crushed into a third of their
  // designed width. Pinned here because every RTL test passes either way.
  const panel = await importDialog.boundingBox();
  expect(panel?.width).toBe(900);

  await importDialog.getByRole("button", { name: "Cancel" }).click();
  await expect(importDialog).toBeHidden();

  // --- dark mode toggles and persists across a reload -------------------
  // Also under System: the design's bar carries no standalone theme control.
  await expect(page.locator("html")).not.toHaveClass(/dark/);
  await systemMenu.click();
  await page.getByRole("menuitem", { name: /switch to dark theme/i }).click();
  await expect(page.locator("html")).toHaveClass(/dark/);
  await page.keyboard.press("Escape"); // the toggle deliberately stays open

  await page.reload();
  await expect(
    topNav.getByRole("navigation", { name: "Zones" }).getByRole("link", { name: "Zones" }),
  ).toHaveAttribute("aria-current", "page");
  await expect(page.locator("html")).toHaveClass(/dark/);
  await expect(row).toBeVisible(); // the record survived the reload too

  // --- create a TSIG key against the real handler -----------------------
  // Last, because it navigates away from the zone the assertions above are
  // about. This is the one thing about that screen jsdom structurally
  // cannot check: whether the algorithm the select submits is a value the
  // *server* accepts. The API's values are miekg's constants and carry a
  // trailing dot ("hmac-sha256.") while the design shows them without one,
  // and a mocked POST would accept either — the real handler validates
  // against its own set and 400s anything else. The name is the same story:
  // what comes back is canonical, not what was typed.
  await systemMenu.click();
  await page.getByRole("menuitem", { name: "TSIG keys" }).click();
  await expect(
    topNav.getByRole("navigation", { name: "System" }).getByRole("link", { name: "TSIG keys" }),
  ).toHaveAttribute("aria-current", "page");

  // No keys on a fresh instance, so "New key" is offered twice (the header
  // and the empty state); `.first()` picks either.
  await page.getByRole("button", { name: "New key" }).first().click();
  // Generated, not typed — the field opens pre-filled from
  // crypto.getRandomValues (see src/lib/tsig.ts).
  const generatedSecret = await page.getByLabel("Secret", { exact: true }).inputValue();
  expect(generatedSecret).toMatch(/^[A-Za-z0-9+/]{43}=$/);
  await page.getByLabel("Key name").fill("XFER.e412.IN");
  await expect(page.getByText("Saved as xfer.e412.in.")).toBeVisible();
  // Picked **by label** — by what the design shows — so what reaches the
  // server is whatever the option was valued with. This is what makes the
  // segment a test of the mapping rather than of the default constant:
  // leaving the select untouched submits DEFAULT_TSIG_ALGORITHM whatever the
  // DOM says, so dotless option values would sail through. (The default's own
  // dotted form is held by the type system — DEFAULT_TSIG_ALGORITHM is a
  // TSIGAlgorithm, so a dotless literal doesn't compile — and asserted on the
  // POST body in pages/tsig-keys.test.tsx.) A non-default algorithm also
  // proves the row sends what was chosen rather than the default.
  await page.getByLabel("Algorithm").selectOption({ label: "hmac-sha512" });
  await page.getByRole("button", { name: "Add", exact: true }).click();

  const keyRow = page.locator('[data-testid="tsig-key-row"]', { hasText: "xfer.e412.in." });
  await expect(keyRow).toBeVisible();
  await expect(keyRow.getByText("hmac-sha512", { exact: true })).toBeVisible();
  // Masked at rest; the eye is what puts it on screen, and what it puts
  // there is byte-for-byte what the create row generated and the server
  // stored.
  await expect(keyRow.getByText("•".repeat(24))).toBeVisible();
  await keyRow.getByRole("button", { name: "Show the secret for xfer.e412.in." }).click();
  await expect(keyRow.getByText(generatedSecret, { exact: true })).toBeVisible();
});
