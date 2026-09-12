import { http, HttpResponse } from "msw";
import { toast } from "sonner";
import { expect, test, vi } from "vitest";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { server } from "../../test/msw-server";
import { replicaHandlers } from "../../test/msw-handlers";
import { renderWithProviders } from "../../test/render";
import type { List } from "../../api/types";
import { ListsTab } from "./lists";

function list(overrides: Partial<List> = {}): List {
  return {
    id: 1,
    url: "https://example.com/hosts",
    name: "example.com hosts",
    kind: "block",
    enabled: true,
    last_refreshed: Date.now() - 15 * 60 * 1000,
    entry_count: 85_000,
    last_status: "ok",
    last_error: "",
    last_attempt: Date.now() - 15 * 60 * 1000,
    ...overrides,
  };
}

test("adding a list posts /filters/lists, the table refetches, and the new row appears", async () => {
  const user = userEvent.setup();
  let requestBody: unknown;
  let lists = [list()];
  server.use(
    http.get("/api/v1/filters/lists", () => HttpResponse.json(lists)),
    http.post("/api/v1/filters/lists", async ({ request }) => {
      requestBody = await request.json();
      const created = list({ id: 2, url: "https://newlist.example.com/hosts", kind: "allow" });
      lists = [...lists, created];
      return HttpResponse.json({ id: created.id }, { status: 201 });
    }),
  );

  renderWithProviders(<ListsTab />);
  await screen.findByText("https://example.com/hosts");

  await user.click(screen.getByRole("button", { name: /^add list$/i }));
  const dialog = await screen.findByRole("dialog");

  await user.type(within(dialog).getByLabelText(/^url$/i), "https://newlist.example.com/hosts");
  await user.selectOptions(within(dialog).getByRole("combobox", { name: /^kind$/i }), "allow");
  await user.click(within(dialog).getByRole("button", { name: /^add list$/i }));

  await waitFor(() =>
    expect(requestBody).toEqual({
      url: "https://newlist.example.com/hosts",
      kind: "allow",
      name: "",
    }),
  );
  expect(await screen.findByText("https://newlist.example.com/hosts")).toBeInTheDocument();
  // Dialog closes on success.
  expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
});

test("toggling a list's enabled switch PATCHes /filters/lists/{id}", async () => {
  const user = userEvent.setup();
  let requestBody: unknown;
  server.use(
    http.get("/api/v1/filters/lists", () => HttpResponse.json([list({ enabled: true })])),
    http.patch("/api/v1/filters/lists/1", async ({ request }) => {
      requestBody = await request.json();
      return new HttpResponse(null, { status: 204 });
    }),
  );

  renderWithProviders(<ListsTab />);
  const toggle = await screen.findByRole("switch", { name: /disable example\.com hosts/i });

  await user.click(toggle);

  await waitFor(() => expect(requestBody).toEqual({ enabled: false }));
});

test("an invalid URL shows inline validation and never posts", async () => {
  const user = userEvent.setup();
  let posted = false;
  server.use(
    http.get("/api/v1/filters/lists", () => HttpResponse.json([list()])),
    http.post("/api/v1/filters/lists", () => {
      posted = true;
      return HttpResponse.json({ id: 99 }, { status: 201 });
    }),
  );

  renderWithProviders(<ListsTab />);
  await screen.findByText("https://example.com/hosts");

  await user.click(screen.getByRole("button", { name: /^add list$/i }));
  const dialog = await screen.findByRole("dialog");

  await user.type(within(dialog).getByLabelText(/^url$/i), "not-a-url");
  await user.click(within(dialog).getByRole("button", { name: /^add list$/i }));

  expect(await within(dialog).findByText(/enter a valid url/i)).toBeInTheDocument();
  // Give any accidental async POST a chance to land before asserting it didn't.
  await new Promise((resolve) => setTimeout(resolve, 30));
  expect(posted).toBe(false);
  expect(screen.getByRole("dialog")).toBeInTheDocument();
});

test("a non-http(s) URL (ftp) shows inline validation and never posts", async () => {
  const user = userEvent.setup();
  let posted = false;
  server.use(
    http.get("/api/v1/filters/lists", () => HttpResponse.json([list()])),
    http.post("/api/v1/filters/lists", () => {
      posted = true;
      return HttpResponse.json({ id: 99 }, { status: 201 });
    }),
  );

  renderWithProviders(<ListsTab />);
  await screen.findByText("https://example.com/hosts");

  await user.click(screen.getByRole("button", { name: /^add list$/i }));
  const dialog = await screen.findByRole("dialog");

  await user.type(within(dialog).getByLabelText(/^url$/i), "ftp://example.com/hosts");
  await user.click(within(dialog).getByRole("button", { name: /^add list$/i }));

  expect(await within(dialog).findByText(/must start with http/i)).toBeInTheDocument();
  await new Promise((resolve) => setTimeout(resolve, 30));
  expect(posted).toBe(false);
});

test("refresh now posts /filters/refresh and shows a toast once it's accepted", async () => {
  const user = userEvent.setup();
  let refreshed = false;
  server.use(
    http.get("/api/v1/filters/lists", () => HttpResponse.json([list()])),
    http.post("/api/v1/filters/refresh", () => {
      refreshed = true;
      return HttpResponse.json({ status: "refreshing" }, { status: 202 });
    }),
  );
  const successSpy = vi.spyOn(toast, "success");

  renderWithProviders(<ListsTab />);
  await screen.findByText("https://example.com/hosts");

  await user.click(screen.getByRole("button", { name: /refresh all/i }));

  await waitFor(() => expect(refreshed).toBe(true));
  await waitFor(() => expect(successSpy).toHaveBeenCalledWith(expect.stringMatching(/refresh/i)));
});

// The 202 means "scheduled", not "done" — internal/filter/refresh.go writes
// each list's new last_refreshed/entry_count as its download finishes. The
// mutation used to invalidate nothing, on the theory that "the lists table
// catches up on its own next poll/interaction"; there is no such poll
// (useLists has no refetchInterval, and refetchOnWindowFocus is off), so
// the row kept reading "refreshed 3d ago" straight after a successful
// refresh until the tab remounted.
test("refresh now re-reads the lists table once the server has had a moment", async () => {
  const user = userEvent.setup();
  let refreshedAt = 0;
  server.use(
    http.get("/api/v1/filters/lists", () =>
      HttpResponse.json([
        list({ last_refreshed: refreshedAt || Date.now() - 3 * 24 * 60 * 60 * 1000 }),
      ]),
    ),
    http.post("/api/v1/filters/refresh", () => {
      refreshedAt = Date.now();
      return HttpResponse.json({ status: "refreshing" }, { status: 202 });
    }),
  );

  renderWithProviders(<ListsTab />);
  // One match now: the row itself. The old header summary line is gone —
  // the design moved that readout into the chrome's sub-tab bar.
  expect(await screen.findAllByText(/3d ago/i)).toHaveLength(1);

  await user.click(screen.getByRole("button", { name: /refresh all/i }));

  // The first delayed re-read lands about a second later.
  await waitFor(() => expect(screen.getAllByText(/just now/i)).toHaveLength(1), { timeout: 4000 });
  expect(screen.queryAllByText(/3d ago/i)).toHaveLength(0);
}, 10_000);

// The row's two temporal facts, together: how old the copy on disk is, and
// how long it has left. `last_refreshed` alone answers only the first, which
// leaves "is this about to fix itself?" unanswerable from the table.
test("a row says when the list was last checked and when it refreshes next", async () => {
  server.use(
    http.get("/api/v1/filters/lists", () =>
      HttpResponse.json([
        list({
          last_attempt: Date.now() - 3 * 60 * 60 * 1000,
          // A half-minute of slack so the render's own clock read cannot
          // round this down to "20h 59m".
          next_refresh_at: Date.now() + 21 * 60 * 60 * 1000 + 30_000,
        }),
      ]),
    ),
  );

  renderWithProviders(<ListsTab />);

  expect(await screen.findByText("Checked 3h ago · next in 21h")).toBeInTheDocument();
});

// A server with no cadence running (lists.refresh_hours is not positive, or
// it has only just started) sends 0, and there is no next time to promise.
test("a row with no scheduled refresh says only when it was checked", async () => {
  server.use(
    http.get("/api/v1/filters/lists", () =>
      HttpResponse.json([
        list({ last_attempt: Date.now() - 3 * 60 * 60 * 1000, next_refresh_at: 0 }),
      ]),
    ),
  );

  renderWithProviders(<ListsTab />);

  expect(await screen.findByText("Checked 3h ago")).toBeInTheDocument();
});

// Unlike the all-lists 202, this one has already done the work when it
// answers — so the table reads the new state immediately rather than on the
// 1s/4s/12s ladder.
test("refreshing one list posts /filters/lists/{id}/refresh and re-reads the table", async () => {
  const user = userEvent.setup();
  let refreshedId = 0;
  let entries = 85_000;
  server.use(
    http.get("/api/v1/filters/lists", () => HttpResponse.json([list({ entry_count: entries })])),
    http.post("/api/v1/filters/lists/1/refresh", () => {
      refreshedId = 1;
      entries = 99_277;
      return HttpResponse.json(list({ entry_count: entries }), { status: 202 });
    }),
  );
  const successSpy = vi.spyOn(toast, "success");

  renderWithProviders(<ListsTab />);
  await screen.findByText("85,000");

  await user.click(screen.getByRole("button", { name: /^refresh example\.com hosts$/i }));

  await waitFor(() => expect(refreshedId).toBe(1));
  expect(await screen.findByText("99,277")).toBeInTheDocument();
  expect(successSpy).toHaveBeenCalledWith(expect.stringMatching(/example\.com hosts/i));
});

test("a failed per-list refresh names the list and leaves the row alone", async () => {
  const user = userEvent.setup();
  server.use(
    http.get("/api/v1/filters/lists", () => HttpResponse.json([list()])),
    http.post("/api/v1/filters/lists/1/refresh", () =>
      HttpResponse.json({ error: "this list is disabled" }, { status: 409 }),
    ),
  );
  const errorSpy = vi.spyOn(toast, "error");

  renderWithProviders(<ListsTab />);
  await screen.findByText("https://example.com/hosts");

  await user.click(screen.getByRole("button", { name: /^refresh example\.com hosts$/i }));

  await waitFor(() => expect(errorSpy).toHaveBeenCalledWith("Couldn't refresh example.com hosts"));
});

test("deleting a list asks for confirmation, then DELETEs /filters/lists/{id}", async () => {
  const user = userEvent.setup();
  let deleted = false;
  server.use(
    http.get("/api/v1/filters/lists", () => HttpResponse.json([list()])),
    http.delete("/api/v1/filters/lists/1", () => {
      deleted = true;
      return new HttpResponse(null, { status: 204 });
    }),
  );

  renderWithProviders(<ListsTab />);
  await screen.findByText("https://example.com/hosts");

  await user.click(screen.getByRole("button", { name: /delete example\.com hosts/i }));
  const confirmDialog = await screen.findByRole("alertdialog");
  await user.click(within(confirmDialog).getByRole("button", { name: /^delete$/i }));

  await waitFor(() => expect(deleted).toBe(true));
  await waitFor(() => expect(screen.queryByRole("alertdialog")).not.toBeInTheDocument());
});

test("an empty list states it, with the toolbar's Add still there", async () => {
  server.use(http.get("/api/v1/filters/lists", () => HttpResponse.json([])));

  renderWithProviders(<ListsTab />);

  // The string ui-contract §7 documents, verbatim.
  expect(await screen.findByText("No filter lists yet.")).toBeInTheDocument();
  // Add lives in the toolbar at all times now, so there is exactly one of
  // it — an empty state with its own duplicate button would be two.
  expect(screen.getAllByRole("button", { name: /^add list$/i })).toHaveLength(1);
});

// The reported bug. `0 entries / never refreshed` is exactly what a list that
// simply hasn't run yet looks like, so a 404 was indistinguishable from a
// brand-new subscription — and a list contributing zero entries silently
// blocks nothing. The row has to name the failure, and the page has to say so
// without the admin scanning every row.
test("a failed list names the fetch error on the row and interrupts the page", async () => {
  server.use(
    http.get("/api/v1/filters/lists", () =>
      HttpResponse.json([
        list({
          url: "https://raw.githubusercontent.com/hagezi/dns-blocklists/main/hosts/pro.txt",
          entry_count: 0,
          last_refreshed: 0,
          last_status: "failed",
          last_error: "404 Not Found",
          last_attempt: Date.now() - 2 * 60 * 1000,
        }),
      ]),
    ),
  );

  renderWithProviders(<ListsTab />);

  expect(await screen.findByText("Failed")).toBeInTheDocument();
  // The reason, on the row and in the page-level alert.
  expect(screen.getAllByText(/404 Not Found/).length).toBeGreaterThanOrEqual(2);
  // Both the row detail and the alert title say it enforces nothing.
  expect(screen.getAllByText(/blocking nothing/i).length).toBeGreaterThanOrEqual(2);
  // "never" must not stand in for a failure any more — the pending copy is
  // reserved for a list that genuinely hasn't been tried.
  expect(screen.queryByText(/added but not yet fetched/i)).not.toBeInTheDocument();
  expect(screen.queryByText("Pending")).not.toBeInTheDocument();
});

// The distinction that makes "Failed" mean something: the same 404 on a list
// that already has a cached copy is not "blocking nothing" — it is still
// enforcing, just off an older file. Calling that Failed would train the
// admin to ignore the loud state.
test("a stale list says it is serving an older copy, not that it failed", async () => {
  server.use(
    http.get("/api/v1/filters/lists", () =>
      HttpResponse.json([
        list({
          entry_count: 99_277,
          last_refreshed: Date.now() - 3 * 24 * 60 * 60 * 1000,
          last_status: "stale",
          last_error: "404 Not Found",
          last_attempt: Date.now() - 5 * 60 * 1000,
        }),
      ]),
    ),
  );

  renderWithProviders(<ListsTab />);

  expect(await screen.findByText("Stale")).toBeInTheDocument();
  expect(screen.getByText(/still enforcing the copy from 3d ago/i)).toBeInTheDocument();
  expect(screen.queryByText("Failed")).not.toBeInTheDocument();
  expect(screen.queryByText(/blocking nothing/i)).not.toBeInTheDocument();
  // Its entries are real, so the count still reads.
  expect(screen.getByText("99,277")).toBeInTheDocument();
});

// The fourth outcome, and the one where every number looks healthy: the
// download worked, the parse returned no error, and the list still enforces
// nothing. "0 entries, refreshed just now" is what sent the owner hunting for
// a download bug that wasn't there, so the parser's side has to be named.
test("a list that fetched but parsed to nothing says so, not '0 entries'", async () => {
  server.use(
    http.get("/api/v1/filters/lists", () =>
      HttpResponse.json([
        list({
          entry_count: 0,
          last_refreshed: Date.now() - 60 * 1000,
          last_status: "empty",
          last_error: "fetched 4.5 MB, no usable entries — 250,431 lines skipped",
          last_attempt: Date.now() - 60 * 1000,
        }),
      ]),
    ),
  );

  renderWithProviders(<ListsTab />);

  expect(await screen.findByText("No entries")).toBeInTheDocument();
  expect(screen.getAllByText(/250,431 lines skipped/).length).toBeGreaterThanOrEqual(2);
  expect(screen.getAllByText(/blocking nothing/i).length).toBeGreaterThanOrEqual(2);
});

// "never" keeps its one honest meaning. A freshly added list is genuinely
// pending until the detached refresh lands, and must not be dressed up as
// either a success or a failure.
test("a never-attempted list reads pending, with no error and no alert", async () => {
  server.use(
    http.get("/api/v1/filters/lists", () =>
      HttpResponse.json([
        list({ entry_count: 0, last_refreshed: 0, last_status: "pending", last_attempt: 0 }),
      ]),
    ),
  );

  renderWithProviders(<ListsTab />);

  expect(await screen.findByText("Pending")).toBeInTheDocument();
  expect(screen.getByText(/added but not yet fetched/i)).toBeInTheDocument();
  expect(screen.queryByText(/blocking nothing/i)).not.toBeInTheDocument();
  expect(screen.queryByRole("alert")).not.toBeInTheDocument();
});

// A healthy list stays quiet — otherwise the loud treatment above is noise.
test("a healthy list reads OK with no alert", async () => {
  server.use(http.get("/api/v1/filters/lists", () => HttpResponse.json([list()])));

  renderWithProviders(<ListsTab />);

  expect(await screen.findByText("OK")).toBeInTheDocument();
  // One match: the row. The header summary that used to repeat it is gone.
  expect(screen.getAllByText(/refreshed 15m ago/i).length).toBe(1);
  expect(screen.queryByRole("alert")).not.toBeInTheDocument();
});

// The point of naming: the URL stops being the identifier. It stays visible
// underneath (a derived name has to be checkable against its source) but the
// name is what the row leads with and what every action refers to.
test("the row leads with the name and keeps the URL as a secondary line", async () => {
  server.use(
    http.get("/api/v1/filters/lists", () =>
      HttpResponse.json([
        list({
          name: "Household baseline",
          url: "https://raw.githubusercontent.com/StevenBlack/hosts/master/hosts",
        }),
      ]),
    ),
  );

  renderWithProviders(<ListsTab />);

  expect(await screen.findByText("Household baseline")).toBeInTheDocument();
  expect(
    screen.getByText("https://raw.githubusercontent.com/StevenBlack/hosts/master/hosts"),
  ).toBeInTheDocument();
  // Actions are addressed by name, not by URL.
  expect(screen.getByRole("switch", { name: /disable household baseline/i })).toBeInTheDocument();
  expect(screen.getByRole("button", { name: /delete household baseline/i })).toBeInTheDocument();
  expect(screen.getByRole("button", { name: /rename household baseline/i })).toBeInTheDocument();
});

// Optional on input. The placeholder has to show the default that blank would
// actually produce, or "leave it blank" is a guess.
test("the add dialog previews the derived name and posts an explicit one", async () => {
  const user = userEvent.setup();
  let requestBody: unknown;
  server.use(
    http.get("/api/v1/filters/lists", () => HttpResponse.json([list()])),
    http.post("/api/v1/filters/lists", async ({ request }) => {
      requestBody = await request.json();
      return HttpResponse.json({ id: 2 }, { status: 201 });
    }),
  );

  renderWithProviders(<ListsTab />);
  await screen.findByText("example.com hosts");

  await user.click(screen.getByRole("button", { name: /^add list$/i }));
  const dialog = await screen.findByRole("dialog");

  await user.type(
    within(dialog).getByLabelText(/^url$/i),
    "https://raw.githubusercontent.com/hagezi/dns-blocklists/main/wildcard/pro.txt",
  );
  // The preview matches what the server's own derivation would store.
  const nameField = within(dialog).getByLabelText(/^name$/i);
  await waitFor(() => expect(nameField).toHaveAttribute("placeholder", "hagezi wildcard/pro.txt"));

  await user.type(nameField, "Aggressive blocklist");
  await user.click(within(dialog).getByRole("button", { name: /^add list$/i }));

  await waitFor(() =>
    expect(requestBody).toEqual({
      url: "https://raw.githubusercontent.com/hagezi/dns-blocklists/main/wildcard/pro.txt",
      kind: "block",
      name: "Aggressive blocklist",
    }),
  );
});

// Renaming, and the blank-means-default rule the server implements.
test("renaming PATCHes the name, and blank resets to the derived default", async () => {
  const user = userEvent.setup();
  const bodies: unknown[] = [];
  server.use(
    http.get("/api/v1/filters/lists", () => HttpResponse.json([list({ name: "Old name" })])),
    http.patch("/api/v1/filters/lists/1", async ({ request }) => {
      bodies.push(await request.json());
      return new HttpResponse(null, { status: 204 });
    }),
  );

  renderWithProviders(<ListsTab />);

  // Submitting the empty field is legitimate — the server reads blank as
  // "go back to the URL-derived default" — so it must not be blocked by
  // client-side validation.
  await user.click(await screen.findByRole("button", { name: /rename old name/i }));
  let dialog = await screen.findByRole("dialog");
  expect(within(dialog).getByText(/blank goes back to example\.com hosts/i)).toBeInTheDocument();
  await user.click(within(dialog).getByRole("button", { name: /^save$/i }));
  await waitFor(() => expect(bodies).toEqual([{ name: "" }]));

  await user.click(await screen.findByRole("button", { name: /rename old name/i }));
  dialog = await screen.findByRole("dialog");
  await user.type(within(dialog).getByLabelText(/^name$/i), "Kids blocklist");
  await user.click(within(dialog).getByRole("button", { name: /^save$/i }));

  await waitFor(() => expect(bodies).toEqual([{ name: "" }, { name: "Kids blocklist" }]));
});

// The dialog is mounted unconditionally, so `useForm` captured its defaults
// once and never again: text typed for one list survived Cancel and was
// still sitting there the next time the dialog opened — on a different list,
// under a placeholder naming that other list's current name.
test("a cancelled rename leaves nothing behind for the next list", async () => {
  const user = userEvent.setup();
  server.use(
    http.get("/api/v1/filters/lists", () =>
      HttpResponse.json([
        list({ id: 1, name: "First list", url: "https://example.com/one" }),
        list({ id: 2, name: "Second list", url: "https://example.com/two" }),
      ]),
    ),
  );

  renderWithProviders(<ListsTab />);

  await user.click(await screen.findByRole("button", { name: /rename first list/i }));
  let dialog = await screen.findByRole("dialog");
  await user.type(within(dialog).getByLabelText(/^name$/i), "half-typed");
  await user.click(within(dialog).getByRole("button", { name: /^cancel$/i }));
  await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());

  await user.click(screen.getByRole("button", { name: /rename second list/i }));
  dialog = await screen.findByRole("dialog");
  // Blank, because blank is what "keep the current name" looks like here —
  // and never the other list's half-typed replacement.
  expect(within(dialog).getByLabelText(/^name$/i)).toHaveValue("");
  expect(within(dialog).getByLabelText(/^name$/i)).toHaveAttribute("placeholder", "Second list");
});

// The grid is a fixed-pixel column template inside a shell that is
// h-screen/overflow-hidden (components/app-shell.tsx), so on a phone the
// Status and Actions columns were clipped with no way to reach them.
test("the fixed-width grid sits in a horizontal scroll container", async () => {
  renderWithProviders(<ListsTab />);
  await screen.findByText("example.com hosts");

  const scroller = document.querySelector('[data-slot="h-scroll"]');
  expect(scroller).not.toBeNull();
  expect(scroller!.className).toContain("overflow-x-auto");
  expect(scroller!.contains(screen.getByText("example.com hosts"))).toBe(true);
});

// --- replica mode ---------------------------------------------------------

// Lists are synced (spec §4.2) but a refresh is not: re-fetching a list's
// contents is an operational act on this box's own copy, which §7 keeps
// allowed on a replica precisely so a stale copy can be repaired locally.
test("a replica says who manages it, stops the list writes and keeps refresh", async () => {
  server.use(http.get("/api/v1/filters/lists", () => HttpResponse.json([list()])));
  server.use(...replicaHandlers("https://main.lan"));

  renderWithProviders(<ListsTab />);

  expect(await screen.findByText("Managed by https://main.lan")).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Add list" })).toBeDisabled();
  expect(screen.getByRole("button", { name: /^Delete /, hidden: true })).toBeDisabled();
  // rnui's Switch is a base-ui <span role="switch">, not a native control,
  // so its disabled state is the ARIA one.
  expect(screen.getByRole("switch", { name: /^Disable / })).toHaveAttribute(
    "aria-disabled",
    "true",
  );
  expect(screen.getByRole("button", { name: /^Refresh example/ })).toBeEnabled();
  expect(screen.getByRole("button", { name: /Refresh all/ })).toBeEnabled();
});
