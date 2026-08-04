import { http, HttpResponse } from "msw";
import { toast } from "sonner";
import { expect, test, vi } from "vitest";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { server } from "../../test/msw-server";
import { renderWithProviders } from "../../test/render";
import type { List } from "../../api/types";
import { ListsTab } from "./lists";

function list(overrides: Partial<List> = {}): List {
  return {
    id: 1,
    url: "https://example.com/hosts",
    kind: "block",
    enabled: true,
    last_refreshed: Date.now() - 15 * 60 * 1000,
    entry_count: 85_000,
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
  await user.click(within(dialog).getByRole("combobox", { name: /list kind/i }));
  await user.click(await screen.findByRole("option", { name: /^allow/i }));
  await user.click(within(dialog).getByRole("button", { name: /^add list$/i }));

  await waitFor(() =>
    expect(requestBody).toEqual({ url: "https://newlist.example.com/hosts", kind: "allow" }),
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
  const toggle = await screen.findByRole("switch", {
    name: /disable https:\/\/example\.com\/hosts/i,
  });

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

  await user.click(screen.getByRole("button", { name: /refresh now/i }));

  await waitFor(() => expect(refreshed).toBe(true));
  await waitFor(() => expect(successSpy).toHaveBeenCalledWith(expect.stringMatching(/refresh/i)));
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

  await user.click(screen.getByRole("button", { name: /delete https:\/\/example\.com\/hosts/i }));
  const confirmDialog = await screen.findByRole("alertdialog");
  await user.click(within(confirmDialog).getByRole("button", { name: /^delete$/i }));

  await waitFor(() => expect(deleted).toBe(true));
  await waitFor(() => expect(screen.queryByRole("alertdialog")).not.toBeInTheDocument());
});

test("an empty list shows EmptyState with an Add list action", async () => {
  server.use(http.get("/api/v1/filters/lists", () => HttpResponse.json([])));

  renderWithProviders(<ListsTab />);

  expect(await screen.findByText("No filter lists yet")).toBeInTheDocument();
  // Only one "Add list" affordance when empty — the EmptyState's own
  // action — not a redundant second button in the header above it.
  expect(screen.getAllByRole("button", { name: /^add list$/i })).toHaveLength(1);
});
