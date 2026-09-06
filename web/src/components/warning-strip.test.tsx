import { expect, test } from "vitest";
import { render, screen } from "@testing-library/react";
import { WarningStrip, WARNING_STRIP_TINT } from "./warning-strip";

/** Classes as tokens, so `bg-warning/8` can't be satisfied by a
 * substring match against something like `bg-warning/80`. */
function classes(el: HTMLElement): string[] {
  return el.className.split(/\s+/);
}

test("roomy renders the caller's own element and role, with the 3px inset and roomy padding", () => {
  render(
    <WarningStrip as="div" role="alert" size="roomy" className="shrink-0 border-b border-border">
      full-bleed
    </WarningStrip>,
  );

  const el = screen.getByRole("alert");
  expect(el.tagName).toBe("DIV");
  expect(classes(el)).toEqual(
    expect.arrayContaining([
      "bg-warning/8",
      "shadow-[inset_3px_0_0_var(--warning)]",
      "px-5",
      "py-2.5",
      "shrink-0",
      "border-b",
      "border-border",
    ]),
  );
  // Never the other size's inset or padding.
  expect(el.className).not.toContain("inset_2px");
  expect(el.className).not.toContain("px-2.5");
});

test("tight renders as an output, with the 2px inset and tight padding", () => {
  render(
    <WarningStrip as="output" size="tight" className="block font-mono">
      nested notice
    </WarningStrip>,
  );

  const el = screen.getByText("nested notice");
  expect(el.tagName).toBe("OUTPUT");
  expect(classes(el)).toEqual(
    expect.arrayContaining([
      "bg-warning/8",
      "shadow-[inset_2px_0_0_var(--warning)]",
      "px-2.5",
      "py-1.5",
      "block",
      "font-mono",
    ]),
  );
  // Never the other size's inset or padding.
  expect(el.className).not.toContain("inset_3px");
  expect(el.className).not.toContain("px-5");
});

// settings.tsx's save bar reuses this exact string in its own cn(...) call
// rather than rendering a WarningStrip (it's a status bar that gains
// warning emphasis when dirty, not a standalone warning notice) — pinned so
// a change here can't silently drift the save bar's tint out of sync with
// the roomy WarningStrip variant it's meant to match.
test("the exported tint constant is the roomy tone, byte for byte", () => {
  expect(WARNING_STRIP_TINT).toBe("bg-warning/8 shadow-[inset_3px_0_0_var(--warning)]");
});
