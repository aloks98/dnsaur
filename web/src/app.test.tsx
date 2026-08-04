import { render, screen } from "@testing-library/react";
import { App } from "./app";

test("renders the dnsaur home card", () => {
  render(<App />);
  expect(screen.getByRole("heading", { name: /dnsaur/i })).toBeInTheDocument();
});
