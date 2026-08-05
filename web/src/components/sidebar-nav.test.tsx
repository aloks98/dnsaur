import type { ReactElement } from "react";
import { http, HttpResponse } from "msw";
import { toast } from "sonner";
import { afterEach, expect, test, vi } from "vitest";
import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import { SidebarProvider } from "@e412/rnui-react";
import { server } from "../test/msw-server";
import { renderWithProviders } from "../test/render";
import { SidebarNav } from "./sidebar-nav";

// Same base-ui-under-jsdom caveat as pause-control.test.tsx: userEvent's
// full pointer sequence trips outside-click detection and re-closes the
// menu, so open it with a plain click.
function openAccountMenu() {
  fireEvent.click(screen.getByRole("button", { name: /account menu/i }));
  const menu = document.querySelector('[data-slot="dropdown-menu-content"]');
  if (!menu) throw new Error("account menu did not open");
  return menu as HTMLElement;
}

function renderSidebar(ui: ReactElement = <SidebarNav />, { open = true } = {}) {
  return renderWithProviders(<SidebarProvider open={open}>{ui}</SidebarProvider>);
}

// useLogout finishes with a hard window.location.assign("/") (see
// hooks/use-auth.ts for why). jsdom exposes `assign` as a non-configurable
// own property, so it can't be spied — but `window.location` itself *is*
// configurable, so swap the whole object for the duration of the test.
let restoreLocation: (() => void) | null = null;

function stubNavigation() {
  const original = window.location;
  const assign = vi.fn<(url: string) => void>();
  Object.defineProperty(window, "location", {
    configurable: true,
    value: Object.assign(
      Object.create(null),
      { href: original.href, origin: original.origin },
      { assign },
    ),
  });
  restoreLocation = () =>
    Object.defineProperty(window, "location", { configurable: true, value: original });
  return assign;
}

afterEach(() => {
  restoreLocation?.();
  restoreLocation = null;
  vi.restoreAllMocks();
});

// lib/theme.ts is a module-level store, so whatever a previous test in this
// file toggled to is still in effect here — assert on the flip, not on a
// fixed starting theme.
function clickThemeToggle() {
  const wasDark = document.documentElement.classList.contains("dark");
  const toggle = screen.getByRole("button", {
    name: wasDark ? /switch to light theme/i : /switch to dark theme/i,
  });
  fireEvent.click(toggle);
  return wasDark;
}

test("renders the primary nav and no marketing tagline", async () => {
  renderSidebar();
  expect(screen.getByRole("link", { name: /query log/i })).toBeInTheDocument();
  expect(screen.queryByText(/observability/i)).not.toBeInTheDocument();
});

test("the footer carries the account menu, with the signed-in username", async () => {
  renderSidebar();

  const trigger = await screen.findByRole("button", { name: /account menu \(admin\)/i });
  // The avatar's initials come from the useMe layer, not from the component.
  expect(within(trigger).getByText("AD")).toBeInTheDocument();
  expect(within(trigger).getByText("admin")).toBeInTheDocument();

  const menu = openAccountMenu();
  expect(within(menu).getByText("admin")).toBeInTheDocument();
  expect(within(menu).getByText(/log out/i)).toBeInTheDocument();
});

test("logging out from the footer posts /auth/logout and leaves for the login screen", async () => {
  const assign = stubNavigation();
  let logoutCalled = false;
  server.use(
    http.post("/api/v1/auth/logout", () => {
      logoutCalled = true;
      return new HttpResponse(null, { status: 204 });
    }),
  );

  renderSidebar();
  await screen.findByRole("button", { name: /account menu \(admin\)/i });

  fireEvent.click(within(openAccountMenu()).getByText(/log out/i));

  await waitFor(() => expect(logoutCalled).toBe(true));
  await waitFor(() => expect(assign).toHaveBeenCalledWith("/"));
});

test("a 401 from logout is a completed logout, not an error toast", async () => {
  const assign = stubNavigation();
  const errorSpy = vi.spyOn(toast, "error");
  server.use(
    http.post("/api/v1/auth/logout", () =>
      HttpResponse.json({ error: "authentication required" }, { status: 401 }),
    ),
  );

  renderSidebar();
  await screen.findByRole("button", { name: /account menu \(admin\)/i });

  fireEvent.click(within(openAccountMenu()).getByText(/log out/i));

  await waitFor(() => expect(assign).toHaveBeenCalledWith("/"));
  expect(errorSpy).not.toHaveBeenCalled();
});

test("a real logout failure keeps the user put and says so", async () => {
  const assign = stubNavigation();
  const errorSpy = vi.spyOn(toast, "error");
  server.use(
    http.post("/api/v1/auth/logout", () => HttpResponse.json({ error: "boom" }, { status: 500 })),
  );

  renderSidebar();
  await screen.findByRole("button", { name: /account menu \(admin\)/i });

  fireEvent.click(within(openAccountMenu()).getByText(/log out/i));

  await waitFor(() => expect(errorSpy).toHaveBeenCalledWith("Couldn't sign out — try again"));
  expect(assign).not.toHaveBeenCalled();
});

test("the footer's theme toggle switches themes", async () => {
  renderSidebar();

  const wasDark = clickThemeToggle();

  expect(document.documentElement.classList.contains("dark")).toBe(!wasDark);
  expect(
    screen.getByRole("button", {
      name: wasDark ? /switch to dark theme/i : /switch to light theme/i,
    }),
  ).toBeInTheDocument();
});

test("the footer keeps exactly one status dot, for resolver health", async () => {
  renderSidebar();
  await waitFor(() =>
    expect(document.querySelectorAll('[data-slot="status-indicator"]')).toHaveLength(1),
  );
  expect(await screen.findByText("Resolving")).toBeInTheDocument();
});

test("collapsed to the icon rail, the account menu and theme toggle stay usable and labelled", async () => {
  renderSidebar(<SidebarNav />, { open: false });

  expect(document.querySelector('[data-slot="sidebar"]')).toHaveAttribute(
    "data-collapsible",
    "icon",
  );

  // Both are still reachable by their accessible names — the rail clips the
  // text, it does not drop the label.
  const account = await screen.findByRole("button", { name: /account menu \(admin\)/i });

  const wasDark = clickThemeToggle();
  expect(document.documentElement.classList.contains("dark")).toBe(!wasDark);

  fireEvent.click(account);
  const menu = document.querySelector('[data-slot="dropdown-menu-content"]');
  expect(menu).not.toBeNull();
  expect(within(menu as HTMLElement).getByText(/log out/i)).toBeInTheDocument();
});
