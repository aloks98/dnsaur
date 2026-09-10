import { useEffect, useState, type ReactNode } from "react";
import {
  Eye,
  KeyRound,
  Pencil,
  Plus,
  ShieldCheck,
  TriangleAlert,
  type LucideIcon,
  Copy,
  Lock,
  X,
} from "lucide-react";
import { toast } from "sonner";
import { useForm, type Control } from "react-hook-form";
import { zodResolver } from "@hookform/resolvers/zod";
import { z } from "zod";
import {
  Alert,
  AlertDescription,
  AlertTitle,
  Badge,
  Button,
  Dialog,
  DialogClose,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
  cn,
  CopyButton,
  EmptyState,
  Form,
  FormControl,
  FormDescription,
  FormField,
  FormItem,
  FormLabel,
  FormMessage,
  Input,
  NativeSelect,
  NativeSelectOption,
  InputOTP,
  InputOTPGroup,
  InputOTPSeparator,
  InputOTPSlot,
  Skeleton,
} from "@e412/rnui-react";
import { ApiError } from "../api/client";
import type { ApiToken } from "../api/types";
import { useMe } from "../hooks/use-auth";
import { useCreateToken, useRevokeToken, useTokens } from "../hooks/use-tokens";
import {
  useTotpConfirm,
  useTotpDisable,
  useTotpStart,
  type TotpStartResult,
} from "../hooks/use-totp";
import { relativeTime } from "../lib/format";
import { requiredText, totpCodeSchema } from "../lib/schemas";
import { StaleDataAlert } from "../components/stale-data-alert";
import { ConfirmDeleteDialog } from "./dialogs";

// --- shared: copyable secret/token block ------------------------------

/**
 * A monospace, break-anywhere block for a value the admin needs to copy
 * verbatim (a TOTP setup key or a freshly minted API token) — paired with
 * rnui's CopyButton. `select-all` (CSS `user-select: all`) means a
 * click-drag or triple-click grabs the whole value in one go rather than a
 * partial selection, a small assist for anyone copying by hand instead of
 * the button, without needing a custom click handler.
 */
function CopyableCode({ value, label }: { value: string; label: string }) {
  return (
    <div className="flex items-center gap-2 rounded-lg border bg-muted/50 px-3 py-2">
      <code className="min-w-0 flex-1 font-mono text-xs break-all text-foreground select-all">
        {value}
      </code>
      <CopyButton value={value} label={`Copy ${label}`} copiedLabel="Copied!" />
    </div>
  );
}

// --- two-factor authentication -----------------------------------------

// Both TOTP dialogs (enable and disable) ask for the same six digits, so
// they share one schema as well as one field component.
const codeFormSchema = z.object({ code: totpCodeSchema });

type CodeFormValues = z.infer<typeof codeFormSchema>;

function CodeField({
  control,
  description,
}: {
  control: Control<CodeFormValues>;
  description?: string;
}) {
  return (
    <FormField
      control={control}
      name="code"
      render={({ field }) => (
        <FormItem>
          <FormLabel>Verification code</FormLabel>
          <FormControl>
            <InputOTP maxLength={6} {...field}>
              <InputOTPGroup>
                <InputOTPSlot index={0} />
                <InputOTPSlot index={1} />
                <InputOTPSlot index={2} />
              </InputOTPGroup>
              <InputOTPSeparator />
              <InputOTPGroup>
                <InputOTPSlot index={3} />
                <InputOTPSlot index={4} />
                <InputOTPSlot index={5} />
              </InputOTPGroup>
            </InputOTP>
          </FormControl>
          {description && <FormDescription>{description}</FormDescription>}
          <FormMessage />
        </FormItem>
      )}
    />
  );
}

/**
 * Enrollment — opened only once useTotpStart has already returned a fresh
 * secret (see TotpCard.onStartEnroll), so this never has to render a
 * mid-flight loading state of its own — the server draws the QR and returns
 * it alongside the secret. `enrollment.secret` is the "must live only in
 * component state" value the brief calls out, and it has *three* holders,
 * all of which TotpCard.onEnableOpenChange clears together the moment this
 * dialog closes for any reason:
 *
 *   1. `enrollment` — the local copy this component renders from.
 *   2. `totpStart.data` — the mutation result the secret arrived in.
 *   3. `totpConfirm.variables` — the `{ secret, code }` passed to mutate(),
 *      which query-core carries through the "success" action untouched.
 *
 * All three matter, and none of them clears itself: a useMutation's `.data`
 * and `.variables` are retained by the hook instance until a new mutate()
 * or an explicit reset(), regardless of what local state stopped rendering
 * them, and this dialog is mounted unconditionally by TotpCard so it never
 * unmounts either. That's why both mutations are owned by TotpCard and
 * passed down rather than being called here — a second useTotpConfirm()
 * call site would be an unrelated instance that resetting wouldn't touch
 * (the same reasoning as TokensCard's createToken; see NewTokenDialog).
 */
function TotpEnrollRow({
  enrollment,
  totpConfirm,
  onDone,
}: {
  enrollment: TotpStartResult;
  totpConfirm: ReturnType<typeof useTotpConfirm>;
  onDone: () => void;
}) {
  const form = useForm<CodeFormValues>({
    resolver: zodResolver(codeFormSchema),
    defaultValues: { code: "" },
  });

  function onSubmit(values: CodeFormValues) {
    totpConfirm.mutate(
      { secret: enrollment.secret, code: values.code },
      {
        onSuccess: () => {
          toast.success("Two-factor authentication enabled");
          onDone();
        },
        onError: (err) => {
          form.setError("code", {
            message: err instanceof ApiError ? err.message : "Invalid code — try again",
          });
          // `keepError` matters here — resetField clears a field's error by
          // default, which would silently wipe the message just set above in
          // the same tick. This only clears the entered digits so the admin
          // can retype, without erasing the reason it failed.
          form.resetField("code", { keepError: true });
        },
      },
    );
  }

  return (
    <Form {...form}>
      <form
        className="grid grid-cols-[auto_1fr] items-start gap-5 p-5"
        onSubmit={(e) => void form.handleSubmit(onSubmit)(e)}
        noValidate
      >
        <div className="flex flex-col items-center gap-2">
          {/* A fixed white surface regardless of theme — an inverted
              (light-on-dark) QR isn't reliably scannable by every reader, so
              this deliberately does not follow the app's dark background.
              Sized to the 160px image plus its 12px padding each side. */}
          <div className="flex size-[184px] items-center justify-center border border-border bg-white p-3">
            <img
              src={`data:image/png;base64,${enrollment.qr_png}`}
              alt="QR code for two-factor setup — scan with your authenticator app"
              className="size-40"
            />
          </div>
          <span className="font-mono text-xs tracking-widest text-muted-foreground uppercase">
            Scan in your app
          </span>
        </div>

        <div className="flex min-w-0 flex-col gap-4">
          <div className="flex flex-col gap-1.5">
            <p className="text-sm font-medium">Or enter this secret manually</p>
            <CopyableCode value={enrollment.secret} label="setup key" />
            <p className="text-xs text-muted-foreground">
              Nothing is saved until you confirm a code.
            </p>
          </div>

          {/* No width cap on the field: CodeField renders six OTP slots and
              a separator, which is wider than any guess — constraining it
              made the button overlap the last slots. Each item sizes itself
              and the row wraps if the band is narrow. */}
          <div className="flex flex-wrap items-end gap-3">
            <CodeField control={form.control} />
            <Button type="submit" disabled={totpConfirm.isPending}>
              {totpConfirm.isPending ? "Confirming…" : "Turn on 2FA"}
            </Button>
            <Button type="button" variant="ghost" onClick={onDone}>
              Cancel
            </Button>
          </div>
        </div>
      </form>
    </Form>
  );
}

/** Same ownership story as TotpEnableDialog above: `totpDisable` is created
 * by TotpCard and reset there when this closes, so the entered code doesn't
 * sit in `totpDisable.variables` afterward. Lower stakes than the shared
 * secret (a TOTP code is single-use and time-limited) but the same leak,
 * and free to close once the pattern is in place. */
function TotpDisableDialog({
  open,
  onOpenChange,
  totpDisable,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  totpDisable: ReturnType<typeof useTotpDisable>;
}) {
  const form = useForm<CodeFormValues>({
    resolver: zodResolver(codeFormSchema),
    defaultValues: { code: "" },
  });

  useEffect(() => {
    if (open) form.reset({ code: "" });
  }, [open, form]);

  function onSubmit(values: CodeFormValues) {
    totpDisable.mutate(
      { code: values.code },
      {
        onSuccess: () => {
          toast.success("Two-factor authentication disabled");
          onOpenChange(false);
        },
        onError: (err) => {
          form.setError("code", {
            message: err instanceof ApiError ? err.message : "Invalid code — try again",
          });
          // `keepError` matters here — resetField clears a field's error by
          // default, which would silently wipe the message just set above
          // in the same tick. This only clears the entered digits so the
          // admin can retype, without erasing the reason it failed.
          form.resetField("code", { keepError: true });
        },
      },
    );
  }

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Disable two-factor authentication</DialogTitle>
          <DialogDescription>
            Enter a current code from your authenticator app. Signing in will need only a password
            afterward.
          </DialogDescription>
        </DialogHeader>
        <Form {...form}>
          <form
            className="flex flex-col gap-4"
            onSubmit={(e) => void form.handleSubmit(onSubmit)(e)}
            noValidate
          >
            <CodeField control={form.control} />
            <DialogFooter>
              <DialogClose render={<Button type="button" variant="outline" />}>Cancel</DialogClose>
              {/* Solid, not rnui's `destructive` Button variant — that one
                  is *tinted* (bg-destructive/10 with destructive-colored
                  text), which would make removing a second factor read
                  softer than the six AlertDialogAction confirms elsewhere
                  on this page and in filtering/dns. Same class list they
                  use, so every destructive confirm in the app carries the
                  same weight. */}
              <Button
                type="submit"
                className="bg-destructive text-destructive-solid-foreground hover:bg-destructive/90"
                disabled={totpDisable.isPending}
              >
                {totpDisable.isPending ? "Disabling…" : "Disable 2FA"}
              </Button>
            </DialogFooter>
          </form>
        </Form>
      </DialogContent>
    </Dialog>
  );
}

/** The band every section on this page sits in — the same 288px label
 * column Settings uses, so the two System tabs read as one shape. */
function Section({
  title,
  icon: Icon,
  badge,
  description,
  grow = false,
  children,
}: {
  title: string;
  icon: LucideIcon;
  badge?: ReactNode;
  description: string;
  /** Lets the tokens list take the leftover height and scroll inside it. */
  grow?: boolean;
  children: ReactNode;
}) {
  return (
    <section
      className={cn(
        "grid grid-cols-[288px_1fr] border-b border-border",
        grow ? "min-h-0 flex-1" : "shrink-0",
      )}
    >
      <div className="flex flex-col items-start gap-2 border-r border-border p-5">
        <h2 className="flex items-center gap-2 font-heading text-base font-semibold">
          <Icon className="size-4 shrink-0 text-muted-foreground" aria-hidden="true" />
          {title}
        </h2>
        {badge}
        <p className="text-xs text-pretty text-muted-foreground">{description}</p>
      </div>
      <div className="flex min-w-0 flex-col">{children}</div>
    </section>
  );
}

function TotpCard({ enabled }: { enabled: boolean }) {
  // All three TOTP mutations are owned here, not inside the dialogs that
  // use them, so this component can reset() the exact instances that hold
  // the secret once a flow ends — see onEnableOpenChange.
  const totpStart = useTotpStart();
  const totpConfirm = useTotpConfirm();
  const totpDisable = useTotpDisable();
  const [enrollment, setEnrollment] = useState<TotpStartResult | null>(null);
  const [enableOpen, setEnableOpen] = useState(false);
  const [disableOpen, setDisableOpen] = useState(false);

  function onStartEnroll() {
    totpStart.mutate(undefined, {
      onSuccess: (data) => {
        setEnrollment(data);
        setEnableOpen(true);
      },
      onError: (err) =>
        toast.error(err instanceof ApiError ? err.message : "Couldn't start setup — try again"),
    });
  }

  function onEnableOpenChange(next: boolean) {
    setEnableOpen(next);
    if (!next) {
      // The secret lives only for the flow's duration — dropped the moment
      // the dialog closes, whether that's a successful confirm, Cancel,
      // Escape, or a click outside. Three things hold it, so all three are
      // cleared: `enrollment` (the local copy this component reads to
      // render the dialog), totpStart's mutation result (`totpStart.data`
      // — the response the secret arrived in), and totpConfirm's mutation
      // *variables* (`{ secret, code }`, which query-core keeps verbatim
      // after a successful mutate). useMutation retains both `.data` and
      // `.variables` until a new mutate() or an explicit reset(),
      // regardless of what local state stops rendering them, and
      // TotpEnableDialog is mounted unconditionally so it never unmounts
      // either — see its doc comment.
      setEnrollment(null);
      totpStart.reset();
      totpConfirm.reset();
    }
  }

  function onDisableOpenChange(next: boolean) {
    setDisableOpen(next);
    // Same reasoning, one step down in stakes: drops the entered code from
    // totpDisable.variables rather than leaving it in mutation state.
    if (!next) totpDisable.reset();
  }

  return (
    <Section
      title="Two-factor authentication"
      icon={ShieldCheck}
      badge={
        <Badge variant={enabled ? "success-light" : enableOpen ? "warning-light" : "secondary"}>
          {enabled ? "Enabled" : enableOpen ? "Setting up" : "Disabled"}
        </Badge>
      }
      description="A code from your authenticator app, plus your password."
    >
      {enableOpen && enrollment ? (
        <TotpEnrollRow
          enrollment={enrollment}
          totpConfirm={totpConfirm}
          onDone={() => onEnableOpenChange(false)}
        />
      ) : (
        <div className="flex flex-wrap items-center gap-5 p-5">
          <p className="text-sm text-muted-foreground">
            {enabled ? "Required at every sign-in." : "Password only, right now."}
          </p>
          <div className="ml-auto">
            {enabled ? (
              <Button
                type="button"
                variant="outline"
                size="sm"
                onClick={() => setDisableOpen(true)}
              >
                Disable 2FA
              </Button>
            ) : (
              <Button
                type="button"
                size="sm"
                onClick={onStartEnroll}
                disabled={totpStart.isPending}
              >
                {totpStart.isPending ? "Starting…" : "Set up 2FA"}
              </Button>
            )}
          </div>
        </div>
      )}

      <TotpDisableDialog
        open={disableOpen}
        onOpenChange={onDisableOpenChange}
        totpDisable={totpDisable}
      />
    </Section>
  );
}

// --- API tokens ----------------------------------------------------------

// Read/write badge styling mirrors settings.tsx's RestartBadge idiom: the
// more consequential option (write — can mutate config, records, etc., not
// just read them) gets the same warning-toned badge used there for
// "this needs your attention," while read-only gets a plain neutral tag.
const SCOPE_META: Record<
  ApiToken["scope"],
  { label: string; icon: LucideIcon; badge: "secondary" | "warning-light" }
> = {
  read: { label: "Read-only", icon: Eye, badge: "secondary" },
  write: { label: "Read & write", icon: Pencil, badge: "warning-light" },
};

const SCOPE_OPTIONS: { value: ApiToken["scope"]; label: string }[] = [
  { value: "read", label: "Read-only" },
  { value: "write", label: "Read & write" },
];

function TokenScopeBadge({ scope }: { scope: ApiToken["scope"] }) {
  const meta = SCOPE_META[scope];
  const Icon = meta.icon;
  return (
    <Badge variant={meta.badge} size="sm">
      <Icon />
      {meta.label}
    </Badge>
  );
}

const tokenFormSchema = z.object({
  name: requiredText("Name is required"),
  scope: z.enum(["read", "write"]),
});

type TokenFormValues = z.infer<typeof tokenFormSchema>;

// Defaults to "read" — the server itself defaults an omitted scope to
// "write" (see internal/api/tokens_handlers.go's handleTokenCreate), but
// this form always sends an explicit scope, so that server-side fallback
// never actually fires from here. Least-privilege is the safer default for
// a *new* credential the admin hasn't reasoned about yet; upgrading to
// read & write is one deliberate Select change away.
function tokenFormDefaults(): TokenFormValues {
  return { name: "", scope: "read" };
}

/** One declaration of the column geometry, shared by the header, the
 * create row and every token row. */
const TOKEN_GRID = "grid grid-cols-[1fr_104px_108px_116px_96px_84px] items-center gap-3.5 px-5";

/**
 * The create row: a band under the header rather than a dialog, so a token
 * is written on the line the existing ones are read on.
 *
 * `createToken` is lifted from here up to TokensCard and passed down, so
 * that component can reset() this exact mutation instance once the reveal
 * is dismissed — a useMutation() call site owns its own `.data`, and a
 * second useCreateToken() there would be an unrelated instance clearing
 * nothing.
 */
function NewTokenRow({
  createToken,
  onCreated,
  onClose,
}: {
  createToken: ReturnType<typeof useCreateToken>;
  onCreated: (result: { id: number; token: string }) => void;
  onClose: () => void;
}) {
  const form = useForm<TokenFormValues>({
    resolver: zodResolver(tokenFormSchema),
    defaultValues: tokenFormDefaults(),
  });

  useEffect(() => {
    form.setFocus("name");
  }, [form]);

  function onSubmit(values: TokenFormValues) {
    createToken.mutate(
      { name: values.name.trim(), scope: values.scope },
      {
        onSuccess: (result) => {
          toast.success("Token created");
          onClose();
          onCreated(result);
        },
        onError: (err) =>
          toast.error(err instanceof ApiError ? err.message : "Couldn't create the token"),
      },
    );
  }

  return (
    <Form {...form}>
      <form
        onSubmit={(e) => void form.handleSubmit(onSubmit)(e)}
        noValidate
        data-slot="new-token-row"
        className="shrink-0 border-b border-border bg-card shadow-[inset_3px_0_0_var(--primary)]"
      >
        <div className={cn(TOKEN_GRID, "py-2.5")}>
          <FormField
            control={form.control}
            name="name"
            render={({ field }) => (
              <FormItem>
                <FormControl>
                  <Input
                    {...field}
                    aria-label="Name"
                    placeholder="homeassistant"
                    autoComplete="off"
                  />
                </FormControl>
              </FormItem>
            )}
          />
          <FormField
            control={form.control}
            name="scope"
            render={({ field }) => (
              <FormItem>
                <FormControl>
                  <NativeSelect {...field} aria-label="Scope">
                    {SCOPE_OPTIONS.map((option) => (
                      <NativeSelectOption key={option.value} value={option.value}>
                        {option.value}
                      </NativeSelectOption>
                    ))}
                  </NativeSelect>
                </FormControl>
              </FormItem>
            )}
          />
          {/* The three the server fills in, shown so the row lines up with
              the header and says what the token will be. */}
          <span className="font-mono text-sm text-muted-foreground">now</span>
          <span className="font-mono text-sm text-muted-foreground">—</span>
          <span className="font-mono text-sm text-muted-foreground">never</span>
          <div className="flex items-center justify-end gap-1.5">
            <Button type="submit" size="sm" disabled={createToken.isPending}>
              {createToken.isPending ? "Creating…" : "Create"}
            </Button>
            <Button
              type="button"
              size="icon-sm"
              variant="ghost"
              aria-label="Close the new-token row"
              onClick={onClose}
            >
              <X />
            </Button>
          </div>
        </div>
        {/* Validation only. The hints that were here described a name
            field, a two-option select and a fact the reveal banner states
            far more loudly a second later. */}
        {form.formState.errors.name && (
          <div className={cn(TOKEN_GRID, "pb-2.5 text-xs")}>
            <FormField control={form.control} name="name" render={() => <FormMessage />} />
          </div>
        )}
      </form>
    </Form>
  );
}

function TokensCard() {
  const tokens = useTokens();
  const revokeToken = useRevokeToken();
  // Lifted up from NewTokenDialog (not called there directly) so it can be
  // reset() here, once the reveal dialog closes — see onDismissReveal.
  const createToken = useCreateToken();
  const [addOpen, setAddOpen] = useState(false);
  const [revealResult, setRevealResult] = useState<{ id: number; token: string } | null>(null);
  const [revokeTarget, setRevokeTarget] = useState<ApiToken | null>(null);

  // TanStack Query's useMutation keeps its `.data` (here, the plaintext
  // token) alive in memory for the lifetime of the component that called
  // it — it is cleared only by a new mutate() or an explicit reset(),
  // never automatically just because the UI that *displayed* it went away.
  // Clearing `revealResult` alone (the local render state) leaves the
  // plaintext still sitting in createToken.data, inspectable via React
  // DevTools. Both are cleared together here, whenever the reveal dialog
  // closes for any reason (the footer button, Escape, or a backdrop click).
  function onDismissReveal(open: boolean) {
    if (open) return;
    setRevealResult(null);
    createToken.reset();
  }

  function onConfirmRevoke() {
    if (!revokeTarget) return;
    const target = revokeTarget;
    revokeToken.mutate(target.id, {
      onSuccess: () => {
        toast.success("Token revoked");
        setRevokeTarget(null);
      },
      onError: () => toast.error(`Couldn't revoke ${target.name}`),
    });
  }

  const isEmpty = tokens.data?.length === 0;

  let body: ReactNode;
  if (tokens.isPending) {
    body = (
      <div className="flex flex-col gap-2" aria-hidden="true">
        {Array.from({ length: 3 }).map((_, i) => (
          <Skeleton key={i} className="h-10 w-full" />
        ))}
      </div>
    );
  } else if (tokens.data === undefined) {
    // isError with no data — the first load itself failed, so there's
    // nothing to fall back to. A background failure with data still in
    // hand takes the StaleDataAlert path below instead.
    body = (
      <Alert variant="destructive">
        <TriangleAlert />
        <AlertTitle>Couldn&apos;t load API tokens</AlertTitle>
        <AlertDescription>Try refreshing the page.</AlertDescription>
      </Alert>
    );
  } else if (isEmpty && addOpen) {
    // The create row above is already the answer to both halves of the
    // empty state — there's something here now, and the way to start is
    // open. Offering "New token" under a form that opened for that click
    // just makes the button look broken.
    body = null;
  } else if (isEmpty) {
    body = (
      <EmptyState
        icon={<KeyRound />}
        title="No API tokens yet"
        description="Create a token to let a script or another tool call the dnsaur API on your behalf."
        action={
          <Button type="button" size="sm" onClick={() => setAddOpen(true)}>
            <Plus />
            New token
          </Button>
        }
      />
    );
  } else {
    body = tokens.data.map((token) => (
      <div
        key={token.id}
        data-slot="token-row"
        className={cn(
          TOKEN_GRID,
          "border-b border-border-muted py-2.5",
          // The one just created, so it stays findable after the banner
          // above it is dismissed.
          revealResult?.id === token.id && "bg-primary/5",
        )}
      >
        <span className="truncate font-mono text-sm" title={token.name}>
          {token.name}
        </span>
        <span>
          <TokenScopeBadge scope={token.scope} />
        </span>
        <span
          className="font-mono text-sm text-muted-foreground"
          title={new Date(token.created_at).toLocaleString()}
        >
          {relativeTime(token.created_at)}
        </span>
        <span
          className={cn(
            "font-mono text-sm",
            token.last_used ? "text-foreground" : "text-muted-foreground",
          )}
          title={token.last_used ? new Date(token.last_used).toLocaleString() : undefined}
        >
          {relativeTime(token.last_used)}
        </span>
        <span className="font-mono text-sm text-muted-foreground">never</span>
        <span className="flex items-center justify-end">
          <Button
            type="button"
            size="sm"
            variant="ghost"
            aria-label={`Revoke ${token.name}`}
            onClick={() => setRevokeTarget(token)}
          >
            Revoke
          </Button>
        </span>
      </div>
    ));
  }

  return (
    <Section
      title="API tokens"
      icon={KeyRound}
      description="Scoped credentials for scripts and tools."
      grow
    >
      {/* A banner, not a dialog. This is the only time the plaintext will
          ever exist on screen, and a modal invites the two gestures that
          lose it — Escape, and a click on the backdrop. This one goes away
          only when its own button says the token has been saved. */}
      {revealResult && (
        <div className="shrink-0 border-b border-border bg-invert p-5 text-invert-foreground">
          <div className="flex items-center gap-2.5">
            <span className="bg-warning px-2 py-1 font-mono text-xs font-semibold tracking-widest text-warning-solid-foreground uppercase">
              Shown once
            </span>
            <p className="font-heading text-sm font-semibold">
              Copy your token now — it can&apos;t be shown again
            </p>
          </div>
          <p className="mt-3 border border-invert-foreground/25 bg-invert-foreground/5 p-3 font-mono text-base break-all">
            {revealResult.token}
          </p>
          <div className="mt-3 flex flex-wrap items-center gap-2">
            <Button
              type="button"
              size="sm"
              // The same guard the query log's Copy row JSON uses. Optional
              // chaining alone would `await undefined` and then toast a
              // success for a copy that never happened — and the clipboard
              // API is absent outside a secure context, which is exactly
              // where a homelab instance reached over plain HTTP lives. A
              // false "Token copied" here costs the token: the next press is
              // "I've saved it".
              onClick={() => {
                if (!navigator.clipboard) {
                  toast.error("Couldn't copy — this browser won't allow clipboard access here");
                  return;
                }
                navigator.clipboard.writeText(revealResult.token).then(
                  () => toast.success("Token copied"),
                  () => toast.error("Couldn't copy the token"),
                );
              }}
            >
              <Copy />
              Copy token
            </Button>
            <Button
              type="button"
              size="sm"
              variant="outline"
              onClick={() => onDismissReveal(false)}
            >
              I&apos;ve saved it
            </Button>
            <span className="ml-auto font-mono text-xs opacity-75">
              Leaving this page discards it.
            </span>
          </div>
        </div>
      )}

      <div className="flex shrink-0 items-center gap-2.5 border-b border-border px-5 py-2.5">
        <span className="font-mono text-xs text-muted-foreground">
          {tokens.data
            ? `${tokens.data.length} ${tokens.data.length === 1 ? "token" : "tokens"} · none expire`
            : ""}
        </span>
        {!isEmpty && !addOpen && (
          <Button type="button" size="sm" className="ml-auto" onClick={() => setAddOpen(true)}>
            <Plus />
            New token
          </Button>
        )}
      </div>

      <div
        className={cn(
          TOKEN_GRID,
          "shrink-0 border-b border-border py-2",
          "font-mono text-xs tracking-widest text-muted-foreground uppercase",
        )}
      >
        <span>Name</span>
        <span>Scope</span>
        <span>Created</span>
        <span>Last used</span>
        <span>Expires</span>
        <span className="text-right">Actions</span>
      </div>

      {addOpen && (
        <NewTokenRow
          createToken={createToken}
          onCreated={setRevealResult}
          onClose={() => setAddOpen(false)}
        />
      )}

      <div className="min-h-0 flex-1 overflow-y-auto">
        {tokens.isError && tokens.data !== undefined && (
          <div className="p-3">
            <StaleDataAlert
              what="API tokens"
              onRetry={() => void tokens.refetch()}
              isRetrying={tokens.isFetching}
            />
          </div>
        )}
        {body}
      </div>

      <ConfirmDeleteDialog
        open={revokeTarget !== null}
        onOpenChange={(open) => !open && setRevokeTarget(null)}
        title="Revoke this token?"
        description={
          revokeTarget && (
            <>
              <code className="font-mono break-all text-foreground">{revokeTarget.name}</code> stops
              working immediately. Anything using it needs a new token.
            </>
          )
        }
        confirmLabel="Revoke"
        pendingLabel="Revoking…"
        isPending={revokeToken.isPending}
        onConfirm={onConfirmRevoke}
      />
    </Section>
  );
}

// --- page ------------------------------------------------------------------

function AccountSkeleton() {
  return (
    <div className="flex flex-col" aria-hidden="true">
      {Array.from({ length: 2 }).map((_, i) => (
        <div key={i} className="grid grid-cols-[288px_1fr] border-b border-border">
          <div className="flex flex-col gap-2 border-r border-border p-5">
            <Skeleton className="h-5 w-40" />
            <Skeleton className="h-4 w-24" />
          </div>
          <div className="flex flex-col gap-3 p-5">
            <Skeleton className="h-8 w-full" />
            <Skeleton className="h-8 w-2/3" />
          </div>
        </div>
      ))}
    </div>
  );
}

// --- page ------------------------------------------------------------------

export function Account() {
  const me = useMe();

  return (
    // The Section band and TOKEN_GRID are both fixed-pixel column templates,
    // and the shell around this is h-screen/overflow-hidden
    // (components/app-shell.tsx) — so a narrow viewport clipped the Revoke
    // column with nothing to scroll.
    <div data-slot="h-scroll" className="flex h-full min-h-0 flex-col overflow-x-auto">
      {me.isPending && <AccountSkeleton />}

      {me.isError && me.data === undefined && (
        <div className="p-5">
          <Alert variant="destructive">
            <TriangleAlert />
            <AlertTitle>Couldn&apos;t load your account</AlertTitle>
            <AlertDescription>Try refreshing the page.</AlertDescription>
          </Alert>
        </div>
      )}

      {/* `data !== undefined`, not `isSuccess` — a failed background
          refetch (every TOTP mutation invalidates `me`) flips isSuccess
          false while data is still perfectly good, and unmounting these
          sections mid-flow would throw away the enrollment dialog's state
          along with them. */}
      {me.data !== undefined && (
        <>
          {me.isError && (
            <div className="shrink-0 border-b border-border p-3">
              <StaleDataAlert
                what="your account"
                onRetry={() => void me.refetch()}
                isRetrying={me.isFetching}
              />
            </div>
          )}
          <TotpCard enabled={me.data.totp_enabled} />
          <TokensCard />
          <Section
            title="Password"
            icon={Lock}
            badge={<Badge variant="secondary">Planned</Badge>}
            description="The password you sign in with."
          >
            <p className="p-5 text-sm text-muted-foreground">
              Password changes aren&apos;t available yet — that&apos;s planned for a future update.
            </p>
          </Section>
        </>
      )}
    </div>
  );
}
