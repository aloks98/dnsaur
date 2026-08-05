import { expect, test } from "vitest";
import { render, screen } from "@testing-library/react";
import { DnsaurLogo } from "./dnsaur-logo";

test("renders an accessible image", () => {
  render(<DnsaurLogo />);
  expect(screen.getByRole("img", { name: /dnsaur logo/i })).toBeInTheDocument();
});

test("light variant paints the tile accent green with a black body", () => {
  const { container } = render(<DnsaurLogo variant="light" />);
  expect(container.querySelector('[data-part="tile"]')).toHaveAttribute("fill", "#2FE26F");
  expect(container.querySelector('[data-part="body"]')).toHaveAttribute("fill", "#101010");
});

test("dark variant paints the tile forest with a green body", () => {
  const { container } = render(<DnsaurLogo variant="dark" />);
  expect(container.querySelector('[data-part="tile"]')).toHaveAttribute("fill", "#0F2B1C");
  expect(container.querySelector('[data-part="body"]')).toHaveAttribute("fill", "#2FE26F");
});

test("two instances don't collide on clipPath ids", () => {
  const { container } = render(
    <>
      <DnsaurLogo />
      <DnsaurLogo />
    </>,
  );
  const ids = [...container.querySelectorAll("clipPath")].map((el) => el.id);
  expect(new Set(ids).size).toBe(ids.length);
});
