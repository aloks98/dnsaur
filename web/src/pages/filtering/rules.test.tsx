import { http, HttpResponse } from "msw";
import { expect, test } from "vitest";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { server } from "../../test/msw-server";
import { renderWithProviders } from "../../test/render";
import type { Group, Rule } from "../../api/types";
import { RulesTab } from "./rules";

function group(overrides: Partial<Group> = {}): Group {
  return { id: 1, name: "Default", enabled: true, ...overrides };
}

function rule(overrides: Partial<Rule> = {}): Rule {
  return {
    id: 1,
    group_id: 1,
    action: "block",
    pattern: "ads.example.com",
    is_regex: false,
    ...overrides,
  };
}

function mockGroups(groups: Group[]) {
  server.use(http.get("/api/v1/groups", () => HttpResponse.json(groups)));
}

function mockRulesByGroup(rulesByGroup: Record<number, Rule[]>) {
  server.use(
    http.get("/api/v1/groups/:id/rules", ({ params }) => {
      const id = Number(params.id);
      return HttpResponse.json(rulesByGroup[id] ?? []);
    }),
  );
}

test("loads the default group's rules and shows action + pattern + match", async () => {
  mockGroups([group()]);
  mockRulesByGroup({
    1: [rule({ id: 1, action: "block", pattern: "ads.example.com", is_regex: false })],
  });

  renderWithProviders(<RulesTab />);

  expect(await screen.findByText("ads.example.com")).toBeInTheDocument();
  expect(screen.getByText("Block")).toBeInTheDocument();
  expect(screen.getByText("Exact")).toBeInTheDocument();
});

test("switching the group Select loads that group's rules", async () => {
  const user = userEvent.setup();
  mockGroups([group({ id: 1, name: "Default" }), group({ id: 2, name: "Kids" })]);
  mockRulesByGroup({
    1: [rule({ id: 1, group_id: 1, pattern: "ads.example.com" })],
    2: [
      rule({ id: 2, group_id: 2, action: "allow", pattern: "school.example.com", is_regex: true }),
    ],
  });

  renderWithProviders(<RulesTab />);
  await screen.findByText("ads.example.com");

  await user.click(screen.getByRole("combobox", { name: /group/i }));
  await user.click(await screen.findByRole("option", { name: /^kids$/i }));

  expect(await screen.findByText("school.example.com")).toBeInTheDocument();
  expect(screen.getByText("Allow")).toBeInTheDocument();
  expect(screen.getByText("Regex")).toBeInTheDocument();
  expect(screen.queryByText("ads.example.com")).not.toBeInTheDocument();
});

test("adding a rule posts /groups/{id}/rules with the trimmed pattern and refetches", async () => {
  const user = userEvent.setup();
  let requestBody: unknown;
  let rules = [rule()];
  mockGroups([group()]);
  server.use(
    http.get("/api/v1/groups/:id/rules", () => HttpResponse.json(rules)),
    http.post("/api/v1/groups/:id/rules", async ({ request }) => {
      requestBody = await request.json();
      const created = rule({
        id: 2,
        action: "allow",
        pattern: "trusted.example.com",
        is_regex: false,
      });
      rules = [...rules, created];
      return HttpResponse.json({ id: created.id }, { status: 201 });
    }),
  );

  renderWithProviders(<RulesTab />);
  await screen.findByText("ads.example.com");

  await user.click(screen.getByRole("button", { name: /^add rule$/i }));
  const dialog = await screen.findByRole("dialog");

  await user.click(within(dialog).getByRole("combobox", { name: /^action$/i }));
  await user.click(await screen.findByRole("option", { name: /^allow$/i }));
  await user.type(within(dialog).getByLabelText(/^pattern$/i), "  trusted.example.com  ");
  await user.click(within(dialog).getByRole("button", { name: /^add rule$/i }));

  await waitFor(() =>
    expect(requestBody).toEqual({
      action: "allow",
      pattern: "trusted.example.com",
      is_regex: false,
    }),
  );
  expect(await screen.findByText("trusted.example.com")).toBeInTheDocument();
  expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
});

test("an invalid regex pattern shows inline error and never posts", async () => {
  const user = userEvent.setup();
  let posted = false;
  mockGroups([group()]);
  server.use(
    http.get("/api/v1/groups/:id/rules", () => HttpResponse.json([rule()])),
    http.post("/api/v1/groups/:id/rules", () => {
      posted = true;
      return HttpResponse.json({ id: 99 }, { status: 201 });
    }),
  );

  renderWithProviders(<RulesTab />);
  await screen.findByText("ads.example.com");

  await user.click(screen.getByRole("button", { name: /^add rule$/i }));
  const dialog = await screen.findByRole("dialog");

  await user.click(within(dialog).getByRole("switch", { name: /^regular expression$/i }));
  await user.type(within(dialog).getByLabelText(/^pattern$/i), "ads(.*");
  await user.click(within(dialog).getByRole("button", { name: /^add rule$/i }));

  expect(await within(dialog).findByText(/invalid regular expression/i)).toBeInTheDocument();
  // Give any accidental async POST a chance to land before asserting it didn't.
  await new Promise((resolve) => setTimeout(resolve, 30));
  expect(posted).toBe(false);
  expect(screen.getByRole("dialog")).toBeInTheDocument();
});

test("a regex pattern over 512 characters shows inline error and never posts", async () => {
  const user = userEvent.setup();
  let posted = false;
  mockGroups([group()]);
  server.use(
    http.get("/api/v1/groups/:id/rules", () => HttpResponse.json([rule()])),
    http.post("/api/v1/groups/:id/rules", () => {
      posted = true;
      return HttpResponse.json({ id: 99 }, { status: 201 });
    }),
  );

  renderWithProviders(<RulesTab />);
  await screen.findByText("ads.example.com");

  await user.click(screen.getByRole("button", { name: /^add rule$/i }));
  const dialog = await screen.findByRole("dialog");

  await user.click(within(dialog).getByRole("switch", { name: /^regular expression$/i }));
  const longPattern = `a${"b".repeat(520)}`;
  await user.click(within(dialog).getByLabelText(/^pattern$/i));
  await user.paste(longPattern);
  await user.click(within(dialog).getByRole("button", { name: /^add rule$/i }));

  expect(await within(dialog).findByText(/512 characters or fewer/i)).toBeInTheDocument();
  await new Promise((resolve) => setTimeout(resolve, 30));
  expect(posted).toBe(false);
});

test("deleting a rule asks for confirmation, then DELETEs /filters/rules/{id}", async () => {
  const user = userEvent.setup();
  let deleted = false;
  mockGroups([group()]);
  server.use(
    http.get("/api/v1/groups/:id/rules", () => HttpResponse.json([rule()])),
    http.delete("/api/v1/filters/rules/1", () => {
      deleted = true;
      return new HttpResponse(null, { status: 204 });
    }),
  );

  renderWithProviders(<RulesTab />);
  await screen.findByText("ads.example.com");

  await user.click(screen.getByRole("button", { name: /delete rule ads\.example\.com/i }));
  const confirmDialog = await screen.findByRole("alertdialog");
  await user.click(within(confirmDialog).getByRole("button", { name: /^delete$/i }));

  await waitFor(() => expect(deleted).toBe(true));
  await waitFor(() => expect(screen.queryByRole("alertdialog")).not.toBeInTheDocument());
});

test("an empty group shows EmptyState with an Add rule action", async () => {
  mockGroups([group()]);
  mockRulesByGroup({ 1: [] });

  renderWithProviders(<RulesTab />);

  expect(await screen.findByText("No rules for this group yet")).toBeInTheDocument();
  expect(screen.getAllByRole("button", { name: /^add rule$/i })).toHaveLength(1);
});
