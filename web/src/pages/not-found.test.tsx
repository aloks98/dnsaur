import { expect, test } from "vitest";
import { screen } from "@testing-library/react";
import { renderWithProviders } from "../test/render";
import { NotFound } from "./not-found";

test("names the failure in a heading, not with the decorative numeral", () => {
  renderWithProviders(<NotFound />);

  // The big "404" is texture. If it were announced, a screen reader would
  // open the page with a bare number before reaching anything that says what
  // went wrong.
  expect(screen.getByText("404")).toHaveAttribute("aria-hidden", "true");
  expect(screen.getByRole("heading", { name: "NXDOMAIN" })).toBeInTheDocument();
});

test("offers a way back rather than stranding the user", () => {
  renderWithProviders(<NotFound />);

  // The whole reason this screen exists instead of a redirect: the user has
  // to be told, *and* still be one click from somewhere real.
  expect(screen.getByRole("link", { name: /back to dashboard/i })).toHaveAttribute("href", "/");
});
