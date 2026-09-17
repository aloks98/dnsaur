import type { ReactNode } from "react";
import { TriangleAlert } from "lucide-react";
import { toast } from "sonner";
import { Alert, AlertDescription, AlertTitle, Button, cn, Skeleton } from "@e412/rnui-react";
import { ApiError } from "../../api/client";
import type { DHCPLease } from "../../api/types";
import { useLeases, useReleaseLease, useReserveLease, useScopes } from "../../hooks/use-dhcp";
import { useManagedBy } from "../../hooks/use-sync";
import { ManagedNotice } from "../../components/managed-notice";
import { StaleDataAlert } from "../../components/stale-data-alert";
import { formatDuration } from "../../lib/format";

/** The board's six columns, head and rows alike. */
const GRID = "grid grid-cols-[200px_190px_1fr_130px_130px_150px] items-center gap-3.5 px-4";

/** What a row with no expiry shows. `expires_at: 0` is a reservation
 * nothing has leased yet — the operator pinned the address and no client
 * has asked for it — which is a row with no countdown, not a row expiring
 * now. The app's own "nothing here" glyph rather than a sentence. */
const NO_EXPIRY = "—";

/**
 * "22h 3m" until the lease lapses, or the em dash for a reservation nothing
 * has leased. Shared `formatDuration`, so this column reads the same way
 * the zones screen's next-transfer column does rather than inventing a
 * second duration grammar.
 */
export function expiresIn(expiresAt: number, now: number = Date.now()): string {
  if (expiresAt === 0) return NO_EXPIRY;
  return formatDuration(expiresAt - now);
}

function LeaseRow({
  lease,
  scopeName,
  orphaned,
  managed,
}: {
  lease: DHCPLease;
  scopeName: string;
  /** No scope has this row's `scope_id` — a lease the engine still holds
   * for a subnet dnsaur no longer configures. */
  orphaned: boolean;
  managed: boolean;
}) {
  const release = useReleaseLease();
  const reserve = useReserveLease();
  // A reservation nothing has leased: the operator pinned the address and no
  // client has asked for it, so there is nothing to hand back — the engine
  // answers lease4-del with "no such lease" (§8.3).
  const nothingToRelease = lease.expires_at === 0;

  return (
    <div data-testid="lease-row" className={cn(GRID, "border-b border-border-muted py-1.5")}>
      <span className="flex min-w-0 items-center gap-2">
        <span className="truncate font-mono text-[13px]">{lease.ip}</span>
        {lease.reserved && (
          <span className="shrink-0 border border-border px-1.5 py-0.5 font-mono text-[9px] leading-none tracking-[0.1em] text-muted-foreground uppercase">
            reserved
          </span>
        )}
      </span>
      <span className="truncate font-mono text-[13px] text-muted-foreground">{lease.mac}</span>
      {/* A device that offered no name at all — no option 12, no FQDN, and
          no reservation to borrow one from. Italic and muted so the column
          still scans as names rather than as one more value. */}
      <span
        className={cn(
          "truncate text-[13px]",
          lease.hostname ? "font-medium" : "text-muted-foreground italic",
        )}
      >
        {lease.hostname || "no hostname"}
      </span>
      <span className={cn("truncate text-[12.5px]", orphaned && "text-muted-foreground italic")}>
        {orphaned ? "deleted scope" : scopeName}
      </span>
      <span className="truncate text-right font-mono text-[13px]">
        {expiresIn(lease.expires_at)}
      </span>
      <span className="flex items-center justify-end gap-0.5">
        {/* Creating a reservation is a configuration write, so a replica
            cannot, and neither can anyone under a scope that is gone — the
            reservation would have to belong to it. Releasing one is not a
            configuration write: a lease belongs to the engine, and Kea's HA
            propagates the release — see hooks/use-dhcp.ts. */}
        <Button
          type="button"
          size="sm"
          variant="ghost"
          disabled={managed || orphaned || lease.reserved || reserve.isPending}
          onClick={() =>
            reserve.mutate(lease.ip, {
              onError: (err) =>
                toast.error(err instanceof ApiError ? err.message : "Couldn't reserve"),
            })
          }
        >
          Reserve
        </Button>
        <Button
          type="button"
          size="sm"
          variant="ghost"
          disabled={nothingToRelease || release.isPending}
          onClick={() =>
            release.mutate(lease.ip, {
              onError: (err) =>
                toast.error(err instanceof ApiError ? err.message : "Couldn't release"),
            })
          }
        >
          Release
        </Button>
      </span>
    </div>
  );
}

/**
 * DHCP › Leases. The engine's own table as of the last poll, refreshed on
 * the same interval the server rebuilds it (hooks/use-dhcp.ts), with the two
 * per-row actions.
 *
 * Reservations nothing has leased yet are in this list too, with no expiry:
 * a reserved device has a name and an address before its first lease, and
 * leaving those rows out would make the page disagree with the Reservations
 * screen about which devices exist.
 */
export function DHCPLeases() {
  const leases = useLeases();
  const scopes = useScopes();
  const managed = useManagedBy() !== "";

  const rows = leases.data ?? [];
  const reserved = rows.filter((l) => l.reserved).length;
  const names = new Map((scopes.data ?? []).map((s) => [s.id, s.name]));

  let body: ReactNode;
  if (leases.isPending) {
    body = (
      <div className="flex flex-col gap-2 p-4" aria-hidden="true">
        {Array.from({ length: 6 }).map((_, i) => (
          <Skeleton key={i} className="h-7 w-full" />
        ))}
      </div>
    );
  } else if (leases.data === undefined) {
    body = (
      <div className="p-4">
        <Alert variant="destructive">
          <TriangleAlert />
          <AlertTitle>Couldn&apos;t load leases</AlertTitle>
          <AlertDescription>Try refreshing the page.</AlertDescription>
        </Alert>
      </div>
    );
  } else if (rows.length === 0) {
    body = (
      <div className="flex h-full flex-col items-center justify-center gap-2.5 p-8 text-center">
        <p className="font-heading text-[15px] font-semibold">No leases yet</p>
        <p className="text-[12.5px] text-muted-foreground">
          Rows appear as clients ask for addresses.
        </p>
      </div>
    );
  } else {
    body = rows.map((lease) => (
      <LeaseRow
        key={lease.ip}
        lease={lease}
        scopeName={names.get(lease.scope_id) ?? ""}
        // Only once the scopes have actually loaded: an empty map while that
        // request is in flight would call every row's scope deleted.
        orphaned={scopes.data !== undefined && !names.has(lease.scope_id)}
        managed={managed}
      />
    ));
  }

  return (
    <div className="flex h-full min-h-0 flex-col overflow-x-auto">
      <div className="flex shrink-0 items-center gap-2.5 border-b border-border px-4 py-2.5">
        <span className="font-heading text-[13.5px] leading-none font-semibold tracking-tight">
          Leases
        </span>
        <span className="font-mono text-[10.5px] text-muted-foreground">
          {rows.length} {rows.length === 1 ? "lease" : "leases"}
          {rows.length > 0 && ` · ${reserved} reserved`}
        </span>
        {/* Release stays live here, so the notice qualifies Reserve alone —
            and it is still the honest line: everything this screen can
            change about the *configuration* belongs to the main. */}
        {managed && (
          <span className="ml-auto">
            <ManagedNotice />
          </span>
        )}
      </div>

      {leases.isError && leases.data !== undefined && (
        <div className="shrink-0 border-b border-border p-3">
          <StaleDataAlert
            what="leases"
            onRetry={() => void leases.refetch()}
            isRetrying={leases.isFetching}
          />
        </div>
      )}

      <div
        className={cn(
          GRID,
          "shrink-0 border-b border-border py-2",
          "font-mono text-[9.5px] leading-none font-medium tracking-[0.14em] text-muted-foreground uppercase",
        )}
      >
        <span>Address</span>
        <span>MAC</span>
        <span>Hostname</span>
        <span>Scope</span>
        <span className="text-right">Expires in</span>
        <span className="text-right">Actions</span>
      </div>

      <div className="min-h-0 flex-1 overflow-y-auto">{body}</div>
    </div>
  );
}
