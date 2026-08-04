import { useId, useState } from "react";
import { CheckIcon, CircleAlert } from "lucide-react";
import { toast } from "sonner";
import { useForm } from "react-hook-form";
import { useQueryClient } from "@tanstack/react-query";
import {
  Alert,
  AlertDescription,
  AlertTitle,
  Button,
  Card,
  CardContent,
  CardDescription,
  CardHeader,
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
  Label,
  Stepper,
  StepperContent,
  StepperIndicator,
  StepperItem,
  StepperNav,
  StepperPanel,
  StepperSeparator,
  StepperTitle,
  StepperTrigger,
} from "@e412/rnui-react";
import { ApiError } from "../api/client";
import { AuthLayout } from "../components/auth-layout";
import { authKeys, useSetup, useSetupSignIn } from "../hooks/use-auth";
import { useAddList, useAssignGroupLists } from "../hooks/use-filters";
import { useUpdateSetting } from "../hooks/use-settings";

const DEFAULT_UPSTREAMS = "1.1.1.1:53,1.0.0.1:53,9.9.9.9:53";
const STARTER_GROUP_ID = 1;

const STARTER_LISTS = [
  {
    key: "stevenblack",
    name: "StevenBlack — Unified hosts",
    url: "https://raw.githubusercontent.com/StevenBlack/hosts/master/hosts",
    description: "A broad, well-maintained baseline of ad and malware domains.",
  },
  {
    key: "hagezi",
    name: "HaGeZi — Multi PRO",
    url: "https://raw.githubusercontent.com/hagezi/dns-blocklists/main/hosts/pro.txt",
    description: "A more aggressive list covering trackers, ads, and scams.",
  },
] as const;

interface AccountFormValues {
  username: string;
  password: string;
  confirmPassword: string;
}

function passwordStrengthHint(password: string): string {
  if (password.length === 0) return "Use at least 8 characters.";
  if (password.length < 8) return "Too short — needs at least 8 characters.";
  const variety = [/[a-z]/, /[A-Z]/, /\d/, /[^a-zA-Z0-9]/].filter((re) => re.test(password)).length;
  if (password.length >= 14 && variety >= 3) return "Strong password.";
  if (password.length >= 10 && variety >= 2) return "Good password.";
  return "Okay — a longer or more varied password is stronger.";
}

/**
 * First-run setup wizard: create the admin account, then optionally seed a
 * couple of starter blocklists and upstream resolvers.
 *
 * POST /setup only creates the account — it does not start a session. Once
 * it succeeds, the wizard silently signs in (useSetupSignIn) to get a
 * session cookie for the optional step-2 writes, but deliberately does not
 * invalidate the `me`/`setup` queries until the very end: doing so earlier
 * would flip the app's auth gate (App.tsx) out from under this component
 * mid-wizard. If the silent sign-in fails for any reason, step 2 is skipped
 * and step 3 points the user at the login page instead.
 */
export function Setup() {
  const qc = useQueryClient();
  const [step, setStep] = useState(1);
  const [formError, setFormError] = useState<{ conflict: boolean; message: string } | null>(null);
  const [loggedIn, setLoggedIn] = useState(false);

  const setup = useSetup();
  const signIn = useSetupSignIn();
  const addList = useAddList();
  const assignGroupLists = useAssignGroupLists();
  const updateSetting = useUpdateSetting();

  const accountForm = useForm<AccountFormValues>({
    defaultValues: { username: "", password: "", confirmPassword: "" },
  });

  const [upstreams, setUpstreams] = useState(DEFAULT_UPSTREAMS);
  const [selectedLists, setSelectedLists] = useState<Record<string, boolean>>({
    stevenblack: true,
    hagezi: true,
  });
  const upstreamsId = useId();

  const creatingAccount = setup.isPending || signIn.isPending;
  const applyingStarters =
    addList.isPending || assignGroupLists.isPending || updateSetting.isPending;

  function onCreateAccount(values: AccountFormValues) {
    setFormError(null);
    setup.mutate(
      { username: values.username, password: values.password },
      {
        onSuccess: () => {
          toast.success("Admin account created");
          signIn.mutate(
            { username: values.username, password: values.password },
            {
              onSuccess: () => {
                setLoggedIn(true);
                setStep(2);
              },
              onError: () => {
                setLoggedIn(false);
                setStep(3);
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

  async function applyStarterSetup() {
    try {
      const chosen = STARTER_LISTS.filter((l) => selectedLists[l.key]);
      const ids: number[] = [];
      for (const list of chosen) {
        const created = await addList.mutateAsync({ url: list.url, kind: "block" });
        ids.push(created.id);
      }
      if (ids.length > 0) {
        await assignGroupLists.mutateAsync({ groupId: STARTER_GROUP_ID, listIds: ids });
      }
      if (upstreams.trim().length > 0) {
        await updateSetting.mutateAsync({ key: "upstreams", value: upstreams.trim() });
      }
      toast.success("Starter blocklists and upstreams saved");
    } catch {
      toast.error("Couldn't save starter setup — you can add lists later in Filtering");
    } finally {
      setStep(3);
    }
  }

  return (
    <AuthLayout eyebrow="First-time setup" title="Set up dnsaur" maxWidthClassName="max-w-lg">
      <Card className="gap-6 py-6">
        <Stepper value={step} indicators={{ completed: <CheckIcon className="size-3.5" /> }}>
          <CardHeader className="px-6 pb-2">
            <StepperNav>
              <StepperItem step={1} disabled>
                <StepperTrigger>
                  <StepperIndicator>1</StepperIndicator>
                  <StepperTitle>Account</StepperTitle>
                </StepperTrigger>
                <StepperSeparator />
              </StepperItem>
              <StepperItem step={2} disabled>
                <StepperTrigger>
                  <StepperIndicator>2</StepperIndicator>
                  <StepperTitle>Starter setup</StepperTitle>
                </StepperTrigger>
                <StepperSeparator />
              </StepperItem>
              <StepperItem step={3} disabled>
                <StepperTrigger>
                  <StepperIndicator>3</StepperIndicator>
                  <StepperTitle>Done</StepperTitle>
                </StepperTrigger>
              </StepperItem>
            </StepperNav>
          </CardHeader>

          <CardContent className="px-6">
            <StepperPanel>
              <StepperContent value={1}>
                <CardDescription className="mb-4">
                  Create the admin account that manages this dnsaur instance.
                </CardDescription>

                {formError && (
                  <Alert variant="destructive" className="mb-4">
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
                      rules={{ required: "Username is required" }}
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
                      rules={{
                        required: "Password is required",
                        minLength: {
                          value: 8,
                          message: "Password must be at least 8 characters",
                        },
                      }}
                      render={({ field }) => (
                        <FormItem>
                          <FormLabel>Password</FormLabel>
                          <FormControl>
                            <Input {...field} type="password" autoComplete="new-password" />
                          </FormControl>
                          <FormDescription>{passwordStrengthHint(field.value)}</FormDescription>
                          <FormMessage />
                        </FormItem>
                      )}
                    />
                    <FormField
                      control={accountForm.control}
                      name="confirmPassword"
                      rules={{
                        required: "Confirm your password",
                        validate: (value, formValues) =>
                          value === formValues.password || "Passwords don't match",
                      }}
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
                    <Button type="submit" disabled={creatingAccount}>
                      {creatingAccount ? "Creating account…" : "Create account"}
                    </Button>
                  </form>
                </Form>
              </StepperContent>

              <StepperContent value={2}>
                <CardDescription className="mb-4">
                  Give dnsaur a head start with a couple of curated blocklists and default upstream
                  resolvers. You can change any of this later in Filtering and Settings.
                </CardDescription>

                <div className="flex flex-col gap-5">
                  <fieldset
                    disabled={applyingStarters}
                    className="flex flex-col gap-2.5 disabled:opacity-60"
                  >
                    <legend className="mb-0.5 text-sm font-medium text-foreground">
                      Starter blocklists
                    </legend>
                    {STARTER_LISTS.map((list) => {
                      const checked = selectedLists[list.key] ?? false;
                      return (
                        <label
                          key={list.key}
                          htmlFor={`list-${list.key}`}
                          className={cn(
                            "flex cursor-pointer items-start gap-3 rounded-lg border p-3 transition-colors",
                            checked
                              ? "border-primary/30 bg-primary/5"
                              : "border-border hover:bg-muted/50",
                          )}
                        >
                          <Checkbox
                            id={`list-${list.key}`}
                            checked={checked}
                            onCheckedChange={(next) =>
                              setSelectedLists((prev) => ({ ...prev, [list.key]: next }))
                            }
                            className="mt-0.5"
                          />
                          <span className="flex flex-col gap-0.5">
                            <span className="text-sm font-medium text-foreground">{list.name}</span>
                            <span className="text-sm text-muted-foreground">
                              {list.description}
                            </span>
                          </span>
                        </label>
                      );
                    })}
                  </fieldset>

                  <div className="flex flex-col gap-1.5">
                    <Label htmlFor={upstreamsId}>Upstream DNS servers</Label>
                    <Input
                      id={upstreamsId}
                      value={upstreams}
                      onChange={(e) => setUpstreams(e.target.value)}
                      disabled={applyingStarters}
                      className="font-mono text-sm"
                    />
                    <p className="text-sm text-muted-foreground">
                      Comma-separated host:port pairs, tried in order.
                    </p>
                  </div>

                  <div className="flex flex-wrap items-center justify-between gap-3">
                    <Button
                      type="button"
                      variant="ghost"
                      onClick={() => setStep(3)}
                      disabled={applyingStarters}
                    >
                      I&apos;ll do this later
                    </Button>
                    <Button
                      type="button"
                      onClick={() => void applyStarterSetup()}
                      disabled={applyingStarters}
                    >
                      {applyingStarters ? "Saving…" : "Continue"}
                    </Button>
                  </div>
                </div>
              </StepperContent>

              <StepperContent value={3}>
                {loggedIn ? (
                  <div className="flex flex-col items-center gap-5 py-6 text-center">
                    <span className="relative flex size-14 items-center justify-center">
                      <span className="absolute inset-0 rounded-full bg-info/50 motion-safe:animate-ping motion-safe:[animation-iteration-count:1]" />
                      <span className="relative flex size-14 items-center justify-center rounded-full bg-primary text-primary-foreground">
                        <CheckIcon className="size-6" />
                      </span>
                    </span>
                    <div className="flex flex-col gap-1">
                      <h2 className="text-lg font-semibold text-foreground">You&apos;re all set</h2>
                      <p className="text-sm text-muted-foreground">
                        dnsaur is listening — head to the dashboard to see it in action.
                      </p>
                    </div>
                    <Button type="button" onClick={goToDashboard}>
                      Go to dashboard
                    </Button>
                  </div>
                ) : (
                  <div className="flex flex-col items-center gap-5 py-6 text-center">
                    <span className="flex size-14 items-center justify-center rounded-full bg-muted text-muted-foreground">
                      <CheckIcon className="size-6" />
                    </span>
                    <div className="flex flex-col gap-1">
                      <h2 className="text-lg font-semibold text-foreground">Account created</h2>
                      <p className="text-sm text-muted-foreground">
                        Sign in with your new account to continue.
                      </p>
                    </div>
                    <Button type="button" onClick={goToLogin}>
                      Go to login
                    </Button>
                  </div>
                )}
              </StepperContent>
            </StepperPanel>
          </CardContent>
        </Stepper>
      </Card>
    </AuthLayout>
  );
}
