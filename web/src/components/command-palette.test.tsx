import { expect, test, vi } from "vitest";
import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useLocation } from "react-router";
import { renderWithProviders } from "../test/render";
import { CommandPalette } from "./command-palette";

// The palette shipped broken: rnui's CommandDialog is only the Dialog shell
// and does not wrap its children in the cmdk root, so every Command* part
// read cmdk's store off a context defaulting to undefined and opening the
// palette threw "Cannot read properties of undefined (reading 'subscribe')".
// Nothing caught it because nothing ever rendered this component open.
function Harness({ open }: { open: boolean }) {
  const location = useLocation();
  return (
    <>
      <span data-testid="pathname">{location.pathname}</span>
      <CommandPalette open={open} onOpenChange={() => {}} />
    </>
  );
}

test("opening the palette lists every page, grouped by nav section", async () => {
  renderWithProviders(<Harness open />);

  expect(await screen.findByPlaceholderText(/jump to a page/i)).toBeInTheDocument();
  // Every *leaf* page, including the three Filtering panels that are now
  // routes of their own rather than one "Filtering" entry with local tabs.
  for (const label of [
    "Dashboard",
    "Query Log",
    "Lists",
    "Rules",
    "Groups & Clients",
    "Local DNS",
    "Settings",
    "Account",
  ]) {
    expect(screen.getByRole("option", { name: label })).toBeInTheDocument();
  }
  // ...under the same four headings the top bar's first row shows.
  for (const heading of ["Monitor", "Filtering", "Network", "System"]) {
    expect(screen.getByText(heading)).toBeInTheDocument();
  }
});

test("filtering narrows the list and falls back to the empty state", async () => {
  renderWithProviders(<Harness open />);

  const input = await screen.findByPlaceholderText(/jump to a page/i);
  await userEvent.type(input, "quer");
  await waitFor(() =>
    expect(screen.getByRole("option", { name: "Query Log" })).toBeInTheDocument(),
  );
  expect(screen.queryByRole("option", { name: "Settings" })).not.toBeInTheDocument();

  await userEvent.clear(input);
  await userEvent.type(input, "zzzz");
  expect(await screen.findByText(/no matching pages/i)).toBeInTheDocument();
});

test("selecting a page navigates to it", async () => {
  const onOpenChange = vi.fn<(open: boolean) => void>();
  function NavHarness() {
    const location = useLocation();
    return (
      <>
        <span data-testid="pathname">{location.pathname}</span>
        <CommandPalette open onOpenChange={onOpenChange} />
      </>
    );
  }
  renderWithProviders(<NavHarness />);

  await userEvent.click(await screen.findByRole("option", { name: "Local DNS" }));

  await waitFor(() => expect(screen.getByTestId("pathname")).toHaveTextContent("/dns"));
  expect(onOpenChange).toHaveBeenCalledWith(false);
});
