import { http, HttpResponse } from "msw";
import { toast } from "sonner";
import { afterEach, expect, test, vi } from "vitest";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useForm } from "react-hook-form";
import { server } from "../test/msw-server";
import { renderWithProviders } from "../test/render";
import type { SyncStatus, TSIGKey } from "../api/types";
import { rhfName } from "../lib/rhf-name";
import { SyncField } from "./sync-field";

// The Sync band is wired to settings.tsx's react-hook-form instance through
// `control`, exactly like the Protocols group — see protocols-field.test.tsx
// for the same harness and sync-field.tsx for why the band is rendered
// wholesale rather than as five SettingRow calls.
function defaultValues(overrides: Record<string, string> = {}): Record<string, string> {
  return {
    [rhfName("sync.peer_url")]: "",
    [rhfName("sync.token")]: "",
    [rhfName("sync.interval_seconds")]: "30",
    [rhfName("sync.primary_dns")]: "",
    [rhfName("sync.tsig_key_id")]: "0",
    ...overrides,
  };
}

function Harness({ values }: { values: Record<string, string> }) {
  const form = useForm<Record<string, string>>({ defaultValues: values });
  return <SyncField control={form.control} />;
}

function mockSyncStatus(status: SyncStatus) {
  server.use(http.get("/api/v1/sync/status", () => HttpResponse.json(status)));
}

function mockTSIGKeys(keys: TSIGKey[]) {
  server.use(http.get("/api/v1/tsig-keys", () => HttpResponse.json(keys)));
}

afterEach(() => vi.restoreAllMocks());

// --- replica --------------------------------------------------------------

test("a replica states the peer it follows, that a token is stored, and how far it has applied", async () => {
  mockSyncStatus({
    role: "replica",
    peer_url: "https://main.lan",
    peer_version: 412,
    applied_version: 412,
    applied_at: Date.now() - 30_000,
    last_pull_at: Date.now() - 30_000,
  });

  renderWithProviders(
    <Harness
      values={defaultValues({
        [rhfName("sync.peer_url")]: "https://main.lan",
        [rhfName("sync.interval_seconds")]: "30",
        [rhfName("sync.primary_dns")]: "10.0.0.5:53",
      })}
    />,
  );

  expect(await screen.findByText("applied 412 of 412")).toBeInTheDocument();
  expect(screen.getByLabelText("Peer URL")).toHaveValue("https://main.lan");
  // The credential is never returned by GET /settings, so the band states
  // whether one is stored rather than showing it.
  expect(screen.getByText("Token: set")).toBeInTheDocument();
  expect(screen.getByLabelText("Pull interval (seconds)")).toHaveValue("30");
  expect(screen.getByLabelText("Primary DNS address")).toHaveValue("10.0.0.5:53");
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

  renderWithProviders(
    <Harness values={defaultValues({ [rhfName("sync.peer_url")]: "https://main.lan" })} />,
  );

  await userEvent.click(await screen.findByRole("button", { name: "Stop following" }));

  // One request, both keys: the server applies a map all-or-nothing and
  // judges a peer being cleared before the token that would otherwise be
  // refused under it (settingsPhases, internal/api/settings_handlers.go).
  await waitFor(() => expect(body).toEqual({ "sync.peer_url": "", "sync.token": "" }));
});

test("a main with no peer says no token is stored", async () => {
  mockSyncStatus({ role: "main" });

  renderWithProviders(<Harness values={defaultValues()} />);

  expect(await screen.findByText("Token: not set")).toBeInTheDocument();
  expect(screen.queryByRole("button", { name: "Stop following" })).not.toBeInTheDocument();
});

// --- main -----------------------------------------------------------------

test("a main picks the key replicas sign with from the stored TSIG keys", async () => {
  mockSyncStatus({ role: "main", sync_key: "xfer.example.com." });
  mockTSIGKeys([
    {
      id: 7,
      name: "xfer.example.com.",
      algorithm: "hmac-sha256.",
      secret: "Sh5ZuulpjcmcJuN6VwMQCVEhTJyUmlPTSHexvePtaWo=",
      created_at: Date.now(),
    },
  ]);

  renderWithProviders(<Harness values={defaultValues()} />);

  const select = await screen.findByLabelText("Sync key");
  expect(select).toHaveValue("0");
  expect(within(select).getByRole("option", { name: "None" })).toBeInTheDocument();
  expect(within(select).getByRole("option", { name: "xfer.example.com." })).toBeInTheDocument();

  await userEvent.selectOptions(select, "7");
  expect(select).toHaveValue("7");
});

test("a main lists each registered replica and forgets one on request", async () => {
  mockSyncStatus({
    role: "main",
    sync_key: "xfer.example.com.",
    replicas: [
      {
        instance_id: "eve-2",
        dns_addr: "10.0.0.6:53",
        version_applied: 412,
        last_seen: Date.now() - 5 * 60_000,
        stale: false,
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

  renderWithProviders(<Harness values={defaultValues()} />);

  const row = await screen.findByRole("row", { name: /eve-2/ });
  expect(within(row).getByText("10.0.0.6:53")).toBeInTheDocument();
  expect(within(row).getByText("412")).toBeInTheDocument();
  expect(within(row).getByText("5m ago")).toBeInTheDocument();

  await userEvent.click(within(row).getByRole("button", { name: "Forget eve-2" }));

  await waitFor(() => expect(deleted).toBe("eve-2"));
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

  renderWithProviders(
    <Harness values={defaultValues({ [rhfName("sync.peer_url")]: "https://main.lan" })} />,
  );

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
      },
    ],
  });
  server.use(
    http.delete("/api/v1/sync/replicas/:instanceId", () =>
      HttpResponse.json({ error: "boom" }, { status: 500 }),
    ),
  );
  const errorSpy = vi.spyOn(toast, "error");

  renderWithProviders(<Harness values={defaultValues()} />);

  const row = await screen.findByRole("row", { name: /eve-2/ });
  await userEvent.click(within(row).getByRole("button", { name: "Forget eve-2" }));

  await waitFor(() => expect(errorSpy).toHaveBeenCalledWith("Couldn't forget eve-2 — try again"));
});
