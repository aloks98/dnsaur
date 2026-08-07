import { useState } from "react";
import { Check, CircleAlert, TriangleAlert } from "lucide-react";
import { toast } from "sonner";
import { useForm } from "react-hook-form";
import { zodResolver } from "@hookform/resolvers/zod";
import { z } from "zod";
import { useQueryClient } from "@tanstack/react-query";
import {
  Alert,
  AlertDescription,
  AlertTitle,
  Button,
  Card,
  Checkbox,
  cn,
  Form,
  FormControl,
  FormDescription,
  FormField,
  FormItem,
  FormLabel,
  FormMessage,
  Input,
} from "@e412/rnui-react";
import { ApiError } from "../api/client";
import { AuthLayout } from "../components/auth-layout";
import { authKeys, useSetup, useSetupSignIn } from "../hooks/use-auth";
import { useAddList, useAssignGroupLists } from "../hooks/use-filters";

const STARTER_GROUP_ID = 1;

/**
 * Every URL here is fetched on first run, so a dead one is the worst
 * possible first impression: the wizard reports success and the instance
 * filters nothing.
 *
 * hagezi's path is `wildcard/`, not `hosts/`. The `hosts/pro.txt` this
 * previously shipped 404s — verified — which meant every fresh install
 * subscribed to a list that could never load.
 */
const STARTER_LISTS = [
  {
    key: "stevenblack",
    name: "StevenBlack hosts",
    url: "https://raw.githubusercontent.com/StevenBlack/hosts/master/hosts",
    description: "Ads and malware, the usual default.",
    size: "~99k",
    default: true,
  },
  {
    key: "hagezi",
    name: "hagezi pro",
    url: "https://raw.githubusercontent.com/hagezi/dns-blocklists/main/wildcard/pro.txt",
    description: "Larger, more aggressive. Occasional false positives.",
    size: "~218k",
    default: true,
  },
  {
    key: "oisd",
    name: "OISD small",
    url: "https://small.oisd.nl/",
    description: "Conservative and well curated.",
    size: "~48k",
    default: true,
  },
  {
    key: "tracking",
    name: "Kids: tracking",
    url: "https://raw.githubusercontent.com/blocklistproject/Lists/master/tracking.txt",
    description: "Extra trackers, useful on a kids group.",
    size: "~34k",
    default: false,
  },
] as const;

/**
 * The account step's rules, mirroring POST /setup (internal/api's
 * handleSetup rejects an empty username and a password under 8
 * characters). Nothing is trimmed: whitespace can be part of a password,
 * and both fields are sent exactly as typed.
 *
 * The confirmation field is not in the artboard, and is kept deliberately:
 * there is no password recovery flow, so a typo here is unrecoverable
 * without shell access. The design's own hint says as much.
 */
const accountFormSchema = z
  .object({
    username: z.string().min(1, "Username is required"),
    password: z
      .string()
      .min(1, "Password is required")
      .min(8, "Password must be at least 8 characters"),
    confirmPassword: z.string().min(1, "Confirm your password"),
  })
  .superRefine((values, ctx) => {
    if (values.confirmPassword !== values.password) {
      ctx.addIssue({ code: "custom", message: "Passwords don't match", path: ["confirmPassword"] });
    }
  });

type AccountFormValues = z.infer<typeof accountFormSchema>;

type Stage = "welcome" | "admin" | "lists" | "done";

/** What the wizard actually managed to do, so the last screen can report it
 * rather than claiming everything worked. */
interface Outcome {
  /** Lists created *and* attached to the default group. */
  applied: number;
  /** How many were selected — `applied < chosen` is the partial case. */
  chosen: number;
  /** Named so the last screen can say which one to look at. */
  failed: string[];
  /** False when the silent post-setup sign-in didn't work. */
  signedIn: boolean;
}

const STEPS = [
  { num: 1, label: "Admin", stage: "admin" },
  { num: 2, label: "Blocklists", stage: "lists" },
  { num: 3, label: "Done", stage: "done" },
] as const;

/** The wizard's position, as three cells across the top of the card. Absent
 * on the welcome screen, which is before step 1 rather than part of it. */
function StepStrip({ stage }: { stage: Stage }) {
  const current = STEPS.findIndex((s) => s.stage === stage);

  return (
    <div className="flex items-stretch border-b border-border">
      {STEPS.map((step, i) => {
        const state = i < current ? "done" : i === current ? "current" : "todo";
        return (
          <span
            key={step.num}
            aria-current={state === "current" ? "step" : undefined}
            className={cn(
              "flex flex-1 items-center gap-2 border-r border-border px-3 py-2.5 last:border-r-0",
              state === "current" && "bg-background shadow-[inset_0_-2px_0_var(--primary)]",
            )}
          >
            <span
              aria-hidden
              className={cn(
                "grid size-4 shrink-0 place-items-center font-mono text-xs font-semibold",
                state === "todo"
                  ? "bg-muted text-muted-foreground"
                  : "bg-primary text-primary-foreground",
              )}
            >
              {state === "done" ? <Check className="size-3" /> : step.num}
            </span>
            <span
              className={cn(
                "font-mono text-xs tracking-wider uppercase",
                state === "current" && "font-semibold",
                state === "todo" && "text-muted-foreground",
              )}
            >
              {step.label}
            </span>
          </span>
        );
      })}
    </div>
  );
}

/** One line of the final report: what happened, and what it means. */
function DoneCheck({ ok, label, detail }: { ok: boolean; label: string; detail: string }) {
  const Icon = ok ? Check : TriangleAlert;
  return (
    <div className="flex items-start gap-2.5">
      <Icon
        aria-hidden
        className={cn("mt-0.5 size-4 shrink-0", ok ? "text-success" : "text-warning")}
      />
      <span className="flex min-w-0 flex-col gap-0.5">
        <span className="text-sm font-medium">{label}</span>
        <span className="text-xs text-pretty text-muted-foreground">{detail}</span>
      </span>
    </div>
  );
}

/**
 * First-run setup wizard: welcome, create the admin account, optionally seed
 * starter blocklists, then report what landed.
 *
 * POST /setup only creates the account — it does not start a session. Once
 * it succeeds, the wizard silently signs in (useSetupSignIn) to get a
 * session cookie for the optional list writes, but deliberately does not
 * invalidate the `me`/`setup` queries until the very end: doing so earlier
 * would flip the app's auth gate (App.tsx) out from under this component
 * mid-wizard. If the silent sign-in fails, the list step is skipped and the
 * last screen points at the login page instead.
 */
export function Setup() {
  const qc = useQueryClient();
  const [stage, setStage] = useState<Stage>("welcome");
  const [formError, setFormError] = useState<{ conflict: boolean; message: string } | null>(null);
  const [outcome, setOutcome] = useState<Outcome>({
    applied: 0,
    chosen: 0,
    failed: [],
    signedIn: false,
  });
  const [username, setUsername] = useState("");
  const [selected, setSelected] = useState<Record<string, boolean>>(
    Object.fromEntries(STARTER_LISTS.map((l) => [l.key, l.default])),
  );

  const setup = useSetup();
  const signIn = useSetupSignIn();
  const addList = useAddList();
  const assignGroupLists = useAssignGroupLists();

  const accountForm = useForm<AccountFormValues>({
    resolver: zodResolver(accountFormSchema),
    defaultValues: { username: "", password: "", confirmPassword: "" },
  });

  const creatingAccount = setup.isPending || signIn.isPending;
  const applyingStarters = addList.isPending || assignGroupLists.isPending;
  const chosenCount = STARTER_LISTS.filter((l) => selected[l.key]).length;

  function onCreateAccount(values: AccountFormValues) {
    setFormError(null);
    setup.mutate(
      { username: values.username, password: values.password },
      {
        onSuccess: () => {
          setUsername(values.username);
          toast.success("Admin account created");
          signIn.mutate(
            { username: values.username, password: values.password },
            {
              onSuccess: () => {
                setOutcome((o) => ({ ...o, signedIn: true }));
                setStage("lists");
              },
              onError: () => {
                setOutcome((o) => ({ ...o, signedIn: false }));
                setStage("done");
              },
            },
          );
        },
        onError: (err) => {
          if (err instanceof ApiError && err.status === 409) {
            setFormError({
              conflict: true,
              message: "An admin account has already been created for this instance.",
            });
            toast.error("An admin account already exists");
            return;
          }
          const message =
            err instanceof ApiError ? err.message : "Couldn't create the account — try again";
          setFormError({ conflict: false, message });
          toast.error(message);
        },
      },
    );
  }

  function goToLogin() {
    void qc.invalidateQueries({ queryKey: authKeys.setup });
  }

  function goToDashboard() {
    void qc.invalidateQueries({ queryKey: authKeys.me });
  }

  /**
   * Best-effort, and honest about how far it got.
   *
   * A list that is created but never assigned to a group is dead weight — it
   * exists in the catalog and filters nothing. So a failure partway through
   * the loop does not abandon the ids already created: whatever was created
   * still gets attached, and the last screen says what actually landed
   * rather than implying the whole step was a no-op.
   */
  async function applyStarterLists() {
    const chosen = STARTER_LISTS.filter((l) => selected[l.key]);
    const created: number[] = [];
    const failed: string[] = [];

    for (const list of chosen) {
      try {
        // The wizard already has human labels for these; pass them through
        // as the list's name rather than letting the server re-derive one.
        const res = await addList.mutateAsync({ url: list.url, kind: "block", name: list.name });
        created.push(res.id);
      } catch {
        failed.push(list.name);
      }
    }

    let applied = created.length;
    if (created.length > 0) {
      try {
        await assignGroupLists.mutateAsync({ groupId: STARTER_GROUP_ID, listIds: created });
      } catch {
        // Created but unattached filters nothing, so this counts as zero
        // applied — and the last screen has to say so.
        applied = 0;
      }
    }

    setOutcome((o) => ({ ...o, applied, chosen: chosen.length, failed }));
    setStage("done");
  }

  const partial = outcome.applied < outcome.chosen;

  // The address the admin reached this page on is the best guess at what
  // their router should point at — dnsaur has no endpoint that reports its
  // own LAN address, and the browser already resolved one that works.
  const resolverHost = typeof window === "undefined" ? "dnsaur" : window.location.hostname;

  // The other three footers each say something an operator can act on. The
  // welcome screen has no such line — what the artboard put there described
  // an API endpoint, which is the app talking about itself.
  const COPY: Record<Stage, { eyebrow: string; title: string; footer?: string; width: string }> = {
    welcome: {
      eyebrow: "First run",
      title: "Set up dnsaur",
      width: "max-w-md",
    },
    admin: {
      eyebrow: "First run",
      title: "Create your admin",
      footer: "Stored as an argon2 hash. dnsaur never sees it again.",
      width: "max-w-md",
    },
    lists: {
      eyebrow: "First run",
      title: "Pick starter blocklists",
      footer: "Each list is added separately, so one failure won't stop the others.",
      width: "max-w-lg",
    },
    done: {
      eyebrow: "First run",
      title: "dnsaur is ready",
      footer: "Nothing is filtered until clients actually query dnsaur.",
      width: "max-w-md",
    },
  };
  const copy = COPY[stage];

  return (
    <AuthLayout
      eyebrow={copy.eyebrow}
      title={copy.title}
      footer={copy.footer}
      maxWidthClassName={copy.width}
    >
      <Card className="gap-0 p-0">
        {/* Absent on welcome: that screen is before step 1, not part of it. */}
        {stage !== "welcome" && <StepStrip stage={stage} />}

        <div className="flex flex-col gap-4 p-5">
          {stage === "welcome" && (
            <>
              <p className="text-sm text-pretty text-muted-foreground">
                No admin account exists yet, so this instance is unclaimed. Three steps: create an
                account, pick starter blocklists, then point your router at dnsaur.
              </p>
              <Button type="button" onClick={() => setStage("admin")}>
                Get started
              </Button>
            </>
          )}

          {stage === "admin" && (
            <>
              <p className="text-sm text-pretty text-muted-foreground">
                The only account, with full write access. You can add API tokens later.
              </p>

              {formError && (
                <Alert variant={formError.conflict ? "info" : "destructive"}>
                  <CircleAlert />
                  <AlertTitle>
                    {formError.conflict ? "Account already exists" : "Couldn't create account"}
                  </AlertTitle>
                  <AlertDescription>
                    <p>{formError.message}</p>
                    {formError.conflict && (
                      <Button type="button" variant="outline" size="sm" onClick={goToLogin}>
                        Go to login
                      </Button>
                    )}
                  </AlertDescription>
                </Alert>
              )}

              <Form {...accountForm}>
                <form
                  className="flex flex-col gap-4"
                  onSubmit={(e) => void accountForm.handleSubmit(onCreateAccount)(e)}
                  noValidate
                >
                  <FormField
                    control={accountForm.control}
                    name="username"
                    render={({ field }) => (
                      <FormItem>
                        <FormLabel>Username</FormLabel>
                        <FormControl>
                          <Input {...field} autoComplete="username" />
                        </FormControl>
                        <FormMessage />
                      </FormItem>
                    )}
                  />
                  <FormField
                    control={accountForm.control}
                    name="password"
                    render={({ field }) => (
                      <FormItem>
                        <FormLabel>Password</FormLabel>
                        <FormControl>
                          <Input {...field} type="password" autoComplete="new-password" />
                        </FormControl>
                        <FormDescription>
                          At least 8 characters. There&apos;s no recovery flow yet — store it
                          somewhere safe.
                        </FormDescription>
                        <FormMessage />
                      </FormItem>
                    )}
                  />
                  {/* Not in the artboard, kept on purpose: with no recovery
                      flow, a typo here costs shell access to undo. */}
                  <FormField
                    control={accountForm.control}
                    name="confirmPassword"
                    render={({ field }) => (
                      <FormItem>
                        <FormLabel>Confirm password</FormLabel>
                        <FormControl>
                          <Input {...field} type="password" autoComplete="new-password" />
                        </FormControl>
                        <FormMessage />
                      </FormItem>
                    )}
                  />
                  <div className="flex flex-col gap-2">
                    <Button type="submit" disabled={creatingAccount}>
                      {creatingAccount ? "Creating account…" : "Create account"}
                    </Button>
                    <Button
                      type="button"
                      variant="ghost"
                      onClick={() => setStage("welcome")}
                      disabled={creatingAccount}
                    >
                      Back
                    </Button>
                  </div>
                </form>
              </Form>
            </>
          )}

          {stage === "lists" && (
            <>
              <p className="text-sm text-pretty text-muted-foreground">
                Sensible defaults for a home network. All optional.
              </p>

              <fieldset
                disabled={applyingStarters}
                className="flex flex-col border border-border bg-background disabled:opacity-60"
              >
                <legend className="sr-only">Starter blocklists</legend>
                {STARTER_LISTS.map((list) => {
                  const checked = selected[list.key] ?? false;
                  return (
                    <label
                      key={list.key}
                      className={cn(
                        "flex cursor-pointer items-center gap-3 border-b border-border-muted p-3 last:border-b-0",
                        checked && "bg-primary/5",
                      )}
                    >
                      <Checkbox
                        checked={checked}
                        onCheckedChange={(next) =>
                          setSelected((prev) => ({ ...prev, [list.key]: next === true }))
                        }
                      />
                      <span className="flex min-w-0 flex-1 flex-col gap-0.5">
                        <span className="text-sm font-medium">{list.name}</span>
                        <span className="text-xs text-pretty text-muted-foreground">
                          {list.description}
                        </span>
                      </span>
                      <span className="shrink-0 font-mono text-xs text-muted-foreground">
                        {list.size}
                      </span>
                    </label>
                  );
                })}
              </fieldset>

              <p className="text-xs text-pretty text-muted-foreground">
                Lists download in the background — entry counts stay at zero until the first fetch
                lands. You can add or remove any of these later.
              </p>

              <div className="flex flex-col gap-2">
                <Button
                  type="button"
                  onClick={() => void applyStarterLists()}
                  disabled={applyingStarters || chosenCount === 0}
                >
                  {applyingStarters
                    ? "Adding…"
                    : `Add ${chosenCount} ${chosenCount === 1 ? "list" : "lists"} and finish`}
                </Button>
                <Button
                  type="button"
                  variant="ghost"
                  onClick={() => {
                    setOutcome((o) => ({ ...o, applied: 0, chosen: 0, failed: [] }));
                    setStage("done");
                  }}
                  disabled={applyingStarters}
                >
                  Skip for now
                </Button>
              </div>
            </>
          )}

          {stage === "done" && (
            <>
              {partial && (
                <Alert variant="warning">
                  <TriangleAlert />
                  <AlertTitle>
                    {outcome.applied} of {outcome.chosen} lists added
                  </AlertTitle>
                  <AlertDescription>
                    {outcome.failed.length > 0
                      ? `${outcome.failed.join(", ")} couldn't be added.`
                      : "The lists were created but couldn't be applied to the default group, so they are filtering nothing."}{" "}
                    Everything else is in place and dnsaur is already resolving. You can retry or
                    remove them from Filtering → Lists.
                  </AlertDescription>
                </Alert>
              )}

              <div className="flex flex-col gap-3">
                <DoneCheck
                  ok
                  label="Admin account created"
                  detail={`${username} · ${outcome.signedIn ? "signed in now" : "sign in to continue"}. 2FA can be added from Account & security.`}
                />
                {outcome.chosen > 0 && (
                  <DoneCheck
                    ok={!partial}
                    label={
                      partial
                        ? `${outcome.applied} of ${outcome.chosen} blocklists added`
                        : `${outcome.applied} blocklists added`
                    }
                    detail={
                      partial
                        ? "The rest were skipped — add them from Filtering → Lists."
                        : "Downloading now — counts appear once the first fetch lands."
                    }
                  />
                )}
                <DoneCheck
                  ok
                  label="Resolver is answering"
                  detail="Upstreams 1.1.1.1, 1.0.0.1 and 9.9.9.9, racing for the first reply."
                />
              </div>

              <div className="flex flex-col gap-1 border border-border bg-background p-3">
                <span className="font-mono text-xs tracking-widest text-muted-foreground uppercase">
                  Point your router here
                </span>
                <span className="font-mono text-sm">
                  {resolverHost}
                  <span className="text-muted-foreground">:53</span>
                </span>
              </div>

              <Button type="button" onClick={outcome.signedIn ? goToDashboard : goToLogin}>
                {outcome.signedIn ? "Open the dashboard" : "Go to login"}
              </Button>
            </>
          )}
        </div>
      </Card>
    </AuthLayout>
  );
}
