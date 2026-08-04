import type { ReactNode } from "react";
import { cn } from "@e412/rnui-react";

interface AuthLayoutProps {
  /** Small uppercase label above the heading, e.g. "First-time setup". */
  eyebrow: string;
  title: string;
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
  maxWidthClassName = "max-w-md",
  children,
}: AuthLayoutProps) {
  return (
    <main className="relative isolate flex min-h-screen items-center justify-center overflow-hidden bg-background p-6">
      <div
        aria-hidden
        className="pointer-events-none absolute inset-0 -z-10 bg-radial-[at_50%_15%] from-primary/12 via-background to-background"
      />

      <div className={cn("w-full", maxWidthClassName)}>
        <div className="mb-8 flex flex-col items-center gap-2 text-center">
          <span className="text-xs font-medium tracking-[0.14em] text-muted-foreground uppercase">
            {eyebrow}
          </span>
          <h1 className="text-[1.75rem] font-heading font-semibold tracking-tight text-foreground">
            {title}
          </h1>
        </div>

        {children}
      </div>
    </main>
  );
}
