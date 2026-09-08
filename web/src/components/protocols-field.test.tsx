import { act, useEffect } from "react";
import { http, HttpResponse } from "msw";
import { expect, test } from "vitest";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useForm, type UseFormReturn } from "react-hook-form";
import { server } from "../test/msw-server";
import { renderWithProviders } from "../test/render";
import type { ResolverStatus } from "../api/types";
import { rhfName } from "../lib/rhf-name";
import { ProtocolsField } from "./protocols-field";

// The Protocols group is wired directly to settings.tsx's react-hook-form
// instance via `control` (see protocols-field.tsx's module doc comment for
// why it isn't decomposed into settings.tsx's usual SettingRow calls). This
// harness stands in for that page: a real useForm(), seeded with the same
// mangled field names settings.tsx would produce.
function defaultValues(overrides: Record<string, string> = {}): Record<string, string> {
  return {
    [rhfName("serve.dot.enabled")]: "false",
    [rhfName("serve.dot.listen")]: ":853",
    [rhfName("serve.doh.enabled")]: "false",
    [rhfName("serve.doh.listen")]: ":443",
    [rhfName("serve.tls.cert")]: "",
    [rhfName("serve.tls.key")]: "",
    ...overrides,
  };
}

function Harness({
  values,
  onForm,
}: {
  values: Record<string, string>;
  onForm?: (form: UseFormReturn<Record<string, string>>) => void;
}) {
  const form = useForm<Record<string, string>>({ defaultValues: values });
  useEffect(() => {
    onForm?.(form);
    // Only ever seeded once per test — a fresh Harness mount each time.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);
  return <ProtocolsField control={form.control} />;
}

function mockResolverStatus(status: ResolverStatus) {
  server.use(http.get("/api/v1/resolver/status", () => HttpResponse.json(status)));
}

function baseServing(): ResolverStatus["serving"] {
  return {
    dot: { enabled: false, listening: false, addr: "" },
    doh: { enabled: false, listening: false, addr: "" },
  };
}

function dotGroup(): HTMLElement {
  return screen.getByRole("group", { name: "DNS-over-TLS" });
}

test("a disabled protocol shows off", async () => {
  mockResolverStatus({
    encryption_downgraded: false,
    reason: "",
    serving: baseServing(),
  });

  renderWithProviders(<Harness values={defaultValues()} />);

  const line = await within(dotGroup()).findByText("off");
  expect(line).toHaveClass("text-muted-foreground");
});

test("an enabled and listening protocol shows its address", async () => {
  mockResolverStatus({
    encryption_downgraded: false,
    reason: "",
    serving: {
      ...baseServing(),
      dot: { enabled: true, listening: true, addr: ":853" },
    },
  });

  renderWithProviders(
    <Harness values={defaultValues({ [rhfName("serve.dot.enabled")]: "true" })} />,
  );

  const line = await within(dotGroup()).findByText("listening on :853");
  expect(line).toHaveClass("text-success-foreground");
});

test("an enabled but failed protocol shows the real bind error, visually distinct from listening", async () => {
  mockResolverStatus({
    encryption_downgraded: false,
    reason: "",
    serving: {
      ...baseServing(),
      dot: {
        enabled: true,
        listening: false,
        addr: ":853",
        error: "listen tcp :853: bind: permission denied",
      },
    },
  });

  renderWithProviders(
    <Harness values={defaultValues({ [rhfName("serve.dot.enabled")]: "true" })} />,
  );

  const line = await within(dotGroup()).findByText(
    "not listening — listen tcp :853: bind: permission denied",
  );
  // The real API error, not the artboard's own hardcoded example text —
  // pinning the literal string above already proves that, this pins the
  // "visually distinct" half.
  expect(line).toHaveClass("text-destructive-foreground");
  expect(line).not.toHaveClass("text-success-foreground");
  expect(line).not.toHaveClass("text-muted-foreground");
});

test("both protocol rows render side by side, independently of each other", async () => {
  mockResolverStatus({
    encryption_downgraded: false,
    reason: "",
    serving: {
      dot: {
        enabled: true,
        listening: false,
        addr: ":853",
        error: "listen tcp :853: bind: permission denied",
      },
      doh: { enabled: true, listening: true, addr: ":443" },
    },
  });

  renderWithProviders(
    <Harness
      values={defaultValues({
        [rhfName("serve.dot.enabled")]: "true",
        [rhfName("serve.doh.enabled")]: "true",
      })}
    />,
  );

  expect(await within(dotGroup()).findByText(/not listening/)).toBeInTheDocument();
  const dohLine = await within(screen.getByRole("group", { name: "DNS-over-HTTPS" })).findByText(
    "listening on :443",
  );
  expect(dohLine).toHaveClass("text-success-foreground");

  // 1fr 1fr, not stacked — the artboard's own layout requirement.
  const grid = dotGroup().parentElement;
  expect(grid).toHaveClass("grid-cols-2");
});

test("clicking a protocol's checkbox flips its enabled setting", async () => {
  mockResolverStatus({ encryption_downgraded: false, reason: "", serving: baseServing() });
  const user = userEvent.setup();
  let latestForm: UseFormReturn<Record<string, string>> | undefined;

  renderWithProviders(<Harness values={defaultValues()} onForm={(form) => (latestForm = form)} />);
  await within(dotGroup()).findByText("off");

  const checkbox = within(dotGroup()).getByRole("checkbox", { name: /DNS-over-TLS/ });
  expect(checkbox).not.toBeChecked();
  await user.click(checkbox);

  expect(checkbox).toBeChecked();
  await waitFor(() => expect(latestForm?.getValues(rhfName("serve.dot.enabled"))).toBe("true"));
});

test("the certificate line reads valid when a certificate has loaded and isn't expiring soon", async () => {
  mockResolverStatus({
    encryption_downgraded: false,
    reason: "",
    serving: baseServing(),
    certificate: { not_after: "2026-11-14T00:00:00Z", expiring_soon: false },
  });

  renderWithProviders(<Harness values={defaultValues()} />);

  const line = await screen.findByText("Expires 14 Nov 2026");
  expect(line).toHaveClass("text-muted-foreground");
  expect(screen.queryByText(/expires in/i)).not.toBeInTheDocument();
});

test("the certificate line counts down when it's within the expiry warning window", async () => {
  // Nine days plus an hour. daysUntil floors rather than rounds (a
  // certificate 36 hours out must not read "2 days"), so an expiry exactly
  // nine days away lands on 8 the moment any wall-clock time passes
  // between building this date and rendering.
  const notAfter = new Date(Date.now() + 9 * 24 * 60 * 60 * 1000 + 60 * 60 * 1000);
  mockResolverStatus({
    encryption_downgraded: false,
    reason: "",
    serving: baseServing(),
    certificate: { not_after: notAfter.toISOString(), expiring_soon: true },
  });

  renderWithProviders(<Harness values={defaultValues()} />);

  const line = await screen.findByText(/^Expires in 9 days — /);
  expect(line).toHaveClass("text-warning-foreground");
});

test("no certificate loaded is its own state, not an expiry warning", async () => {
  mockResolverStatus({
    encryption_downgraded: false,
    reason: "",
    serving: baseServing(),
    // certificate omitted entirely — nothing has ever loaded.
  });

  renderWithProviders(<Harness values={defaultValues()} />);

  const line = await screen.findByText("No certificate loaded.");
  expect(line).toHaveClass("text-muted-foreground");
  expect(screen.queryByText(/expires/i)).not.toBeInTheDocument();
});

test("an unreadable certificate shows the save-time rejection reason, destructively", async () => {
  mockResolverStatus({
    encryption_downgraded: false,
    reason: "",
    serving: baseServing(),
    // No certificate ever loaded — same as the "none" fixture above — but
    // this time a save was attempted and rejected, which is the only
    // source for "unreadable" (see protocols-field.tsx's certLine doc
    // comment: the API itself never reports *why* nothing loaded).
  });
  let latestForm: UseFormReturn<Record<string, string>> | undefined;

  renderWithProviders(
    <Harness
      values={defaultValues({
        [rhfName("serve.tls.cert")]: "/etc/letsencrypt/live/adam.dns.e412.in/fullchain.pem",
        [rhfName("serve.tls.key")]: "/etc/letsencrypt/live/adam.dns.e412.in/privkey.pem",
      })}
      onForm={(form) => (latestForm = form)}
    />,
  );
  await screen.findByText("No certificate loaded.");

  act(() => {
    latestForm?.setError(rhfName("serve.tls.key"), {
      type: "server",
      message:
        "invalid value for serve.tls.key: open /etc/letsencrypt/live/adam.dns.e412.in/privkey.pem: permission denied",
    });
  });

  const line = await screen.findByText(
    "invalid value for serve.tls.key: open /etc/letsencrypt/live/adam.dns.e412.in/privkey.pem: permission denied",
  );
  expect(line).toHaveClass("text-destructive-foreground");
  expect(screen.queryByText("No certificate loaded.")).not.toBeInTheDocument();

  // Both certificate path inputs flag invalid, not just the one whose save
  // actually failed — a broken pair is a fact about the pair.
  expect(screen.getByLabelText("Certificate")).toHaveAttribute("aria-invalid", "true");
  expect(screen.getByLabelText("Private key")).toHaveAttribute("aria-invalid", "true");
});

test("the closing note about certificate order is always shown", async () => {
  mockResolverStatus({ encryption_downgraded: false, reason: "", serving: baseServing() });

  renderWithProviders(<Harness values={defaultValues()} />);

  expect(
    await screen.findByText("Set the certificate before enabling a protocol."),
  ).toBeInTheDocument();
});

// --- I3: a status that never answers must not read as fact --------------

test("a failing status endpoint says so rather than reporting off", async () => {
  server.use(http.get("/api/v1/resolver/status", () => new HttpResponse(null, { status: 500 })));

  // The box is ticked: the setting says DoT is on. Only the *reality* is
  // unknown, which is exactly the pair that used to read "☑ DNS-over-TLS …
  // ○ off" beside "No certificate loaded." — three confident statements
  // about a listener that may be serving perfectly well, and an operator
  // whose natural remedy (untick, re-tick) takes it down.
  renderWithProviders(
    <Harness values={defaultValues({ [rhfName("serve.dot.enabled")]: "true" })} />,
  );

  // retry: 1 in lib/query-client.ts, so the error state lands after a
  // backoff — past findBy's default timeout.
  const line = await within(dotGroup()).findByText("status unavailable", undefined, {
    timeout: 3000,
  });
  expect(line).toBeInTheDocument();
  expect(within(dotGroup()).queryByText("off")).not.toBeInTheDocument();
  expect(screen.getByText("Status unavailable.")).toBeInTheDocument();
  expect(screen.queryByText("No certificate loaded.")).not.toBeInTheDocument();
});

test("a save-time certificate rejection still shows while the status is unknown", async () => {
  server.use(http.get("/api/v1/resolver/status", () => new HttpResponse(null, { status: 500 })));

  let form: UseFormReturn<Record<string, string>> | undefined;
  renderWithProviders(<Harness values={defaultValues()} onForm={(f) => (form = f)} />);
  await screen.findByLabelText("Certificate");

  act(() => {
    form?.setError(rhfName("serve.tls.cert"), {
      type: "server",
      message: "certificate: open /etc/ssl/x.pem: permission denied",
    });
  });

  // The reason came from this form's own save, not from the endpoint that
  // is not answering, so it outranks "Status unavailable."
  expect(await screen.findByText(/permission denied/)).toBeInTheDocument();
});

test("an expired certificate reads as expired, not as expiring in 0 days", async () => {
  mockResolverStatus({
    encryption_downgraded: false,
    reason: "",
    serving: baseServing(),
    // The server reports a lapsed certificate as expiring_soon (see
    // internal/app/serve.go's CertExpiry), so the past tense has to come
    // from the date.
    certificate: { not_after: "2020-03-04T00:00:00Z", expiring_soon: true },
  });

  renderWithProviders(<Harness values={defaultValues()} />);

  const line = await screen.findByText("Expired — 4 Mar 2020");
  expect(line).toHaveClass("text-destructive-foreground");
});
