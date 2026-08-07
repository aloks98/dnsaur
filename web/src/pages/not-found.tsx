import { Link } from "react-router";
import { Button } from "@e412/rnui-react";

/**
 * The catch-all screen.
 *
 * `path="*"` used to redirect silently to the dashboard, which is why a
 * mistyped or dead URL read as the app ignoring you: the address bar
 * rewrote itself under the cursor and nothing ever said what had happened.
 * A wrong link and a working one were indistinguishable.
 *
 * It renders inside the shell so the nav stays usable, but with no group
 * marked and no second row — an unknown path belongs to no section, and
 * marking one would point at a page you are not on. See lib/nav.ts's
 * findActiveGroup.
 */
export function NotFound() {
  return (
    <div className="flex flex-1 flex-col items-center justify-center gap-3.5 py-10">
      {/* Texture, not content. The heading below carries the meaning, so
          this is hidden from assistive tech rather than announced as a bare
          "404" ahead of it. */}
      <span
        aria-hidden="true"
        className="font-mono text-8xl leading-none font-semibold tracking-widest text-muted-foreground/40 select-none"
      >
        404
      </span>

      <div className="flex max-w-md flex-col items-center gap-2">
        {/* NXDOMAIN is the DNS response code for a name that does not exist.
            On a DNS server's own admin UI it is the exact word for this, and
            the one an operator already knows — a joke that is also correct. */}
        <h1 className="font-heading text-2xl font-bold tracking-tight">NXDOMAIN</h1>
        <p className="text-center text-sm text-pretty text-muted-foreground">
          This one&apos;s on the app, not the resolver.
        </p>
      </div>

      <Button size="sm" render={<Link to="/" />}>
        Back to dashboard
      </Button>
    </div>
  );
}
