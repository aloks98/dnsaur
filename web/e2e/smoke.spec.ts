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

test("first-run setup, login, create a zone and add a record, dark mode persists across reload, create a TSIG key, pull a secondary, route a forwarder", async ({
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

  // The zone name is a link straight into its detail page (Task 12), and now
  // the only one on the row — the pencil that used to point at the same place
  // is gone, folded into the row's kebab along with everything else.
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

  // --- set an allow transfer, through the real handler -------------------
  // allow_transfer is default-deny — a zone created with none set answers
  // every transfer REFUSED — so this field is the only door into letting a
  // secondary pull it at all. Worth a real round trip through the actual
  // PATCH handler and its own store write (Task 10's own D3 field), not
  // just the mocked one the Vitest suite exercises; persistence across a
  // reload (below, alongside dark mode's own) is what proves it was really
  // written rather than only held in this tab's state.
  // Read is the default state; the pencil opens the field.
  await expect(page.getByText("No peer may transfer this zone.")).toBeVisible();
  await page.getByRole("button", { name: "Edit allow transfer" }).click();
  await page.getByLabel("Allow transfer", { exact: true }).fill("10.0.0.0/24");
  await page.getByRole("button", { name: "Save", exact: true }).click();
  await expect(page.getByText("No peer may transfer this zone.")).toBeHidden();
  await expect(page.getByText("10.0.0.0/24")).toBeVisible();

  // --- set a notify target, through the real handler ---------------------
  // notify_to is empty by default — nobody is told when this zone changes
  // — the same shape as allow_transfer above, and worth the same real round
  // trip through the actual PATCH handler (Task 12's own D4 field) rather
  // than only the mocked one the Vitest suite exercises.
  await expect(page.getByText("No targets are notified.")).toBeVisible();
  await page.getByRole("button", { name: "Edit notify targets" }).click();
  await page.getByLabel("Notify to", { exact: true }).fill("10.0.0.6:5353");
  await page.getByRole("button", { name: "Save", exact: true }).click();
  await expect(page.getByText("No targets are notified.")).toBeHidden();
  await expect(page.getByText("10.0.0.6:5353")).toBeVisible();
  // The roll-up itself is not asserted more precisely than this: the real
  // background notifier starts trying this (unreachable) target the moment
  // the server reconciles it, so its exact state is a race this test must
  // not depend on — only that a target now exists to have one at all. All
  // four of notifyRollup's own branches are legal outcomes of that race,
  // including the freshly-added, not-yet-tried "0 of 1 current, 1 never
  // notified" one — the reconcile hasn't necessarily run at all yet.
  await expect(
    page.getByText(/^(no targets|all 1 current|1 of 1 behind|0 of 1 current, 1 never notified)$/),
  ).toBeVisible();

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
  // …and so did the allow transfer — read back from the server, not the tab,
  // and shown in read mode (the default on a fresh load) rather than the
  // input a click on the pencil would have to open first.
  await expect(page.getByText("10.0.0.0/24")).toBeVisible();
  // …and the notify target too, on the same terms.
  await expect(page.getByText("10.0.0.6:5353")).toBeVisible();

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
  // Nothing uses it yet, so it is deletable — the other half is asserted
  // once a zone references it, below.
  await expect(keyRow.getByText("—", { exact: true })).toBeVisible();

  // --- a secondary zone, end to end -------------------------------------
  // The one chain nothing else covers whole: the create row's TSIG select is
  // valued by key *id* while the design shows names, the refresh endpoint is
  // reached through the real app wiring, and the failure it reports is
  // written to a column rather than kept in the process. A mocked POST would
  // accept a name-valued select happily; the real handler 400s an id that
  // names no key, and 400s a secondary with no primaries at all.
  //
  // The primary is a port nothing listens on, so the transfer fails fast and
  // for a reason the operating system supplies rather than this test.
  // Reached through the group menu, the same way as the first zone above —
  // row 2's tab only exists once Zones is the current group.
  await topNav.getByRole("button", { name: "Zones", exact: true }).click();
  await page.getByRole("menuitem", { name: "Zones" }).click();
  await page.getByRole("button", { name: "New zone" }).first().click();
  await page.getByLabel("Zone type").selectOption("secondary");
  await page.getByLabel("Zone name").fill("branch.e412.in");
  await page.getByLabel("Primary servers").fill("127.0.0.1:1");
  // By label, like the algorithm above: what reaches the server is whatever
  // the option was valued with, and only an id is accepted there.
  await page.getByLabel("TSIG key").selectOption({ label: "xfer.e412.in." });
  await page.getByRole("button", { name: "Add", exact: true }).click();

  const secondaryRow = page.locator('[data-testid="zone-row"]', { hasText: "branch.e412.in" });
  await expect(secondaryRow).toBeVisible();
  // Enabled in the database and answering nothing, because it holds nothing
  // it may speak for. An "Enabled" badge here would be the screen's most
  // misleading element.
  await expect(secondaryRow.getByText("Not answering")).toBeVisible();
  // Nothing has been tried yet, and the row says exactly that rather than
  // dating a failure that never happened.
  await expect(secondaryRow.getByTestId("zone-pull-attempt")).toHaveText("never attempted");
  // Which of the two "Not answering" states this is lives in the tooltip —
  // the one thing here a jsdom test cannot really prove, since it needs a
  // browser that actually hovers and a popup that actually positions itself.
  await secondaryRow.getByTestId("zone-status").hover();
  const secondaryTip = page.getByTestId("zone-status-tip");
  await expect(secondaryTip).toContainText("Never transferred");
  await expect(secondaryTip).toContainText("Answering nothing under branch.e412.in.");

  await page.getByRole("link", { name: "branch.e412.in", exact: true }).click();
  await expect(page.getByText("127.0.0.1:1", { exact: true })).toBeVisible();
  // The id round-tripped: the band resolves it back to the name the peer
  // knows the key by. A wrong id would render "#N (missing)".
  await expect(page.getByText("xfer.e412.in.", { exact: true })).toBeVisible();
  // Read-only, because every write route answers 409 for a secondary.
  await expect(page.getByRole("button", { name: /add record/i })).toHaveCount(0);
  await expect(page.getByRole("button", { name: /^import$/i })).toHaveCount(0);

  await page.getByRole("button", { name: "Refresh now" }).click();
  // The transfer's own error, from the server, in the band — on two lines:
  // what actually failed, and the whole message under it. Reloading proves
  // the point of storing it: a page that had only remembered the response
  // would come back blank.
  const rawError = page.getByTestId("transfer-error-raw");
  await expect(rawError).toContainText("127.0.0.1:1");
  await page.reload();
  await expect(page.getByTestId("transfer-error-raw")).toContainText("127.0.0.1:1");

  // The one place a REAL transfer error is checked rather than a fixture, and
  // so the only one that can say the first line is genuinely part of the
  // second. Asserted as a relation — a shorter verbatim tail — rather than as
  // an expected phrase, because the innermost words there are the host's
  // ("connect: connection refused"), not this project's, and the split is a
  // presentation choice that must survive them changing (see
  // transferErrorLead in src/lib/zones.ts).
  const leadText = (await page.getByTestId("transfer-error").textContent()) ?? "";
  const rawText = (await rawError.textContent()) ?? "";
  expect(leadText.length).toBeGreaterThan(0);
  expect(leadText.length).toBeLessThan(rawText.length);
  expect(rawText.endsWith(leadText)).toBe(true);

  // --- a stub zone, and the row that used to lie about it ---------------
  // The bug this list had: a stub was given the plain green "Enabled" of a
  // primary, whatever state its fetch was in. One that has never fetched
  // holds no NS set, and its suffix answers SERVFAIL rather than falling
  // through to the default resolvers — a suffix-wide outage the row drew as
  // health. Worth the real round trip because the server is what decides a
  // stub is created with `primaries` and left with `refreshed_at` at 0.
  //
  // No TSIG key on this one, deliberately: the key's usage count is asserted
  // as "1 zone" at the end of this test.
  await topNav.getByRole("button", { name: "Zones", exact: true }).click();
  await page.getByRole("menuitem", { name: "Zones" }).click();
  await page.getByRole("button", { name: "New zone" }).first().click();
  await page.getByLabel("Zone type").selectOption("stub");
  await page.getByLabel("Zone name").fill("ad.corp.e412.in");
  await page.getByLabel("Primary servers").fill("127.0.0.1:1");
  await page.getByRole("button", { name: "Add", exact: true }).click();

  const stubRow = page.locator('[data-testid="zone-row"]', { hasText: "ad.corp.e412.in" });
  await expect(stubRow).toBeVisible();
  await expect(stubRow.getByText("Not answering")).toBeVisible();
  await expect(stubRow.getByText("Enabled")).toHaveCount(0);
  // The last attempt is dated inside the status cell now rather than in a
  // band across the foot of the row, so the row stays one grid line tall in
  // every other column. Nothing has been tried yet, and it says exactly that.
  await expect(stubRow.getByTestId("zone-pull-attempt")).toHaveText("never attempted");

  // A stub fetches an NS set; it does not transfer, and it never expires. No
  // noun on the row may say otherwise — including the menu item's. The menu
  // is the one part of this row jsdom cannot really open (base-ui wants a
  // pointer sequence it does not have), so this is where it is proved.
  await stubRow.getByRole("button", { name: "Actions for ad.corp.e412.in" }).click();
  const stubMenu = page.getByRole("menu");
  await expect(stubMenu.getByRole("menuitem", { name: "Fetch now" })).toBeVisible();
  await expect(stubMenu.getByRole("menuitem", { name: "Disable" })).toBeVisible();
  await expect(stubMenu.getByRole("menuitem", { name: "Delete zone" })).toBeVisible();
  // Drawn on the artboard, deliberately not built: no rename exists in this
  // app or its API.
  await expect(stubMenu.getByRole("menuitem", { name: "Rename" })).toHaveCount(0);
  await page.keyboard.press("Escape");
  await expect(stubMenu).toHaveCount(0);

  await stubRow.getByTestId("zone-status").hover();
  await expect(page.getByTestId("zone-status-tip")).toContainText("Never fetched");

  // --- a forwarder zone, end to end ------------------------------------
  // The one type whose whole configuration is a single column nothing else
  // in this suite writes. Worth the real round trip for two things a mocked
  // PATCH cannot judge: the server 400s `forward_to` on any type but this
  // one, and it stores the value in its own canonical spelling
  // (FormatForwardTo — the port always written), which is what comes back on
  // the page rather than what was typed.
  await topNav.getByRole("button", { name: "Zones", exact: true }).click();
  await page.getByRole("menuitem", { name: "Zones" }).click();
  await page.getByRole("button", { name: "New zone" }).first().click();
  await page.getByLabel("Zone type").selectOption("forwarder");
  await page.getByLabel("Zone name").fill("corp.example");
  // The create row's own field for this type — "Forward to", not "Primary
  // servers": posting `primaries` here is refused outright ("primaries
  // applies to secondary and stub zones only"), so a shared field would fail
  // every time.
  await page.getByLabel("Forward to").fill("10.0.0.1, 10.0.0.2:5353");
  await page.getByRole("button", { name: "Add", exact: true }).click();

  const forwarderRow = page.locator('[data-testid="zone-row"]', { hasText: "corp.example" });
  await expect(forwarderRow).toBeVisible();

  await page.getByRole("link", { name: "corp.example", exact: true }).click();
  // Canonical, not as typed: the port is explicit on both entries because
  // the server wrote it back that way.
  await expect(page.getByText("10.0.0.1:53, 10.0.0.2:5353")).toBeVisible();
  await expect(page.getByText("2 upstreams")).toBeVisible();
  // The line that stops a page with a header, one row and two buttons
  // reading as one that failed to load.
  await expect(page.getByText(/queries for it get SERVFAIL/)).toBeVisible();
  // Nothing that assumes authored data: no SOA form, no records grid, and no
  // Export (there is nothing under the apex to render into a file). Refresh
  // is absent because POST /zones/{id}/refresh answers a forwarder 400 —
  // there is no master to pull from.
  await expect(page.getByRole("button", { name: "SOA", exact: true })).toHaveCount(0);
  await expect(page.getByTestId("zone-record-list")).toHaveCount(0);
  await expect(page.getByRole("button", { name: "Export" })).toHaveCount(0);
  await expect(page.getByRole("button", { name: "Refresh now" })).toHaveCount(0);

  // Edit them through the real PATCH handler, then reload: read back from
  // the server rather than from this tab, and in read mode (the default on a
  // fresh load) rather than the input the pencil would have to open first.
  // A page that had only remembered the response would come back showing the
  // old pair.
  await page.getByRole("button", { name: "Edit upstreams" }).click();
  await page.getByLabel("Forward to", { exact: true }).fill("10.0.0.7:5353");
  await page.getByRole("button", { name: "Save", exact: true }).click();
  await expect(page.getByText("1 upstream")).toBeVisible();

  await page.reload();
  await expect(page.getByText("10.0.0.7:5353")).toBeVisible();
  await expect(page.getByText("1 upstream")).toBeVisible();

  // And the key it names cannot now be deleted out from under it.
  await systemMenu.click();
  await page.getByRole("menuitem", { name: "TSIG keys" }).click();
  await expect(keyRow.getByText("1 zone", { exact: true })).toBeVisible();
  await keyRow.getByRole("button", { name: "Delete xfer.e412.in." }).click();
  await expect(page.getByText("In use by 1 zone. Remove it from them first.")).toBeVisible();
  await expect(page.getByRole("button", { name: "Delete", exact: true })).toBeDisabled();
});
