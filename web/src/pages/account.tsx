import { useEffect, useState, type ReactNode } from "react";
import {
  Eye,
  KeyRound,
  Pencil,
  Plus,
  ShieldCheck,
  Trash2,
  TriangleAlert,
  type LucideIcon,
} from "lucide-react";
import { toast } from "sonner";
import { useForm, type Control } from "react-hook-form";
import * as QRCode from "qrcode";
import {
  Alert,
  AlertDescription,
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
  AlertTitle,
  Badge,
  Button,
  Card,
  CardAction,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
  CopyButton,
  Dialog,
  DialogClose,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
  EmptyState,
  Form,
  FormControl,
  FormDescription,
  FormField,
  FormItem,
  FormLabel,
  FormMessage,
  Input,
  InputOTP,
  InputOTPGroup,
  InputOTPSeparator,
  InputOTPSlot,
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
  Separator,
  Skeleton,
  Spinner,
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@e412/rnui-react";
import { ApiError } from "../api/client";
import type { ApiToken } from "../api/types";
import { useMe } from "../hooks/use-auth";
import { useCreateToken, useRevokeToken, useTokens } from "../hooks/use-tokens";
import { useTotpConfirm, useTotpDisable, useTotpStart } from "../hooks/use-totp";
import { relativeTime } from "../lib/format";

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

/** otpauth:// URL -> an `<img>`-ready data URL, via `qrcode`'s pure string
 * SVG renderer (no canvas involved — jsdom doesn't implement 2D canvas
 * rasterization, see test/setup.ts, and SVG is also just crisper at any
 * zoom level than a rasterized PNG would be). The renderer's defaults
 * already paint an opaque white background behind the dark modules, so the
 * code stays scannable in dark mode without extra styling here beyond the
 * white card it sits in below. */
function useQrDataUrl(text: string | null): string | null {
  const [dataUrl, setDataUrl] = useState<string | null>(null);

  useEffect(() => {
    if (!text) {
      setDataUrl(null);
      return;
    }
    let cancelled = false;
    QRCode.toString(text, { type: "svg", margin: 1, width: 160 })
      .then((svg) => {
        if (!cancelled) setDataUrl(`data:image/svg+xml;utf8,${encodeURIComponent(svg)}`);
      })
      .catch(() => {
        if (!cancelled) setDataUrl(null);
      });
    return () => {
      cancelled = true;
    };
  }, [text]);

  return dataUrl;
}

interface CodeFormValues {
  code: string;
}

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
      rules={{ validate: (value) => value.length === 6 || "Enter the 6-digit code" }}
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
 * mid-flight loading state of its own for the secret itself, only for the
 * QR image derived from it. `enrollment.secret` is the "must live only in
 * component state" value the brief calls out: TotpCard drops the local copy
 * (`setEnrollment(null)`) *and* resets the underlying totpStart mutation
 * (`totpStart.reset()`) the moment this dialog closes for any reason — see
 * TotpCard.onEnableOpenChange. Both matter: useMutation's own `.data` isn't
 * a react-query cache entry, but it is retained by the hook instance until
 * reset() is called, independent of whatever local state stops rendering
 * it — clearing `enrollment` alone would leave the secret sitting in
 * totpStart.data.
 */
function TotpEnableDialog({
  open,
  onOpenChange,
  enrollment,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  enrollment: { secret: string; otpauth_url: string } | null;
}) {
  const totpConfirm = useTotpConfirm();
  const form = useForm<CodeFormValues>({ defaultValues: { code: "" } });
  const qrDataUrl = useQrDataUrl(open ? (enrollment?.otpauth_url ?? null) : null);

  useEffect(() => {
    if (open) form.reset({ code: "" });
  }, [open, form]);

  function onSubmit(values: CodeFormValues) {
    if (!enrollment) return;
    totpConfirm.mutate(
      { secret: enrollment.secret, code: values.code },
      {
        onSuccess: () => {
          toast.success("Two-factor authentication enabled");
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
          <DialogTitle>Set up two-factor authentication</DialogTitle>
          <DialogDescription>
            Scan the QR code with your authenticator app, or enter the setup key manually.
          </DialogDescription>
        </DialogHeader>

        {enrollment && (
          <Form {...form}>
            <form
              className="flex flex-col gap-4"
              onSubmit={(e) => void form.handleSubmit(onSubmit)(e)}
              noValidate
            >
              <div className="flex justify-center">
                {/* A fixed white surface regardless of theme — an inverted
                    (light-on-dark) QR code isn't reliably scannable by
                    every reader, so this deliberately doesn't follow the
                    app's dark-mode card background here. A light shadow
                    (instead of relying on the border alone) keeps it from
                    reading as a flat cutout against a dark popover. Sized
                    to the 160px image plus its 12px (p-3) padding on each
                    side, so the loading Spinner reserves the same
                    footprint and nothing jumps once the QR image lands. */}
                <div className="flex size-[184px] items-center justify-center rounded-lg border bg-white p-3 shadow-sm">
                  {qrDataUrl ? (
                    <img
                      src={qrDataUrl}
                      alt="QR code for two-factor setup — scan with your authenticator app"
                      className="size-40"
                    />
                  ) : (
                    // A fixed gray, not text-muted-foreground: that token is
                    // theme-relative and, in dark mode, resolves to a pale
                    // gray meant for dark surfaces — nearly invisible on
                    // this box's always-white background.
                    <Spinner className="size-6 text-gray-400" />
                  )}
                </div>
              </div>

              <div className="flex flex-col gap-1.5">
                <p className="text-xs text-muted-foreground">
                  Can&apos;t scan? Enter this key manually:
                </p>
                <CopyableCode value={enrollment.secret} label="setup key" />
              </div>

              <CodeField
                control={form.control}
                description="Enter the 6-digit code your authenticator app is now showing."
              />

              <DialogFooter>
                <DialogClose render={<Button type="button" variant="outline" />}>
                  Cancel
                </DialogClose>
                <Button type="submit" disabled={totpConfirm.isPending}>
                  {totpConfirm.isPending ? "Confirming…" : "Confirm"}
                </Button>
              </DialogFooter>
            </form>
          </Form>
        )}
      </DialogContent>
    </Dialog>
  );
}

function TotpDisableDialog({
  open,
  onOpenChange,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
}) {
  const totpDisable = useTotpDisable();
  const form = useForm<CodeFormValues>({ defaultValues: { code: "" } });

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
            Enter a current code from your authenticator app to confirm. Your account will only need
            a password to sign in afterward.
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
              <Button type="submit" variant="destructive" disabled={totpDisable.isPending}>
                {totpDisable.isPending ? "Disabling…" : "Disable 2FA"}
              </Button>
            </DialogFooter>
          </form>
        </Form>
      </DialogContent>
    </Dialog>
  );
}

function TotpCard({ enabled }: { enabled: boolean }) {
  const totpStart = useTotpStart();
  const [enrollment, setEnrollment] = useState<{ secret: string; otpauth_url: string } | null>(
    null,
  );
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
      // Escape, or a click outside. Two things hold it, so both are
      // cleared: `enrollment` (the local copy this component reads to
      // render the dialog) and totpStart's own mutation result
      // (`totpStart.data` — useMutation retains it until a new mutate() or
      // an explicit reset(), regardless of what local state stops
      // rendering it; see TotpEnableDialog's doc comment).
      setEnrollment(null);
      totpStart.reset();
    }
  }

  return (
    <Card>
      <CardHeader>
        <CardTitle className="flex items-center gap-2">
          <ShieldCheck className="size-4 text-muted-foreground" aria-hidden="true" />
          Two-factor authentication
          <Badge variant={enabled ? "success-light" : "secondary"} size="sm">
            {enabled ? "Enabled" : "Disabled"}
          </Badge>
        </CardTitle>
        <CardDescription>
          Require a 6-digit code from an authenticator app in addition to your password at sign-in.
        </CardDescription>
      </CardHeader>
      <CardContent>
        <div className="flex flex-wrap items-center justify-between gap-3">
          <p className="text-sm text-muted-foreground">
            {enabled
              ? "Your account currently requires a code at sign-in."
              : "Your account can currently be signed into with just a password."}
          </p>
          {enabled ? (
            <Button type="button" variant="outline" size="sm" onClick={() => setDisableOpen(true)}>
              Disable 2FA
            </Button>
          ) : (
            <Button type="button" size="sm" onClick={onStartEnroll} disabled={totpStart.isPending}>
              {totpStart.isPending ? "Starting…" : "Enable 2FA"}
            </Button>
          )}
        </div>
      </CardContent>

      <TotpEnableDialog
        open={enableOpen}
        onOpenChange={onEnableOpenChange}
        enrollment={enrollment}
      />
      <TotpDisableDialog open={disableOpen} onOpenChange={setDisableOpen} />
    </Card>
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

interface TokenFormValues {
  name: string;
  scope: ApiToken["scope"];
}

function validateTokenName(value: string): string | true {
  return value.trim() ? true : "Name is required";
}

// Defaults to "read" — the server itself defaults an omitted scope to
// "write" (see internal/api/tokens_handlers.go's handleTokenCreate), but
// this form always sends an explicit scope, so that server-side fallback
// never actually fires from here. Least-privilege is the safer default for
// a *new* credential the admin hasn't reasoned about yet; upgrading to
// read & write is one deliberate Select change away.
function tokenFormDefaults(): TokenFormValues {
  return { name: "", scope: "read" };
}

function NewTokenDialog({
  createToken,
  open,
  onOpenChange,
  onCreated,
}: {
  // Lifted from a local useCreateToken() call up to TokensCard and passed
  // down, so TokensCard can reset() this exact mutation instance once the
  // reveal dialog closes — a useMutation() call site owns its own `.data`;
  // a second, separate useCreateToken() call in TokensCard would create an
  // unrelated instance and not actually clear anything.
  createToken: ReturnType<typeof useCreateToken>;
  open: boolean;
  onOpenChange: (open: boolean) => void;
  onCreated: (result: { id: number; token: string }) => void;
}) {
  const form = useForm<TokenFormValues>({ defaultValues: tokenFormDefaults() });

  useEffect(() => {
    if (open) form.reset(tokenFormDefaults());
  }, [open, form]);

  function onSubmit(values: TokenFormValues) {
    createToken.mutate(
      { name: values.name.trim(), scope: values.scope },
      {
        onSuccess: (result) => {
          toast.success("Token created");
          onOpenChange(false);
          onCreated(result);
        },
        onError: (err) =>
          toast.error(err instanceof ApiError ? err.message : "Couldn't create the token"),
      },
    );
  }

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>New token</DialogTitle>
          <DialogDescription>
            Create a scoped credential a script or another tool can use to call the dnsaur API.
          </DialogDescription>
        </DialogHeader>
        <Form {...form}>
          <form
            className="flex flex-col gap-4"
            onSubmit={(e) => void form.handleSubmit(onSubmit)(e)}
            noValidate
          >
            <FormField
              control={form.control}
              name="name"
              rules={{ validate: validateTokenName }}
              render={({ field }) => (
                <FormItem>
                  <FormLabel>Name</FormLabel>
                  <FormControl>
                    <Input {...field} placeholder="Home Assistant" autoComplete="off" />
                  </FormControl>
                  <FormDescription>A label to help you recognize this token later.</FormDescription>
                  <FormMessage />
                </FormItem>
              )}
            />
            <FormField
              control={form.control}
              name="scope"
              render={({ field }) => (
                <FormItem>
                  <FormLabel>Scope</FormLabel>
                  {/* `items` maps each value to its display label — without
                      it, SelectValue renders the raw stored value ("read")
                      instead of the option's label (Task 12's finding). */}
                  <Select
                    items={Object.fromEntries(SCOPE_OPTIONS.map((o) => [o.value, o.label]))}
                    value={field.value}
                    onValueChange={field.onChange}
                  >
                    <SelectTrigger aria-label="Scope">
                      <SelectValue />
                    </SelectTrigger>
                    <SelectContent>
                      {SCOPE_OPTIONS.map((option) => (
                        <SelectItem key={option.value} value={option.value}>
                          {option.label}
                        </SelectItem>
                      ))}
                    </SelectContent>
                  </Select>
                  <FormDescription>
                    Read-only tokens can fetch data. Read & write tokens can also change it.
                  </FormDescription>
                </FormItem>
              )}
            />
            <DialogFooter>
              <DialogClose render={<Button type="button" variant="outline" />}>Cancel</DialogClose>
              <Button type="submit" disabled={createToken.isPending}>
                {createToken.isPending ? "Creating…" : "Create token"}
              </Button>
            </DialogFooter>
          </form>
        </Form>
      </DialogContent>
    </Dialog>
  );
}

/**
 * The one-time plaintext reveal — the single highest-stakes moment on this
 * page (closing without copying loses the token forever; dnsaur only ever
 * stores a hash of it, see store.AuthToken.TokenHash). Deliberately no
 * plain "Close" affordance in the footer: the one button there names the
 * consequence directly, and the Alert above states the fact rather than
 * dramatizing it — the stakes carry the design, not extra chrome.
 * `showCloseButton={false}` on DialogContent turns off rnui's default
 * top-right X icon, so the deliberate footer button really is the only
 * *labeled* dismissal — the design intent this component already claimed
 * before the X was actually suppressed. (Escape / backdrop click still
 * close it, same as every other dialog on this page; trapping the dialog
 * open entirely would be a bigger, separate UX call this fix doesn't make.)
 * The caller (TokensCard) is responsible for resetting the createToken
 * mutation whenever this closes — see its onOpenChange.
 */
function TokenRevealDialog({
  result,
  onOpenChange,
}: {
  result: { id: number; token: string } | null;
  onOpenChange: (open: boolean) => void;
}) {
  return (
    <Dialog open={result !== null} onOpenChange={onOpenChange}>
      <DialogContent showCloseButton={false}>
        <DialogHeader>
          <DialogTitle>Copy your token now</DialogTitle>
          {/* States context, not the warning — the Alert right below owns
              that, so the two don't repeat "you won't see this again"
              back to back. */}
          <DialogDescription>Your token was created successfully.</DialogDescription>
        </DialogHeader>
        {result && (
          <div className="flex flex-col gap-4">
            <Alert variant="warning">
              <TriangleAlert />
              <AlertTitle>You won&apos;t see this again</AlertTitle>
              <AlertDescription>
                Copy it now and store it somewhere safe. dnsaur only keeps a hash of it — if
                it&apos;s lost, revoke this token and create a new one.
              </AlertDescription>
            </Alert>
            <CopyableCode value={result.token} label="token" />
            <DialogFooter>
              <Button type="button" onClick={() => onOpenChange(false)}>
                I&apos;ve saved it — close
              </Button>
            </DialogFooter>
          </div>
        )}
      </DialogContent>
    </Dialog>
  );
}

function TokensTable({
  tokens,
  onRevokeRequest,
}: {
  tokens: ApiToken[];
  onRevokeRequest: (token: ApiToken) => void;
}) {
  return (
    <Table>
      <TableHeader>
        <TableRow>
          <TableHead>Name</TableHead>
          <TableHead>Scope</TableHead>
          <TableHead>Created</TableHead>
          <TableHead>Last used</TableHead>
          <TableHead className="text-right">
            <span className="sr-only">Actions</span>
          </TableHead>
        </TableRow>
      </TableHeader>
      <TableBody>
        {tokens.map((token) => (
          <TableRow key={token.id}>
            <TableCell className="max-w-48 truncate" title={token.name}>
              {token.name}
            </TableCell>
            <TableCell>
              <TokenScopeBadge scope={token.scope} />
            </TableCell>
            <TableCell
              className="text-muted-foreground tabular-nums"
              title={new Date(token.created_at).toLocaleString()}
            >
              {relativeTime(token.created_at)}
            </TableCell>
            <TableCell
              className="text-muted-foreground tabular-nums"
              title={token.last_used ? new Date(token.last_used).toLocaleString() : undefined}
            >
              {relativeTime(token.last_used)}
            </TableCell>
            <TableCell className="text-right">
              <Button
                type="button"
                size="icon-sm"
                variant="ghost"
                aria-label={`Revoke ${token.name}`}
                onClick={() => onRevokeRequest(token)}
              >
                <Trash2 />
              </Button>
            </TableCell>
          </TableRow>
        ))}
      </TableBody>
    </Table>
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

  const isEmpty = tokens.isSuccess && tokens.data.length === 0;

  let body: ReactNode;
  if (tokens.isPending) {
    body = (
      <div className="flex flex-col gap-2" aria-hidden="true">
        {Array.from({ length: 3 }).map((_, i) => (
          <Skeleton key={i} className="h-10 w-full" />
        ))}
      </div>
    );
  } else if (tokens.isError) {
    body = (
      <Alert variant="destructive">
        <TriangleAlert />
        <AlertTitle>Couldn&apos;t load API tokens</AlertTitle>
        <AlertDescription>Try refreshing the page.</AlertDescription>
      </Alert>
    );
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
    body = <TokensTable tokens={tokens.data} onRevokeRequest={setRevokeTarget} />;
  }

  return (
    <Card>
      <CardHeader>
        <CardTitle className="flex items-center gap-2">
          <KeyRound className="size-4 text-muted-foreground" aria-hidden="true" />
          API tokens
        </CardTitle>
        <CardDescription>
          Scoped credentials scripts and other tools can use to call the dnsaur API instead of your
          session.
        </CardDescription>
        {!isEmpty && (
          <CardAction>
            <Button type="button" size="sm" onClick={() => setAddOpen(true)}>
              <Plus />
              New token
            </Button>
          </CardAction>
        )}
      </CardHeader>
      <CardContent>{body}</CardContent>

      <NewTokenDialog
        createToken={createToken}
        open={addOpen}
        onOpenChange={setAddOpen}
        onCreated={setRevealResult}
      />
      <TokenRevealDialog result={revealResult} onOpenChange={onDismissReveal} />

      <AlertDialog
        open={revokeTarget !== null}
        onOpenChange={(open) => !open && setRevokeTarget(null)}
      >
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>Revoke this token?</AlertDialogTitle>
            <AlertDialogDescription>
              {revokeTarget && (
                <>
                  <code className="font-mono break-all text-foreground">{revokeTarget.name}</code>{" "}
                  will stop working immediately. Anything using it will need a new token.
                </>
              )}
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>Cancel</AlertDialogCancel>
            <AlertDialogAction
              className="bg-destructive text-destructive-solid-foreground hover:bg-destructive/90"
              onClick={onConfirmRevoke}
              disabled={revokeToken.isPending}
            >
              {revokeToken.isPending ? "Revoking…" : "Revoke"}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </Card>
  );
}

// --- page ------------------------------------------------------------------

function AccountSkeleton() {
  return (
    <div className="flex flex-col gap-6" aria-hidden="true">
      <Card>
        <CardHeader>
          <Skeleton className="h-5 w-56" />
          <Skeleton className="mt-1.5 h-4 w-80" />
        </CardHeader>
        <CardContent>
          <Skeleton className="h-9 w-full max-w-sm" />
        </CardContent>
      </Card>
      <Card>
        <CardHeader>
          <Skeleton className="h-5 w-28" />
          <Skeleton className="mt-1.5 h-4 w-72" />
        </CardHeader>
        <CardContent className="flex flex-col gap-2">
          {Array.from({ length: 3 }).map((_, i) => (
            <Skeleton key={i} className="h-10 w-full" />
          ))}
        </CardContent>
      </Card>
    </div>
  );
}

/**
 * Account & security — two-factor authentication and API token management
 * (Task 13). GET /auth/me gates both sections (TOTP needs totp_enabled to
 * pick a flow; tokens don't depend on it, but there's no reason to show
 * them before the page even knows who's asking).
 *
 * No password-change section: internal/api/openapi.yaml's Auth tag has no
 * change-password endpoint at this milestone (only login/logout/me/totp/*).
 * A disabled form here would look broken rather than honest, so this says
 * so in one quiet line instead of pretending the control exists.
 */
export function Account() {
  const me = useMe();

  return (
    <div className="flex flex-col gap-6">
      <div>
        <h1 className="text-2xl font-heading font-semibold text-foreground">Account & security</h1>
        <p className="mt-1 text-sm text-muted-foreground">
          Two-factor authentication and API tokens for this account.
        </p>
        <p className="mt-2 text-xs text-muted-foreground">
          Password changes aren&apos;t available yet — that&apos;s planned for a future update.
        </p>
      </div>

      <Separator />

      {me.isPending && <AccountSkeleton />}

      {me.isError && (
        <Alert variant="destructive">
          <TriangleAlert />
          <AlertTitle>Couldn&apos;t load your account</AlertTitle>
          <AlertDescription>Try refreshing the page.</AlertDescription>
        </Alert>
      )}

      {me.isSuccess && (
        <>
          <TotpCard enabled={me.data.totp_enabled} />
          <TokensCard />
        </>
      )}
    </div>
  );
}
