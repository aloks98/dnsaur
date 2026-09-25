import { http, HttpResponse } from "msw";
import { toast } from "sonner";
import { afterEach, expect, test, vi } from "vitest";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useForm } from "react-hook-form";
import { dhcpHandlers } from "../test/msw-handlers";
import { server } from "../test/msw-server";
import { renderWithProviders } from "../test/render";
import type { SyncReplica, SyncStatus } from "../api/types";
import { rhfName } from "../lib/rhf-name";
import { SyncField } from "./sync-field";

// The Sync band is wired to settings.tsx's react-hook-form instance through
// `control`, exactly like the Protocols group — see protocols-field.test.tsx
// for the same harness and sync-field.tsx for why the band is rendered
// wholesale rather than as a pair of SettingRow calls.
//
// Only two keys are still form fields: pairing writes the peer URL and the
// secret (POST /sync/follow), and promotion clears them.
function defaultValues(overrides: Record<string, string> = {}): Record<string, string> {
  return {
    [rhfName("sync.interval_seconds")]: "30",
    [rhfName("sync.primary_dns")]: "",
    ...overrides,
  };
}

function Harness({ values = defaultValues() }: { values?: Record<string, string> } = {}) {
  const form = useForm<Record<string, string>>({ defaultValues: values });
  return <SyncField control={form.control} />;
}

function mockSyncStatus(status: SyncStatus) {
  server.use(http.get("/api/v1/sync/status", () => HttpResponse.json(status)));
}

/** The disclosure is closed on load — every Advanced assertion opens it
 * first, the same gesture an operator makes. */
async function openAdvanced() {
  await userEvent.click(await screen.findByRole("button", { name: /advanced/i }));
}

afterEach(() => vi.restoreAllMocks());

// --- main: the replica registry -------------------------------------------

test("a main lists each registered replica and marks a stale one by its last seen", async () => {
  mockSyncStatus({
    role: "main",
    sync_key: "sync-k3n9wq.",
    replicas: [
      {
        instance_id: "backup-box",
        dns_addr: "192.168.150.2:53",
        version_applied: 412,
        last_seen: Date.now() - 40_000,
        stale: false,
        dhcp: true,
      },
      {
        instance_id: "attic-pi",
        dns_addr: "192.168.150.3:53",
        version_applied: 409,
        last_seen: Date.now() - 2 * 60 * 60_000,
        stale: true,
        dhcp: true,
      },
    ],
  });

  renderWithProviders(<Harness />);

  const fresh = await screen.findByRole("row", { name: /backup-box/ });
  expect(within(fresh).getByText("192.168.150.2:53")).toBeInTheDocument();
  expect(within(fresh).getByText("v412")).toBeInTheDocument();
  expect(within(fresh).getByText("just now")).toBeInTheDocument();

  // The stale one is marked by that cell's colour and by nothing else — no
  // badge, no extra word, no row tint.
  const stale = screen.getByRole("row", { name: /attic-pi/ });
  expect(within(stale).getByText("2h ago")).toHaveClass("text-muted-foreground");
});

function twoReplicas(): SyncStatus {
  const base = {
    dns_addr: "192.168.150.2:53",
    version_applied: 412,
    last_seen: Date.now(),
    stale: false,
  };
  return {
    role: "main",
    replicas: [
      { ...base, instance_id: "backup-box", dhcp: true },
      { ...base, instance_id: "attic-pi", dhcp: false },
    ],
  };
}

const noEngineLine = "A replica without an engine is never the standby.";

test("the DHCP column says engine or no engine, the second muted", async () => {
  mockSyncStatus(twoReplicas());
  renderWithProviders(<Harness />);

  const withEngine = await screen.findByRole("row", { name: /backup-box/ });
  expect(within(withEngine).getByText("engine")).not.toHaveClass("text-muted-foreground");
  const without = screen.getByRole("row", { name: /attic-pi/ });
  expect(within(without).getByText("no engine")).toHaveClass("text-muted-foreground");
  expect(screen.getByRole("columnheader", { name: "DHCP" })).toBeInTheDocument();
});

test("a replica from before the dhcp key reads as no engine", async () => {
  const status = twoReplicas();
  // An older replica's registry row has no dhcp key at all.
  const { dhcp: _absent, ...older } = status.replicas![1];
  mockSyncStatus({ ...status, replicas: [status.replicas![0], older as SyncReplica] });
  renderWithProviders(<Harness />);

  const row = await screen.findByRole("row", { name: /attic-pi/ });
  expect(within(row).getByText("no engine")).toHaveClass("text-muted-foreground");
});

test("a replica with no engine gets the standby line when this box runs DHCP", async () => {
  server.use(...dhcpHandlers());
  mockSyncStatus(twoReplicas());
  renderWithProviders(<Harness />);

  expect(await screen.findByText(noEngineLine)).toBeInTheDocument();
});

test("no standby line when this box runs no DHCP", async () => {
  mockSyncStatus(twoReplicas());
  renderWithProviders(<Harness />);

  await screen.findByRole("row", { name: /attic-pi/ });
  expect(screen.queryByText(noEngineLine)).not.toBeInTheDocument();
});

test("no standby line when every replica has an engine", async () => {
  server.use(...dhcpHandlers());
  const status = twoReplicas();
  status.replicas = status.replicas!.map((r) => ({ ...r, dhcp: true }));
  mockSyncStatus(status);
  renderWithProviders(<Harness />);

  await screen.findByRole("row", { name: /attic-pi/ });
  expect(screen.queryByText(noEngineLine)).not.toBeInTheDocument();
});

test("a main offers no key select and no token box", async () => {
  mockSyncStatus({ role: "main", sync_key: "sync-k3n9wq." });

  renderWithProviders(<Harness />);

  expect(await screen.findByRole("button", { name: "Add replica" })).toBeInTheDocument();
  // The main creates its own sync key on the first pairing, so there is
  // nothing to choose; the pull secret is minted by pairing, so there is
  // nothing to type.
  expect(screen.queryByLabelText("Sync key")).not.toBeInTheDocument();
  expect(screen.queryByLabelText("Token")).not.toBeInTheDocument();
});

test("Add replica mints a one-time code and states how long it lives", async () => {
  mockSyncStatus({ role: "main" });
  let minted = 0;
  server.use(
    http.post("/api/v1/sync/pairing-code", () => {
      minted += 1;
      return HttpResponse.json({ code: "7KQ2-9MXA", expires_at: Date.now() + 600_000 });
    }),
  );

  renderWithProviders(<Harness />);

  await userEvent.click(await screen.findByRole("button", { name: "Add replica" }));

  expect(await screen.findByText("7KQ2-9MXA")).toBeInTheDocument();
  expect(minted).toBe(1);
  expect(screen.getByText("PAIRING CODE · ONE-TIME")).toBeInTheDocument();
  expect(screen.getByText("Expires in 10 minutes")).toBeInTheDocument();
});

test("the code panel is dismissed on request, and there is no way to read it back", async () => {
  mockSyncStatus({ role: "main" });
  server.use(
    http.post("/api/v1/sync/pairing-code", () =>
      HttpResponse.json({ code: "7KQ2-9MXA", expires_at: Date.now() + 600_000 }),
    ),
  );

  renderWithProviders(<Harness />);

  await userEvent.click(await screen.findByRole("button", { name: "Add replica" }));
  await screen.findByText("7KQ2-9MXA");

  await userEvent.click(screen.getByRole("button", { name: "Dismiss" }));

  await waitFor(() => expect(screen.queryByText("7KQ2-9MXA")).not.toBeInTheDocument());
});

test("a main forgets a replica on request", async () => {
  mockSyncStatus({
    role: "main",
    replicas: [
      {
        instance_id: "eve-2",
        dns_addr: "10.0.0.6:53",
        version_applied: 412,
        last_seen: Date.now() - 5 * 60_000,
        stale: false,
        dhcp: true,
      },
    ],
  });

  let deleted = "";
  server.use(
    http.delete("/api/v1/sync/replicas/:instanceId", ({ params }) => {
      deleted = String(params.instanceId);
      return new HttpResponse(null, { status: 204 });
    }),
  );

  renderWithProviders(<Harness />);

  const row = await screen.findByRole("row", { name: /eve-2/ });
  await userEvent.click(within(row).getByRole("button", { name: "Forget eve-2" }));

  await waitFor(() => expect(deleted).toBe("eve-2"));
});

test("a main's Advanced holds the pull interval and no primary DNS override", async () => {
  mockSyncStatus({ role: "main" });

  renderWithProviders(<Harness />);

  await openAdvanced();

  expect(screen.getByLabelText("Pull interval (seconds)")).toHaveValue("30");
  // The override is what a replica joins its peer's host with; a main has no
  // peer to derive an address from.
  expect(screen.queryByLabelText("Primary DNS address (override)")).not.toBeInTheDocument();
});

// --- becoming a replica ----------------------------------------------------

test("a box with no peer offers the pairing code the main showed", async () => {
  mockSyncStatus({ role: "main" });

  let body: unknown;
  server.use(
    http.post("/api/v1/sync/follow", async ({ request }) => {
      body = await request.json();
      return new HttpResponse(null, { status: 204 });
    }),
  );

  renderWithProviders(<Harness />);

  await userEvent.type(await screen.findByLabelText("Peer URL"), "https://main.lan");
  await userEvent.type(screen.getByLabelText("Pairing code"), "7KQ2-9MXA");
  await userEvent.click(screen.getByRole("button", { name: "Follow" }));

  await waitFor(() => expect(body).toEqual({ peer_url: "https://main.lan", code: "7KQ2-9MXA" }));
});

test("a code the main would not spend is reported where it was typed", async () => {
  mockSyncStatus({ role: "main" });
  server.use(
    http.post("/api/v1/sync/follow", () =>
      HttpResponse.json({ error: "the main refused the pairing code" }, { status: 502 }),
    ),
  );

  renderWithProviders(<Harness />);

  await userEvent.type(await screen.findByLabelText("Peer URL"), "https://main.lan");
  await userEvent.type(screen.getByLabelText("Pairing code"), "WRNG-CODE");
  await userEvent.click(screen.getByRole("button", { name: "Follow" }));

  // Verbatim from the server: this screen has no better wording for it than
  // the one the API documents. An alert, and named by both boxes: the press
  // that failed moved nothing on screen, so a line only a sighted reader
  // finds is a press that reported nothing.
  const error = await screen.findByRole("alert");
  expect(error).toHaveTextContent("the main refused the pairing code");
  expect(screen.getByLabelText("Peer URL")).toHaveAccessibleDescription(
    "the main refused the pairing code",
  );
  expect(screen.getByLabelText("Pairing code")).toHaveAccessibleDescription(
    "the main refused the pairing code",
  );
});

// --- replica ---------------------------------------------------------------

test("a replica states who it follows and how far it has applied", async () => {
  mockSyncStatus({
    role: "replica",
    peer_url: "https://main.lan",
    peer_version: 412,
    applied_version: 412,
    applied_at: Date.now() - 5 * 60_000,
    last_pull_at: Date.now() - 5 * 60_000,
  });

  renderWithProviders(<Harness />);

  // The peer is its own <span> so the mono face can be on it alone, which
  // puts it outside getByText's direct-text-node matching.
  expect(await screen.findByText(/^Following/)).toHaveTextContent("Following https://main.lan");
  expect(screen.getByText("applied 412 of 412 · last pull 5m ago")).toBeInTheDocument();
  // Neither offer belongs to a box that already follows one.
  expect(screen.queryByRole("button", { name: "Follow" })).not.toBeInTheDocument();
  expect(screen.queryByRole("button", { name: "Add replica" })).not.toBeInTheDocument();
});

test("a replica whose last pull failed says so, and says when the peer is plaintext", async () => {
  mockSyncStatus({
    role: "replica",
    peer_url: "http://main.lan",
    peer_version: 412,
    applied_version: 409,
    last_pull_at: Date.now() - 6 * 60_000,
    last_error: "connection refused",
    plain_http: true,
  });

  renderWithProviders(<Harness />);

  expect(await screen.findByText("Last pull failed: connection refused")).toBeInTheDocument();
  // plain_http is a fact about the peer URL, so it belongs beside the peer
  // rather than in the status panel (lib/serving.ts's statusFacts).
  expect(screen.getByText("Peer reached over plain HTTP")).toBeInTheDocument();
  expect(screen.getByText("applied 409 of 412 · last pull 6m ago")).toBeInTheDocument();
});

test("Stop following clears the peer and the token in one settings write", async () => {
  mockSyncStatus({
    role: "replica",
    peer_url: "https://main.lan",
    peer_version: 412,
    applied_version: 412,
  });

  let body: unknown;
  server.use(
    http.put("/api/v1/settings", async ({ request }) => {
      body = await request.json();
      return new HttpResponse(null, { status: 204 });
    }),
  );

  renderWithProviders(<Harness />);

  await userEvent.click(await screen.findByRole("button", { name: "Stop following" }));

  // One request, both keys: the server applies a map all-or-nothing and
  // judges a peer being cleared before the token that would otherwise be
  // refused under it (settingsPhases, internal/api/settings_handlers.go).
  await waitFor(() => expect(body).toEqual({ "sync.peer_url": "", "sync.token": "" }));
});

test("a replica's Advanced adds the primary DNS override", async () => {
  mockSyncStatus({ role: "replica", peer_url: "https://main.lan" });

  renderWithProviders(
    <Harness values={defaultValues({ [rhfName("sync.primary_dns")]: "192.168.150.1:53" })} />,
  );

  await openAdvanced();

  expect(screen.getByLabelText("Pull interval (seconds)")).toHaveValue("30");
  expect(screen.getByLabelText("Primary DNS address (override)")).toHaveValue("192.168.150.1:53");
  expect(
    screen.getByText("Leave empty to use the address the main advertises."),
  ).toBeInTheDocument();
});

// --- failures -------------------------------------------------------------

// Both of these are one press with no visible result of their own: the
// replica list is re-read from the server and the promotion shows up as the
// screen coming back to life. A refusal that only reached the console would
// read as "nothing happened", which is also what success looks like for a
// second or two.

test("a refused promotion says so", async () => {
  mockSyncStatus({ role: "replica", peer_url: "https://main.lan" });
  server.use(http.put("/api/v1/settings", () => HttpResponse.error()));
  const errorSpy = vi.spyOn(toast, "error");

  renderWithProviders(<Harness />);

  await userEvent.click(await screen.findByRole("button", { name: "Stop following" }));

  await waitFor(() => expect(errorSpy).toHaveBeenCalledWith("Couldn't stop following — try again"));
});

test("a refused forget names the replica it could not remove", async () => {
  mockSyncStatus({
    role: "main",
    replicas: [
      {
        instance_id: "eve-2",
        dns_addr: "10.0.0.6:53",
        version_applied: 412,
        last_seen: Date.now() - 60_000,
        stale: false,
        dhcp: true,
      },
    ],
  });
  server.use(
    http.delete("/api/v1/sync/replicas/:instanceId", () =>
      HttpResponse.json({ error: "boom" }, { status: 500 }),
    ),
  );
  const errorSpy = vi.spyOn(toast, "error");

  renderWithProviders(<Harness />);

  const row = await screen.findByRole("row", { name: /eve-2/ });
  await userEvent.click(within(row).getByRole("button", { name: "Forget eve-2" }));

  await waitFor(() => expect(errorSpy).toHaveBeenCalledWith("Couldn't forget eve-2 — try again"));
});

test("a refused pairing code says so", async () => {
  mockSyncStatus({ role: "main" });
  server.use(http.post("/api/v1/sync/pairing-code", () => HttpResponse.error()));
  const errorSpy = vi.spyOn(toast, "error");

  renderWithProviders(<Harness />);

  await userEvent.click(await screen.findByRole("button", { name: "Add replica" }));

  await waitFor(() =>
    expect(errorSpy).toHaveBeenCalledWith("Couldn't get a pairing code — try again"),
  );
});
