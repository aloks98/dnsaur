import { useId, useState, type KeyboardEvent, type ReactNode } from "react";
import { Plus, X } from "lucide-react";
import { toast } from "sonner";
import { useController, type Control } from "react-hook-form";
import {
  Accordion,
  AccordionContent,
  AccordionItem,
  AccordionTrigger,
  Button,
  Input,
  Label,
  cn,
} from "@e412/rnui-react";
import {
  useFollow,
  useForgetReplica,
  usePairingCode,
  useStopFollowing,
  useSyncStatus,
} from "../hooks/use-sync";
import { relativeTime } from "../lib/format";
import { rhfName } from "../lib/rhf-name";

/**
 * The Sync settings group — spec §8's band: who this instance follows, or
 * who follows it.
 *
 * Rendered wholesale like the Protocols group (see protocols-field.tsx for
 * the same `control` wiring and why), and for a stronger reason: almost
 * nothing on screen here is a setting. The role, the registered replicas,
 * how far a replica has applied and whether its peer is plaintext all come
 * from `GET /sync/status`; the pairing code comes from a POST and is never
 * readable again; the peer URL and the pull secret are written by pairing
 * (`POST /sync/follow`) and cleared together by promotion. Two keys are
 * left for the form, and both are adjustments rather than the point of the
 * band, so both sit under ADVANCED.
 *
 * Everything the operator might want explained — what a replica may still
 * change, what promotion leaves behind — is docs/dashboard.md's job.
 */

const INTERVAL = "sync.interval_seconds";
const PRIMARY_DNS = "sync.primary_dns";
/** The Accordion's one item. A constant so the controlled value and the item
 * cannot drift apart. */
const ADVANCED_ITEM = "advanced";

/** The band's left-hand line, which is a different fact on each role.
 * Exported because that column is settings.tsx's to render — see
 * SettingGroup.description. */
export function SyncDescription() {
  const status = useSyncStatus();
  return status.data?.role === "replica"
    ? "Configuration is pulled from the main."
    : "Replicas that pull this instance's configuration.";
}

/** One labelled control, with the key printed beside the label — the same
 * pairing settings.tsx's SettingRow makes, rebuilt here because this band
 * bypasses SettingRow (and so rnui's FormField context) exactly as
 * protocols-field.tsx does. */
function SyncRow({
  id,
  label,
  settingKey,
  dirty,
  error,
  children,
  note,
}: {
  id: string;
  label: string;
  settingKey?: string;
  /** Unsaved, for this field — the same dot SettingRow puts at the end of
   * its label line (settings.tsx). */
  dirty?: boolean;
  /** The schema's rejection, if the form has one for this field. Rendered
   * under the control and pointed at by its `aria-describedby` — the call
   * site owns the control, so it spells the id the same way (`errorId`). */
  error?: string;
  children: ReactNode;
  /** A fact about the value that the control itself cannot show. */
  note?: ReactNode;
}) {
  return (
    <div className="flex min-w-0 flex-col gap-1.5">
      <div className="flex items-baseline gap-2">
        <Label htmlFor={id}>{label}</Label>
        {settingKey && (
          <span className="font-mono text-xs text-muted-foreground">{settingKey}</span>
        )}
        {dirty && <span aria-hidden className="ml-auto size-1.5 bg-warning" />}
      </div>
      {children}
      {error && (
        <p id={errorId(id)} className="text-xs text-destructive-foreground">
          {error}
        </p>
      )}
      {note}
    </div>
  );
}

/** The id SyncRow gives a field's message, so the control can name it. */
function errorId(id: string): string {
  return `${id}-error`;
}

/** The replicas paired with this main, as the main knows them: their id,
 * where they answer DNS, how far they have applied, and when they last
 * probed. A replica that stopped probing goes stale and is never removed
 * automatically, so forgetting one is the operator's call — and it takes
 * that address back out of the implicit transfer allow. */
function ReplicaTable() {
  const status = useSyncStatus();
  const forget = useForgetReplica();
  const replicas = status.data?.replicas ?? [];

  if (replicas.length === 0) {
    return <p className="font-mono text-[11.5px] text-muted-foreground">No replicas registered.</p>;
  }

  return (
    <table className="w-full border-collapse border border-border font-mono text-[12.5px]">
      <thead>
        <tr className="border-b border-border bg-muted text-[9.5px] tracking-[0.12em] text-muted-foreground uppercase">
          <th className="px-2.5 py-1.5 text-left font-semibold">Instance</th>
          <th className="w-[176px] px-2.5 py-1.5 text-left font-semibold">DNS address</th>
          <th className="w-24 px-2.5 py-1.5 text-left font-semibold">Applied</th>
          <th className="w-32 px-2.5 py-1.5 text-left font-semibold">Last seen</th>
          <th className="w-[72px] px-2.5 py-1.5">
            {/* The board leaves this header blank; the name is still owed to
                anyone reading the table a cell at a time. */}
            <span className="sr-only">Actions</span>
          </th>
        </tr>
      </thead>
      <tbody>
        {replicas.map((replica) => (
          <tr key={replica.instance_id} className="border-b border-border-muted">
            <td className="px-2.5 py-1.5 font-medium">{replica.instance_id}</td>
            <td className="px-2.5 py-1.5">{replica.dns_addr}</td>
            <td className="px-2.5 py-1.5 tabular-nums">v{replica.version_applied}</td>
            {/* A stale replica is marked here and nowhere else: the row is
                not tinted and the id gains no badge, because the fact being
                reported is exactly "this is how long it has been quiet". */}
            <td className={cn("px-2.5 py-1.5", replica.stale && "text-muted-foreground")}>
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
                onClick={() =>
                  forget.mutate(replica.instance_id, {
                    // The row going away is the whole of the success
                    // signal, so a refusal that said nothing would look
                    // exactly like the moment before one that worked.
                    onError: () =>
                      toast.error(`Couldn't forget ${replica.instance_id} — try again`),
                  })
                }
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

/**
 * A main: who is paired with this box, and the one-time code that pairs the
 * next one.
 *
 * The code is held by the mutation and nothing else (see usePairingCode).
 * It is shown once — there is no endpoint that reads one back — so it is
 * dismissed by the ×, replaced by a second press, and gone the moment this
 * screen unmounts.
 */
function MainState() {
  const pairing = usePairingCode();
  const code = pairing.data?.code;

  return (
    <>
      <div className="flex items-center gap-2">
        <span className="text-[12.5px] font-medium">Replicas</span>
        <span className="font-mono text-xs text-muted-foreground">sync.role = main</span>
        <Button
          type="button"
          size="sm"
          variant="outline"
          className="ml-auto"
          disabled={pairing.isPending}
          onClick={() =>
            pairing.mutate(undefined, {
              onError: () => toast.error("Couldn't get a pairing code — try again"),
            })
          }
        >
          <Plus />
          Add replica
        </Button>
      </div>

      {code !== undefined && (
        <div className="grid grid-cols-[1fr_auto] items-start gap-4 border border-border bg-card px-4 py-3.5 shadow-[inset_3px_0_0_var(--primary)]">
          <div className="flex min-w-0 flex-col gap-2">
            <span className="font-mono text-[9.5px] font-semibold tracking-[0.14em] text-muted-foreground">
              PAIRING CODE · ONE-TIME
            </span>
            <span className="font-mono text-[28px] leading-none font-semibold tracking-[0.14em]">
              {code}
            </span>
            <span className="flex flex-wrap items-baseline gap-2.5 text-[11.5px] text-muted-foreground">
              <span>Expires in 10 minutes</span>
              <span>· Enter it on the replica under Settings › Sync.</span>
            </span>
          </div>
          <Button
            type="button"
            size="sm"
            variant="ghost"
            aria-label="Dismiss"
            onClick={() => pairing.reset()}
          >
            <X />
          </Button>
        </div>
      )}

      <ReplicaTable />
    </>
  );
}

/**
 * The door out of being a main, and the only one: a box with no peer is a
 * main, so this is what every replica starts as.
 *
 * The two boxes are not form fields. `POST /sync/follow` writes
 * `sync.peer_url` and the secret it gets back in one settings write of its
 * own, and the code is spent rather than stored — so there is nothing here
 * for the Save bar to carry, and nothing to leave unsaved.
 */
function FollowRow() {
  const follow = useFollow();
  const [peerUrl, setPeerUrl] = useState("");
  const [code, setCode] = useState("");
  const peerId = useId();
  const codeId = useId();
  const failureId = useId();
  // Both boxes, because the server does not say which of the two it
  // rejected — a bad URL, a spent code and an unreachable peer all land here.
  const describedBy = follow.error ? failureId : undefined;

  function onFollow() {
    follow.mutate({ peer_url: peerUrl.trim(), code: code.trim() });
  }

  // Enter belongs to this pair of boxes, not to the settings form they sit
  // inside — which would otherwise save unrelated fields and pair nothing.
  function onKeyDown(event: KeyboardEvent<HTMLInputElement>) {
    if (event.key !== "Enter") return;
    event.preventDefault();
    onFollow();
  }

  return (
    <>
      <div className="grid grid-cols-[1fr_200px_auto] items-end gap-3.5">
        <SyncRow id={peerId} label="Peer URL" settingKey="sync.peer_url">
          <Input
            id={peerId}
            value={peerUrl}
            onChange={(e) => setPeerUrl(e.target.value)}
            onKeyDown={onKeyDown}
            aria-invalid={Boolean(follow.error)}
            aria-describedby={describedBy}
            placeholder="https://adam.dns.e412.in"
            autoComplete="off"
            spellCheck={false}
            className="font-mono text-xs"
          />
        </SyncRow>
        <SyncRow id={codeId} label="Pairing code">
          <Input
            id={codeId}
            value={code}
            onChange={(e) => setCode(e.target.value)}
            onKeyDown={onKeyDown}
            aria-invalid={Boolean(follow.error)}
            aria-describedby={describedBy}
            placeholder="XXXX-XXXX"
            autoComplete="off"
            spellCheck={false}
            className="font-mono text-xs tracking-[0.1em]"
          />
        </SyncRow>
        <Button type="button" size="sm" disabled={follow.isPending} onClick={onFollow}>
          Follow
        </Button>
      </div>
      <p className="text-[11.5px] text-muted-foreground">
        The code is shown once on the main, under Settings › Sync › Add replica.
      </p>
      {/* Verbatim, and next to the code that was typed: a refused code, an
          unreachable peer and a malformed URL are three different things to
          do about it, and only the server knows which happened.

          `role="alert"`, because the press that failed changes nothing else
          on screen — the boxes keep what was typed and no row appears, so a
          line only a sighted reader finds is a press that reported nothing. */}
      {follow.error && (
        <p id={failureId} role="alert" className="text-[11.5px] text-destructive-foreground">
          {follow.error.message}
        </p>
      )}
    </>
  );
}

/** A replica: the peer, how far it has got, and the way back to being a
 * main. */
function FollowingState() {
  const status = useSyncStatus();
  const stopFollowing = useStopFollowing();
  const sync = status.data;
  const failed = Boolean(sync?.last_error);

  return (
    <div className="grid grid-cols-[1fr_auto] items-start gap-4">
      <div className="flex min-w-0 flex-col gap-1.5">
        <p className="flex items-center gap-2 text-[13.5px] font-medium">
          <span
            aria-hidden
            className={cn("size-[7px] shrink-0", failed ? "bg-destructive" : "bg-success")}
          />
          Following <span className="font-mono text-[12.5px]">{sync?.peer_url}</span>
        </p>
        <p className="pl-[15px] font-mono text-[11.5px] text-muted-foreground">
          applied {sync?.applied_version ?? 0} of {sync?.peer_version ?? 0} · last pull{" "}
          {relativeTime(sync?.last_pull_at ?? 0)}
        </p>
        {/* The reason verbatim. A failed pull leaves the configuration that
            was already applied in force, so this is not a DNS outage. */}
        {failed && (
          <p className="pl-[15px] text-[11.5px] text-destructive-foreground">
            Last pull failed: {sync?.last_error}
          </p>
        )}
        {/* Beside the peer URL rather than in the status panel: it is a
            reading of that URL and nothing else can change it. */}
        {sync?.plain_http && (
          <p className="pl-[15px] text-[11.5px] text-muted-foreground">
            Peer reached over plain HTTP
          </p>
        )}
      </div>
      <Button
        type="button"
        size="sm"
        variant="outline"
        disabled={stopFollowing.isPending}
        onClick={() =>
          // Promotion shows up as the rest of the dashboard coming back to
          // life, which is not instant — so a refusal has to say so rather
          // than leave the operator waiting for it.
          stopFollowing.mutate(undefined, {
            onError: () => toast.error("Couldn't stop following — try again"),
          })
        }
      >
        Stop following
      </Button>
    </div>
  );
}

/** The two keys that are still settings. Closed by default: neither is a
 * thing anyone sets up a pair by changing, and the defaults are the answer
 * on almost every box. */
function Advanced({
  control,
  isReplica,
}: {
  control: Control<Record<string, string>>;
  isReplica: boolean;
}) {
  const [open, setOpen] = useState(false);
  const interval = useController({ control, name: rhfName(INTERVAL) });
  const primaryDNS = useController({ control, name: rhfName(PRIMARY_DNS) });
  const intervalId = useId();
  const primaryDNSId = useId();

  const intervalError = interval.fieldState.error?.message;
  // Only when it is on screen: a main's override is registered but not
  // rendered, and a message for a control nobody can reach is worse than
  // none.
  const overrideError = isReplica ? primaryDNS.fieldState.error?.message : undefined;
  // A rejected value under a closed disclosure is a save that failed for no
  // stated reason, so its own field's error is what opens this.
  const shown = open || intervalError !== undefined || overrideError !== undefined;

  return (
    // rnui's Accordion rather than a hand-rolled button and a boolean: it is
    // in the package already, and it brings the header/trigger semantics, the
    // chevron and the panel transition with it. Controlled, because the
    // disclosure has to be able to open on something other than a press.
    <Accordion
      className="border-t border-border-muted pt-2"
      value={shown ? [ADVANCED_ITEM] : []}
      onValueChange={(next) => setOpen(next.includes(ADVANCED_ITEM))}
    >
      <AccordionItem value={ADVANCED_ITEM}>
        <AccordionTrigger className="py-1 font-mono text-[9.5px] font-semibold tracking-[0.14em] text-muted-foreground uppercase">
          Advanced
        </AccordionTrigger>
        {/* The same two-column grid a settings group gets (see SettingsForm),
            so a label has the whole column to sit on and "Pull interval
            (seconds)" stops wrapping into three lines. On a main the
            interval is the only field here and takes the first column. */}
        <AccordionContent className="grid items-start gap-5 pt-1 sm:grid-cols-2">
          <SyncRow
            id={intervalId}
            label="Pull interval (seconds)"
            settingKey={INTERVAL}
            error={intervalError}
            dirty={interval.fieldState.isDirty}
          >
            <Input
              id={intervalId}
              {...interval.field}
              inputMode="numeric"
              autoComplete="off"
              aria-invalid={intervalError !== undefined}
              aria-describedby={intervalError === undefined ? undefined : errorId(intervalId)}
              className="font-mono text-xs"
            />
          </SyncRow>

          {/* Only a replica has a peer whose host it would otherwise derive
              the main's DNS address from. */}
          {isReplica && (
            <SyncRow
              id={primaryDNSId}
              label="Primary DNS address (override)"
              settingKey={PRIMARY_DNS}
              error={overrideError}
              dirty={primaryDNS.fieldState.isDirty}
              note={
                <span className="text-[11px] text-muted-foreground">
                  Leave empty to use the address the main advertises.
                </span>
              }
            >
              <Input
                id={primaryDNSId}
                {...primaryDNS.field}
                placeholder="192.168.150.1:53"
                autoComplete="off"
                spellCheck={false}
                aria-invalid={overrideError !== undefined}
                aria-describedby={overrideError === undefined ? undefined : errorId(primaryDNSId)}
                className="font-mono text-xs"
              />
            </SyncRow>
          )}
        </AccordionContent>
      </AccordionItem>
    </Accordion>
  );
}

export function SyncField({ control }: { control: Control<Record<string, string>> }) {
  const status = useSyncStatus();
  const isReplica = status.data?.role === "replica";

  return (
    <div className="flex flex-col gap-2.5">
      {isReplica ? (
        <FollowingState />
      ) : (
        <>
          <MainState />
          {/* The boards draw "not following" as its own screen; the server
              has no such role — an instance with no peer *is* a main — so
              the way to become a replica sits below the registry rather
              than instead of it. */}
          <div className="flex flex-col gap-2.5 border-t border-border-muted pt-2.5">
            <FollowRow />
          </div>
        </>
      )}
      <Advanced control={control} isReplica={isReplica} />
    </div>
  );
}
