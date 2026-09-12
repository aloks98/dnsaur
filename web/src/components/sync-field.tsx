import { useId, type ReactNode } from "react";
import { useController, type Control } from "react-hook-form";
import { Button, Input, Label, NativeSelect, NativeSelectOption, cn } from "@e412/rnui-react";
import { useTSIGKeys } from "../hooks/use-tsig-keys";
import { useForgetReplica, useStopFollowing, useSyncStatus } from "../hooks/use-sync";
import { relativeTime } from "../lib/format";
import { rhfName } from "../lib/rhf-name";

/**
 * The Sync settings group — spec §8's band: who this instance follows, and
 * then whichever half of the relationship it is on.
 *
 * Rendered wholesale like the Protocols group (see protocols-field.tsx for
 * the same `control` wiring and why), and for the same reason: three of the
 * things on screen here are not settings at all. What has been applied, who
 * is registered, and whether a token is stored come from
 * `GET /sync/status`; only the five keys below are values SettingRow could
 * have rendered, and splitting the band would have put the facts somewhere
 * other than beside the fields they describe.
 *
 * The four "who do I follow" fields render on both roles. A main with an
 * empty peer URL is how every replica starts: typing a peer and a token and
 * saving is what makes this instance one, which is a door the band cannot
 * show only to instances that have already walked through it.
 *
 * Everything the operator might want explained — what a replica may still
 * change, what promotion leaves behind — is docs/dashboard.md's job.
 */

const PEER_URL = "sync.peer_url";
const TOKEN = "sync.token";
const INTERVAL = "sync.interval_seconds";
const PRIMARY_DNS = "sync.primary_dns";
const TSIG_KEY_ID = "sync.tsig_key_id";

/** One labelled control, with the key printed beside the label — the same
 * pairing settings.tsx's SettingRow makes, rebuilt here because this band
 * bypasses SettingRow (and so rnui's FormField context) exactly as
 * protocols-field.tsx does. */
function SyncRow({
  id,
  label,
  settingKey,
  children,
  note,
}: {
  id: string;
  label: string;
  settingKey: string;
  children: ReactNode;
  /** A fact about the stored value that the control itself cannot show. */
  note?: ReactNode;
}) {
  return (
    <div className="flex min-w-0 flex-col gap-2">
      <div className="flex items-baseline gap-2">
        <Label htmlFor={id}>{label}</Label>
        <span className="font-mono text-xs text-muted-foreground">{settingKey}</span>
      </div>
      {children}
      {note}
    </div>
  );
}

/** The replicas registered with this main, as they registered themselves:
 * their id, where they answer DNS, how far they have applied, and when they
 * last said so. Forgetting one is the operator's call — a replica that
 * stopped pulling goes stale and is never removed automatically — and it
 * takes that address back out of the implicit transfer allow. */
function ReplicaTable() {
  const status = useSyncStatus();
  const forget = useForgetReplica();
  const replicas = status.data?.replicas ?? [];

  if (replicas.length === 0) {
    return <p className="font-mono text-[11.5px] text-muted-foreground">No replicas registered.</p>;
  }

  return (
    <table className="w-full border-collapse border border-border font-mono text-xs">
      <thead>
        <tr className="bg-muted text-[9.5px] tracking-[0.12em] text-muted-foreground uppercase">
          <th className="px-2.5 py-1.5 text-left font-semibold">Instance</th>
          <th className="px-2.5 py-1.5 text-left font-semibold">DNS address</th>
          <th className="px-2.5 py-1.5 text-right font-semibold">Applied</th>
          <th className="px-2.5 py-1.5 text-left font-semibold">Last seen</th>
          <th className="px-2.5 py-1.5 text-right font-semibold">Actions</th>
        </tr>
      </thead>
      <tbody>
        {replicas.map((replica) => (
          <tr key={replica.instance_id} className="border-t border-border-muted">
            <td className="px-2.5 py-1.5">{replica.instance_id}</td>
            <td className="px-2.5 py-1.5">{replica.dns_addr}</td>
            <td className="px-2.5 py-1.5 text-right tabular-nums">{replica.version_applied}</td>
            <td
              className={cn("px-2.5 py-1.5", replica.stale && "text-warning-foreground")}
              // The strip above already names a stale replica; here the
              // colour is the whole of the marker, so the date carries it.
            >
              {relativeTime(replica.last_seen)}
            </td>
            <td className="px-2.5 py-1.5 text-right">
              {/* type="button": this band sits inside the settings form, and
                  a bare button in a form submits it. */}
              <Button
                type="button"
                size="sm"
                variant="ghost"
                aria-label={`Forget ${replica.instance_id}`}
                disabled={forget.isPending}
                onClick={() => forget.mutate(replica.instance_id)}
              >
                Forget
              </Button>
            </td>
          </tr>
        ))}
      </tbody>
    </table>
  );
}

export function SyncField({ control }: { control: Control<Record<string, string>> }) {
  const status = useSyncStatus();
  const keys = useTSIGKeys();
  const stopFollowing = useStopFollowing();

  const peerUrl = useController({ control, name: rhfName(PEER_URL) });
  const token = useController({ control, name: rhfName(TOKEN) });
  const interval = useController({ control, name: rhfName(INTERVAL) });
  const primaryDNS = useController({ control, name: rhfName(PRIMARY_DNS) });
  const tsigKeyID = useController({ control, name: rhfName(TSIG_KEY_ID) });

  const peerId = useId();
  const tokenId = useId();
  const intervalId = useId();
  const primaryDNSId = useId();
  const tsigKeyId = useId();

  const role = status.data?.role;
  // Read off what is *stored*, never off the field above it: a peer URL
  // being typed has no token behind it yet, and a line that flipped to
  // "set" as it was typed would be stating something untrue. Absent until
  // the status answers, for the same reason.
  const tokenLine = status.data ? (status.data.peer_url ? "Token: set" : "Token: not set") : null;

  // Destructured, then spread — the same shape protocols-field.tsx uses and
  // for the same reason: naming `.ref` in JSX is what tells React Compiler
  // this object is a ref, after which any other read of it during render
  // drops the component from memoization. The one field read here comes out
  // and goes back on explicitly.
  const { value: tsigKeyValue, ...tsigKeyRest } = tsigKeyID.field;
  const storedKeyID = Number(tsigKeyValue) || 0;
  const knownKeys = keys.data ?? [];
  const unlistedKey = storedKeyID !== 0 && !knownKeys.some((k) => k.id === storedKeyID);

  return (
    <div className="flex flex-col gap-3">
      <div className="grid grid-cols-2 gap-[14px]">
        <SyncRow id={peerId} label="Peer URL" settingKey={PEER_URL}>
          <Input
            id={peerId}
            {...peerUrl.field}
            placeholder="https://adam.dns.e412.in"
            autoComplete="off"
            spellCheck={false}
            className="font-mono text-xs"
          />
        </SyncRow>

        <SyncRow
          id={tokenId}
          label="Token"
          settingKey={TOKEN}
          note={
            tokenLine && (
              <span className="font-mono text-[11.5px] leading-[1.35] text-muted-foreground">
                {tokenLine}
              </span>
            )
          }
        >
          {/* Write-only: GET /settings never returns it, so the box starts
              empty on every load and an empty box means "leave it alone". */}
          <Input
            id={tokenId}
            {...token.field}
            type="password"
            autoComplete="off"
            spellCheck={false}
            className="font-mono text-xs"
          />
        </SyncRow>

        <SyncRow id={intervalId} label="Pull interval (seconds)" settingKey={INTERVAL}>
          <Input
            id={intervalId}
            {...interval.field}
            inputMode="numeric"
            autoComplete="off"
            className="w-32 font-mono text-xs"
          />
        </SyncRow>

        <SyncRow id={primaryDNSId} label="Primary DNS address" settingKey={PRIMARY_DNS}>
          <Input
            id={primaryDNSId}
            {...primaryDNS.field}
            placeholder="10.0.0.5:53"
            autoComplete="off"
            spellCheck={false}
            className="font-mono text-xs"
          />
        </SyncRow>
      </div>

      {role === "replica" && (
        <div className="flex flex-wrap items-center gap-3 border border-border px-3 py-[11px]">
          <span className="font-mono text-[11.5px] leading-[1.35] text-muted-foreground">
            applied {status.data?.applied_version ?? 0} of {status.data?.peer_version ?? 0}
          </span>
          <Button
            type="button"
            size="sm"
            variant="outline"
            className="ml-auto"
            disabled={stopFollowing.isPending}
            onClick={() => stopFollowing.mutate()}
          >
            Stop following
          </Button>
        </div>
      )}

      {role === "main" && (
        <>
          <SyncRow id={tsigKeyId} label="Sync key" settingKey={TSIG_KEY_ID}>
            {/* The key a replica's AXFR has to verify under before the
                implicit transfer allow lets it through. A stored id this
                list cannot name keeps an option of its own, so saving an
                unrelated field never silently unsigns every transfer — the
                same guard zones' own TSIG select makes. */}
            <NativeSelect
              id={tsigKeyId}
              {...tsigKeyRest}
              value={tsigKeyValue}
              className="w-[240px] font-mono text-xs"
            >
              <NativeSelectOption value="0">None</NativeSelectOption>
              {knownKeys.map((k) => (
                <NativeSelectOption key={k.id} value={String(k.id)}>
                  {k.name}
                </NativeSelectOption>
              ))}
              {unlistedKey && (
                <NativeSelectOption value={String(storedKeyID)}>#{storedKeyID}</NativeSelectOption>
              )}
            </NativeSelect>
          </SyncRow>
          <ReplicaTable />
        </>
      )}
    </div>
  );
}
