import { TriangleAlert } from "lucide-react";
import { useResolverStatus } from "../hooks/use-settings";
import { WarningStrip } from "./warning-strip";

/**
 * The one thing about the running server that isn't tied to any single
 * screen: dnsaur is resolving through its hardcoded plaintext resolvers
 * because the stored `upstreams` value — which asked for DNS-over-TLS or
 * DNS-over-HTTPS — would not parse.
 *
 * Mounted in the shell (see app-shell.tsx) rather than on any one page,
 * because the fact it reports is a fact about the server, not about
 * Settings. Someone who lands on Query Log or Zones and never opens
 * Settings still needs to learn every query is going out in the clear —
 * the query log itself shows answers arriving normally, and the only other
 * record is one ERROR line in a log nobody reads on a good day.
 *
 * Same treatment as the save bar on the Settings page — `--warning` with an
 * inset left bar — rather than `destructive`: nothing is broken, and DNS is
 * working. What is wrong is that it is working in a way the operator did
 * not ask for.
 *
 * It clears on its own: the server drops the state the moment a settings
 * apply installs a forwarder built from the stored value (see
 * useResolverStatus for how quickly this notices).
 */
export function EncryptionDowngradeBanner() {
  const status = useResolverStatus();
  if (!status.data?.encryption_downgraded) return null;

  return (
    <WarningStrip as="div" role="alert" size="roomy" className="shrink-0 border-b border-border">
      <p className="flex items-center gap-2 text-sm font-medium text-warning-foreground">
        <TriangleAlert className="size-4 shrink-0" aria-hidden />
        Encryption is off — <code className="font-mono">upstreams</code> could not be parsed,
        falling back to plaintext resolvers.
      </p>
      {status.data.reason !== "" && (
        <p className="mt-1 pl-6 font-mono text-xs break-words text-muted-foreground">
          {status.data.reason}
        </p>
      )}
    </WarningStrip>
  );
}
