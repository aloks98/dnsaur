import { expect, test, vi } from "vitest";
import { fireEvent, render, screen, within } from "@testing-library/react";
import { parseUpstreams } from "../lib/upstreams";
import { UpstreamsField } from "./upstreams-field";

// UpstreamsField has no react-query/router dependency (unlike
// pause-control.tsx), so plain `render` is enough — see
// dnsaur-logo.test.tsx for the same no-providers shape. Every test supplies
// its own `onChange` spy and reads the emitted string back through
// `parseUpstreams`, per the brief: assert on what the grammar accepts, not
// on the exact string, so a change to (say) a preset's second address
// doesn't break the test.

test("given a plain value, the Plain transport is selected and there is no Server name field", () => {
  render(
    <UpstreamsField value="1.1.1.1:53,1.0.0.1:53" onChange={vi.fn<(next: string) => void>()} />,
  );

  expect(screen.getByRole("radio", { name: /plain/i })).toBeChecked();
  expect(screen.getByRole("radio", { name: /^tls/i })).not.toBeChecked();
  expect(screen.queryByLabelText(/server name/i)).not.toBeInTheDocument();
  // The preset row only exists for encrypted transports.
  expect(screen.queryByText(/preset/i)).not.toBeInTheDocument();
});

test("given a tls:// value, DNS-over-TLS is selected and address/name show as separate values", () => {
  render(
    <UpstreamsField
      value="tls://1.1.1.1:853#cloudflare-dns.com"
      onChange={vi.fn<(next: string) => void>()}
    />,
  );

  expect(screen.getByRole("radio", { name: /^tls/i })).toBeChecked();
  expect(screen.getByLabelText("Address")).toHaveValue("1.1.1.1:853");
  expect(screen.getByLabelText("Server name")).toHaveValue("cloudflare-dns.com");
});

test("choosing the Cloudflare preset under DNS-over-TLS emits a value parseUpstreams accepts, all tls", () => {
  const onChange = vi.fn<(next: string) => void>();
  render(<UpstreamsField value="" onChange={onChange} />);

  fireEvent.click(screen.getByRole("radio", { name: /^tls/i }));
  fireEvent.click(screen.getByRole("button", { name: /^cloudflare$/i }));

  expect(onChange).toHaveBeenCalled();
  const emitted = onChange.mock.calls.at(-1)?.[0] as string;
  const parsed = parseUpstreams(emitted);
  if (!parsed.ok) throw new Error(`emitted value was rejected: ${parsed.error.message}`);
  expect(parsed.entries.every((e) => e.scheme === "tls")).toBe(true);
  expect(parsed.entries).toHaveLength(2);

  // The artboard's own two rows, both columns filled.
  const addresses = screen.getAllByLabelText("Address");
  const names = screen.getAllByLabelText("Server name");
  expect(addresses.map((el) => (el as HTMLInputElement).value)).toEqual([
    "1.1.1.1:853",
    "1.0.0.1:853",
  ]);
  expect(names.map((el) => (el as HTMLInputElement).value)).toEqual([
    "cloudflare-dns.com",
    "cloudflare-dns.com",
  ]);
});

test("choosing the Cloudflare preset under DNS-over-HTTPS shows one row with the /dns-query path", () => {
  const onChange = vi.fn<(next: string) => void>();
  render(<UpstreamsField value="" onChange={onChange} />);

  fireEvent.click(screen.getByRole("radio", { name: /^https/i }));
  fireEvent.click(screen.getByRole("button", { name: /^cloudflare$/i }));

  const emitted = onChange.mock.calls.at(-1)?.[0] as string;
  const parsed = parseUpstreams(emitted);
  if (!parsed.ok) throw new Error(`emitted value was rejected: ${parsed.error.message}`);
  expect(parsed.entries).toHaveLength(1);
  expect(parsed.entries[0].scheme).toBe("https");

  expect(screen.getAllByLabelText("Address")).toHaveLength(1);
  expect(screen.getByLabelText("Path")).toHaveValue("/dns-query");
  expect(screen.getByLabelText("Server name")).toHaveValue("cloudflare-dns.com");
});

// The artboard demonstrates the invalid state with a bare hostname
// ("cloudflare-dns.com") typed into the first row's Address under Custom —
// this reproduces exactly that.
test("typing a hostname into Address under DNS-over-TLS shows the fix-it message and never calls onChange", () => {
  const onChange = vi.fn<(next: string) => void>();
  render(<UpstreamsField value="" onChange={onChange} />);

  fireEvent.click(screen.getByRole("radio", { name: /^tls/i }));
  fireEvent.click(screen.getByRole("button", { name: /^custom$/i }));
  // Switching into tls from a wholly blank Plain state harmlessly re-emits
  // "" (still nothing configured) — clear that before the assertion below,
  // which is about the hostname edit specifically, not the earlier switch.
  onChange.mockClear();
  fireEvent.change(screen.getByLabelText("Address"), {
    target: { value: "cloudflare-dns.com" },
  });

  expect(
    screen.getByText("Needs an address, not a name. Put the name in Server name."),
  ).toBeInTheDocument();
  expect(screen.getByLabelText("Address")).toHaveAttribute("aria-invalid", "true");
  expect(onChange).not.toHaveBeenCalled();
});

test("switching DNS-over-TLS to Plain keeps the addresses and drops the server names", () => {
  const onChange = vi.fn<(next: string) => void>();
  render(
    <UpstreamsField
      value="tls://1.1.1.1:853#cloudflare-dns.com,tls://1.0.0.1:853#cloudflare-dns.com"
      onChange={onChange}
    />,
  );

  fireEvent.click(screen.getByRole("radio", { name: /plain/i }));

  expect(screen.getByText("Server names dropped.")).toBeInTheDocument();
  expect(screen.queryByLabelText(/server name/i)).not.toBeInTheDocument();

  const emitted = onChange.mock.calls.at(-1)?.[0] as string;
  const parsed = parseUpstreams(emitted);
  if (!parsed.ok) throw new Error(`emitted value was rejected: ${parsed.error.message}`);
  expect(parsed.entries.map((e) => e.scheme)).toEqual(["udp", "udp"]);
  expect(parsed.entries.map((e) => e.addr)).toEqual(["1.1.1.1:853", "1.0.0.1:853"]);
});

test("switching Plain with a hostname entry to DNS-over-TLS clears the entries and says so", () => {
  const onChange = vi.fn<(next: string) => void>();
  render(<UpstreamsField value="resolver.lan:5353" onChange={onChange} />);

  fireEvent.click(screen.getByRole("radio", { name: /^tls/i }));

  expect(screen.getByText("Entries cleared — they had no address.")).toBeInTheDocument();
  expect(onChange).toHaveBeenCalledWith("");
  // Left with one empty row to fill in, not zero rows and nowhere to type.
  expect(screen.getByLabelText("Address")).toHaveValue("");
});

test("removing a row re-validates and re-emits the remaining ones", () => {
  const onChange = vi.fn<(next: string) => void>();
  render(
    <UpstreamsField
      value="tls://1.1.1.1:853#cloudflare-dns.com,tls://1.0.0.1:853#cloudflare-dns.com"
      onChange={onChange}
    />,
  );

  const removeButtons = screen.getAllByRole("button", { name: /remove/i });
  fireEvent.click(removeButtons[1]);

  const emitted = onChange.mock.calls.at(-1)?.[0] as string;
  const parsed = parseUpstreams(emitted);
  if (!parsed.ok) throw new Error(`emitted value was rejected: ${parsed.error.message}`);
  expect(parsed.entries).toHaveLength(1);
  expect(parsed.entries[0].addr).toBe("1.1.1.1:853");
});

test("Add resolver appends a blank, editable row without emitting a change", () => {
  const onChange = vi.fn<(next: string) => void>();
  render(<UpstreamsField value="1.1.1.1:53" onChange={onChange} />);

  fireEvent.click(screen.getByRole("button", { name: /add resolver/i }));

  expect(screen.getAllByLabelText("Address")).toHaveLength(2);
  expect(onChange).not.toHaveBeenCalled();
});

test("the hint below the table names the actual rule for the selected transport", () => {
  const { rerender } = render(
    <UpstreamsField value="1.1.1.1:53" onChange={vi.fn<(next: string) => void>()} />,
  );
  expect(screen.getByText("Plain UDP, with TCP fallback on truncation.")).toBeInTheDocument();

  rerender(
    <UpstreamsField
      value="tls://1.1.1.1:853#cloudflare-dns.com"
      onChange={vi.fn<(next: string) => void>()}
    />,
  );
  expect(screen.getByText("Addresses only — a name here is rejected.")).toBeInTheDocument();
});

test("plain shows only an Address column, no Server name or Path", () => {
  render(
    <UpstreamsField value="1.1.1.1:53,1.0.0.1:53" onChange={vi.fn<(next: string) => void>()} />,
  );

  const headerRow = screen.getByText("Address").closest("div")!;
  expect(within(headerRow).queryByText("Server name")).not.toBeInTheDocument();
  expect(within(headerRow).queryByText("Path")).not.toBeInTheDocument();
});

// The whole point of the guard in upstreams.ts's decodeFragment: before it,
// a "%" in Server name made parseUpstreams throw a URIError out of `emit`,
// so the character appeared in the field, no row error was shown, and the
// edit was silently never registered with the form — with an uncaught
// exception in the console. Now it is an ordinary rejection like any other.
test("typing a bare % into Server name is rejected in-row rather than thrown", () => {
  const onChange = vi.fn<(next: string) => void>();
  render(<UpstreamsField value="tls://1.1.1.1:853#cloudflare-dns.com" onChange={onChange} />);
  onChange.mockClear();

  expect(() =>
    fireEvent.change(screen.getByLabelText("Server name"), { target: { value: "100%" } }),
  ).not.toThrow();

  expect(screen.getByLabelText("Server name")).toHaveValue("100%");
  expect(screen.getByLabelText("Address")).toHaveAttribute("aria-invalid", "true");
  expect(onChange).not.toHaveBeenCalled();
});

// The render path, which is the worse half: deriveState runs in a useState
// initializer, so a stored value with a malformed escape (hand-edited — the
// API refuses to store one) used to take the whole settings page down rather
// than just this field. It falls back to a single Plain row holding the raw
// text, the same as any other value this editor cannot make sense of.
test("a stored value with a malformed percent-escape renders instead of throwing", () => {
  expect(() =>
    render(
      <UpstreamsField value="tls://1.1.1.1:853#a%zz" onChange={vi.fn<(next: string) => void>()} />,
    ),
  ).not.toThrow();

  expect(screen.getByRole("radio", { name: /plain/i })).toBeChecked();
  expect(screen.getByLabelText("Address")).toHaveValue("tls://1.1.1.1:853#a%zz");
});

// The bug reported by manual testing: after selecting a named preset,
// Custom is unselectable — activePreset is derived from the rows, so as
// long as the rows still match Cloudflare exactly, clicking Custom just
// recomputes "cloudflare" and the chip snaps back. The fix pins Custom as
// an explicit override that coexists with the derivation rather than
// replacing it.
test("clicking Custom after a preset is applied keeps the rows and pins the chip to Custom", () => {
  const onChange = vi.fn<(next: string) => void>();
  render(<UpstreamsField value="" onChange={onChange} />);

  fireEvent.click(screen.getByRole("radio", { name: /^tls/i }));
  fireEvent.click(screen.getByRole("button", { name: /^cloudflare$/i }));

  const addressesBefore = screen
    .getAllByLabelText("Address")
    .map((el) => (el as HTMLInputElement).value);
  const namesBefore = screen
    .getAllByLabelText("Server name")
    .map((el) => (el as HTMLInputElement).value);

  fireEvent.click(screen.getByRole("button", { name: /^custom$/i }));

  expect(screen.getAllByLabelText("Address").map((el) => (el as HTMLInputElement).value)).toEqual(
    addressesBefore,
  );
  expect(
    screen.getAllByLabelText("Server name").map((el) => (el as HTMLInputElement).value),
  ).toEqual(namesBefore);
  expect(screen.getByRole("button", { name: /^custom$/i })).toHaveAttribute("aria-pressed", "true");
  expect(screen.getByRole("button", { name: /^cloudflare$/i })).toHaveAttribute(
    "aria-pressed",
    "false",
  );
});

test("choosing a named preset after Custom clears the pin and re-applies that preset's rows", () => {
  const onChange = vi.fn<(next: string) => void>();
  render(<UpstreamsField value="" onChange={onChange} />);

  fireEvent.click(screen.getByRole("radio", { name: /^tls/i }));
  fireEvent.click(screen.getByRole("button", { name: /^cloudflare$/i }));
  fireEvent.click(screen.getByRole("button", { name: /^custom$/i }));

  fireEvent.click(screen.getByRole("button", { name: /^cloudflare$/i }));

  expect(screen.getByRole("button", { name: /^cloudflare$/i })).toHaveAttribute(
    "aria-pressed",
    "true",
  );
  expect(screen.getByRole("button", { name: /^custom$/i })).toHaveAttribute(
    "aria-pressed",
    "false",
  );
  const addresses = screen.getAllByLabelText("Address").map((el) => (el as HTMLInputElement).value);
  expect(addresses).toEqual(["1.1.1.1:853", "1.0.0.1:853"]);
});

test("changing transport clears the Custom pin, so a row set that still matches a preset lights it again", () => {
  const onChange = vi.fn<(next: string) => void>();
  render(<UpstreamsField value="" onChange={onChange} />);

  fireEvent.click(screen.getByRole("radio", { name: /^tls/i }));
  fireEvent.click(screen.getByRole("button", { name: /^quad9$/i }));
  fireEvent.click(screen.getByRole("button", { name: /^custom$/i }));

  // tls <-> https is the one transport change that leaves the rows
  // byte-for-byte untouched (both are "encrypted", so changeTransport's
  // carry-over logic doesn't fire) — round-tripping through it means any
  // difference in the resulting chip is down to the pin, not a row rewrite.
  fireEvent.click(screen.getByRole("radio", { name: /^https/i }));
  fireEvent.click(screen.getByRole("radio", { name: /^tls/i }));

  expect(screen.getAllByLabelText("Address").map((el) => (el as HTMLInputElement).value)).toEqual([
    "9.9.9.9:853",
    "149.112.112.112:853",
  ]);
  expect(screen.getByRole("button", { name: /^quad9$/i })).toHaveAttribute("aria-pressed", "true");
  expect(screen.getByRole("button", { name: /^custom$/i })).toHaveAttribute(
    "aria-pressed",
    "false",
  );
});

test("with no pin set, hand-editing a preset row away from the preset still moves the chip to Custom on its own", () => {
  const onChange = vi.fn<(next: string) => void>();
  render(<UpstreamsField value="" onChange={onChange} />);

  fireEvent.click(screen.getByRole("radio", { name: /^tls/i }));
  fireEvent.click(screen.getByRole("button", { name: /^cloudflare$/i }));

  fireEvent.change(screen.getAllByLabelText("Address")[0], {
    target: { value: "1.1.1.2:853" },
  });

  expect(screen.getByRole("button", { name: /^custom$/i })).toHaveAttribute("aria-pressed", "true");
  expect(screen.getByRole("button", { name: /^cloudflare$/i })).toHaveAttribute(
    "aria-pressed",
    "false",
  );
});

// Rows are keyed by a stable per-row id, not by array index. With an index
// key React reuses row *i*'s DOM node for what was row *i+1* after a
// removal, so the node the operator was typing in is the one that unmounts:
// focus and caret land somewhere else, mid-edit, in a table of text inputs.
test("removing a row leaves focus in the row that was being edited", () => {
  render(
    <UpstreamsField
      value="tls://1.1.1.1:853#a.test,tls://1.0.0.1:853#b.test,tls://9.9.9.9:853#c.test"
      onChange={vi.fn<(next: string) => void>()}
    />,
  );

  const third = screen.getAllByLabelText("Address")[2] as HTMLInputElement;
  expect(third).toHaveValue("9.9.9.9:853");
  third.focus();
  expect(document.activeElement).toBe(third);

  fireEvent.click(screen.getAllByRole("button", { name: "Remove" })[0]);

  expect(screen.getAllByLabelText("Address")).toHaveLength(2);
  expect(document.activeElement).toHaveValue("9.9.9.9:853");
});
