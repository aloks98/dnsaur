import type { ReactNode } from "react";
import { cn } from "@e412/rnui-react";
import { DnsaurLogo } from "./dnsaur-logo";

interface AuthLayoutProps {
  /** Small uppercase label above the heading, e.g. "First-time setup". */
  eyebrow: string;
  title: string;
  /**
   * Small print below the card. Screen-specific rather than fixed: the thing
   * worth saying under a password form ("which build is this?") is not the
   * thing worth saying under a 2FA prompt ("what if I lost my phone?").
   */
  footer?: ReactNode;
  /** Tailwind max-width class for the centered column. Defaults to a card-sized column. */
  maxWidthClassName?: string;
  children: ReactNode;
}

/**
 * Shared full-screen chrome for the unauthenticated screens (Setup, Login):
 * a centered column over a soft radial gradient, with a small eyebrow label
 * and heading above the page's own content (a Card, typically). Keeping
 * this in one place is what makes Setup and Login read as one system rather
 * than two independently-styled screens.
 */
export function AuthLayout({
  eyebrow,
  title,
  footer,
  maxWidthClassName = "max-w-md",
  children,
}: AuthLayoutProps) {
  return (
    <main className="relative isolate flex min-h-screen items-center justify-center overflow-hidden bg-background p-6">
      <div
        aria-hidden
        className="pointer-events-none absolute inset-0 -z-10 bg-radial-[at_50%_15%] from-primary/12 via-background to-background"
      />

      <div className={cn("flex w-full flex-col gap-4", maxWidthClassName)}>
        <div className="flex flex-col items-center gap-2 text-center">
          {/* The mark's one moment at full size. First run is the only time
              an operator sees dnsaur before any chrome exists, so the brand
              is the tile itself here, not the 22px cell it becomes in the
              top bar. It follows the live theme on its own. */}
          <DnsaurLogo size={46} className="mb-1" />
          {/* Mono, like every other label in the app that is a tag rather
              than prose — the chrome's nav cells, the table headers. */}
          <span className="font-mono text-xs font-medium tracking-widest text-muted-foreground uppercase">
            {eyebrow}
          </span>
          <h1 className="font-heading text-2xl font-bold tracking-tight text-foreground">
            {title}
          </h1>
        </div>

        {children}

        {footer && <p className="text-center font-mono text-xs text-muted-foreground">{footer}</p>}
      </div>
    </main>
  );
}
