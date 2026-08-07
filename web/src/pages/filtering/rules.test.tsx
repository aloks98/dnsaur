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

/** Rows are a CSS grid, not a table — scope by the slot the page marks them
 * with, since "block" and "allow" also appear in the filter and form
 * selects above. */
function rows(): HTMLElement[] {
  return Array.from(document.querySelectorAll<HTMLElement>('[data-slot="rule-row"]'));
}

/** The add row is revealed by the toolbar button rather than always present. */
async function openAddRow(user: ReturnType<typeof userEvent.setup>) {
  await user.click(screen.getByRole("button", { name: /^add rule$/i }));
  return screen.findByLabelText(/^pattern$/i);
}

test("loads the default group's rules and shows action, pattern and type", async () => {
  mockGroups([group()]);
  mockRulesByGroup({
    1: [rule({ id: 1, action: "block", pattern: "ads.example.com", is_regex: false })],
  });

  renderWithProviders(<RulesTab />);
  await screen.findByText("ads.example.com");

  const [row] = rows();
  expect(within(row).getByText("block")).toBeInTheDocument();
  expect(within(row).getByText("ads.example.com")).toBeInTheDocument();
  expect(within(row).getByText("literal")).toBeInTheDocument();
});

// A rule belongs to exactly one group — there are no global rules — so the
// group selector changes what the whole page is about, not just what it
// filters.
test("switching the group loads that group's rules", async () => {
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

  await user.selectOptions(screen.getByLabelText(/^group$/i), "2");

  expect(await screen.findByText("school.example.com")).toBeInTheDocument();
  const [row] = rows();
  expect(within(row).getByText("allow")).toBeInTheDocument();
  expect(within(row).getByText("regex")).toBeInTheDocument();
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
      const created = rule({ id: 2, action: "allow", pattern: "trusted.example.com" });
      rules = [...rules, created];
      return HttpResponse.json({ id: created.id }, { status: 201 });
    }),
  );

  renderWithProviders(<RulesTab />);
  await screen.findByText("ads.example.com");

  const pattern = await openAddRow(user);
  await user.selectOptions(screen.getByLabelText(/rule action/i), "allow");
  await user.type(pattern, "  trusted.example.com  ");
  await user.click(screen.getByRole("button", { name: /^add$/i }));

  await waitFor(() =>
    expect(requestBody).toEqual({
      action: "allow",
      pattern: "trusted.example.com",
      is_regex: false,
    }),
  );
  expect(await screen.findByText("trusted.example.com")).toBeInTheDocument();
});

// Adding one rule is usually adding several, and the action and regex mode
// are normally the same across them — so only the pattern clears.
test("a successful add clears the pattern but keeps action and regex mode", async () => {
  const user = userEvent.setup();
  mockGroups([group()]);
  server.use(
    http.get("/api/v1/groups/:id/rules", () => HttpResponse.json([rule()])),
    http.post("/api/v1/groups/:id/rules", () => HttpResponse.json({ id: 2 }, { status: 201 })),
  );

  renderWithProviders(<RulesTab />);
  await screen.findByText("ads.example.com");

  const pattern = await openAddRow(user);
  await user.selectOptions(screen.getByLabelText(/rule action/i), "allow");
  await user.click(screen.getByRole("switch", { name: /regular expression/i }));
  await user.type(pattern, "^trusted");
  await user.click(screen.getByRole("button", { name: /^add$/i }));

  await waitFor(() => expect(screen.getByLabelText(/^pattern$/i)).toHaveValue(""));
  expect(screen.getByLabelText(/rule action/i)).toHaveValue("allow");
  expect(screen.getByRole("switch", { name: /regular expression/i })).toBeChecked();
});

test("an invalid regex pattern shows an inline error and never posts", async () => {
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

  const pattern = await openAddRow(user);
  await user.click(screen.getByRole("switch", { name: /regular expression/i }));
  await user.type(pattern, "ads(.*");
  await user.click(screen.getByRole("button", { name: /^add$/i }));

  expect(await screen.findByText(/invalid regular expression/i)).toBeInTheDocument();
  // Give any accidental async POST a chance to land before asserting it didn't.
  await new Promise((resolve) => setTimeout(resolve, 30));
  expect(posted).toBe(false);
});

test("a regex pattern over 512 characters shows an inline error and never posts", async () => {
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

  const pattern = await openAddRow(user);
  await user.click(screen.getByRole("switch", { name: /regular expression/i }));
  await user.click(pattern);
  await user.paste(`a${"b".repeat(520)}`);
  await user.click(screen.getByRole("button", { name: /^add$/i }));

  expect(await screen.findByText(/512 characters or fewer/i)).toBeInTheDocument();
  await new Promise((resolve) => setTimeout(resolve, 30));
  expect(posted).toBe(false);
});

// The pattern's rules change with the switch, so an error already on screen
// has to be re-judged against the new mode rather than waiting for another
// submit — `ads(.*` is invalid as a regex and perfectly fine as a literal.
test("turning the regex switch off clears an error the literal mode accepts", async () => {
  const user = userEvent.setup();
  mockGroups([group()]);
  server.use(http.get("/api/v1/groups/:id/rules", () => HttpResponse.json([rule()])));

  renderWithProviders(<RulesTab />);
  await screen.findByText("ads.example.com");

  const pattern = await openAddRow(user);
  await user.click(screen.getByRole("switch", { name: /regular expression/i }));
  await user.type(pattern, "ads(.*");
  await user.click(screen.getByRole("button", { name: /^add$/i }));
  expect(await screen.findByText(/invalid regular expression/i)).toBeInTheDocument();

  await user.click(screen.getByRole("switch", { name: /regular expression/i }));

  await waitFor(() =>
    expect(screen.queryByText(/invalid regular expression/i)).not.toBeInTheDocument(),
  );
});

// First match wins and the order is not guessable: allow beats block,
// literal beats regex, and anything here beats a list on the Lists tab.
// On demand rather than always on screen — you need it once, then know it.
test("the match-order popover states the order, and that rules outrank lists", async () => {
  const user = userEvent.setup();
  mockGroups([group()]);
  mockRulesByGroup({ 1: [rule()] });

  renderWithProviders(<RulesTab />);
  await screen.findByText("ads.example.com");

  // Not on the page until asked for.
  expect(screen.queryByText(/first match wins/i)).not.toBeInTheDocument();

  await user.click(screen.getByRole("button", { name: /match order/i }));

  expect(await screen.findByText(/first match wins/i)).toBeInTheDocument();
  const stages = [
    "literal allow",
    "regex allow",
    "literal block",
    "regex block",
    "allowlists",
    "blocklists",
  ];
  for (const stage of stages) {
    expect(screen.getByText(stage)).toBeInTheDocument();
  }
  // The order is the claim, so assert the sequence rather than presence.
  const listed = screen.getAllByRole("listitem").map((li) => li.textContent);
  expect(listed).toEqual(stages.map((s, i) => `${i + 1}.${s}`));
});

test("the action filter narrows the rows, and can be cleared", async () => {
  const user = userEvent.setup();
  mockGroups([group()]);
  mockRulesByGroup({
    1: [
      rule({ id: 1, action: "block", pattern: "ads.example.com" }),
      rule({ id: 2, action: "allow", pattern: "good.example.com" }),
    ],
  });

  renderWithProviders(<RulesTab />);
  await screen.findByText("ads.example.com");
  expect(rows()).toHaveLength(2);

  await user.selectOptions(screen.getByLabelText(/filter by action/i), "allow");
  await waitFor(() => expect(rows()).toHaveLength(1));
  expect(screen.getByText("good.example.com")).toBeInTheDocument();

  await user.selectOptions(screen.getByLabelText(/filter by action/i), "");
  await waitFor(() => expect(rows()).toHaveLength(2));
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

// An empty rules tab does not mean the group filters nothing — the lists on
// the neighbouring tab still apply. Saying so is the difference between
// "nothing is set up" and "nothing is excepted".
test("an empty group says lists still apply, and offers to add a rule", async () => {
  mockGroups([group()]);
  mockRulesByGroup({ 1: [] });

  renderWithProviders(<RulesTab />);

  expect(await screen.findByText(/no rules in default/i)).toBeInTheDocument();
  expect(screen.getByText(/lists still apply/i)).toBeInTheDocument();
  expect(screen.getAllByRole("button", { name: /^add rule$/i })).toHaveLength(2);
});
