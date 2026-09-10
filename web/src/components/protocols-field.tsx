import { useId } from "react";
import { Check } from "lucide-react";
import { useController, type Control } from "react-hook-form";
import { Input, Label, cn } from "@e412/rnui-react";
import type { CertificateStatus, ProtocolStatus } from "../api/types";
import { useResolverStatus } from "../hooks/use-settings";
import { rhfName } from "../lib/rhf-name";
import { expiringSoonDetail, servingState } from "../lib/serving";

/**
 * The Protocols settings group — see
 * .superpowers/sdd/2026-09-08-encrypted-serving-e2/protocols-artboard.md,
 * the authoritative source for this file's layout, colours and copy.
 *
 * Every other group's fields decompose into independent label+input rows
 * (settings.tsx's SettingRow). These six don't: a protocol's checkbox and
 * its reality line are one fact told two ways (spec §8 — a checked box
 * alone is exactly the lie the encryption-downgrade banner was built to
 * prevent), and the certificate's two paths share one verification line.
 * So this component owns the whole group body, wired directly to
 * settings.tsx's form via `control` — see settings.tsx's `render` field on
 * SettingGroup — rather than being decomposed into six SettingRow calls.
 *
 * Bypassing SettingRow means bypassing rnui's FormField/FormLabel too:
 * those read a "which field is this" context from <FormField>, which
 * nothing here renders (the six controls aren't laid out as six generic
 * FormItems). `Label` (the plain, context-free base component) plus a
 * manual `htmlFor`/`useId()` pair gets the identical Space-Grotesk-medium
 * typography without that dependency.
 */

const DOT_ENABLED = "serve.dot.enabled";
const DOT_LISTEN = "serve.dot.listen";
const DOH_ENABLED = "serve.doh.enabled";
const DOH_LISTEN = "serve.doh.listen";
const TLS_CERT = "serve.tls.cert";
const TLS_KEY = "serve.tls.key";

/** The reality line's glyph and text for one protocol row (proto()'s
 * `glyph`/`stateColor`/`stateText` in the artboard) — built from the real
 * API status, not the artboard's placeholder example, per the artboard's
 * own note: "The bind text after the em dash is the real error from the
 * API, not a fixed string."
 *
 * The artboard's three states plus a fourth it could not know about:
 * `undefined`, meaning GET /resolver/status has not answered — it is still
 * in flight, or it failed and the query has given up. "off" was the wrong
 * answer for both. The permanent case is the dangerous one: a DoT listener
 * happily serving on :853 read as `☑ DNS-over-TLS … ○ off`, and the
 * operator's natural remedy — untick, re-tick — takes a working listener
 * down. */
function realityLine(reality: ProtocolStatus | undefined): {
  glyph: string;
  text: string;
  colorClass: string;
} {
  if (!reality) {
    return { glyph: "◌", text: "status unavailable", colorClass: "text-muted-foreground" };
  }
  const state = servingState(reality);
  if (state === "off") {
    return { glyph: "○", text: "off", colorClass: "text-muted-foreground" };
  }
  if (state === "listening") {
    return {
      glyph: "●",
      text: `listening on ${reality.addr}`,
      colorClass: "text-success-foreground",
    };
  }
  return {
    glyph: "●",
    text: reality.error ? `not listening — ${reality.error}` : "not listening",
    colorClass: "text-destructive-foreground",
  };
}

function ProtocolRow({
  name,
  enabledKey,
  listenKey,
  enabledField,
  listenField,
  reality,
}: {
  name: string;
  enabledKey: string;
  listenKey: string;
  enabledField: ReturnType<typeof useController>["field"];
  listenField: ReturnType<typeof useController>["field"];
  reality: ProtocolStatus | undefined;
}) {
  // Destructured, then spread, rather than picked apart prop by prop. Naming
  // `.ref` in JSX is what tells React Compiler this object is a ref, after
  // which every other read of it during render is an error (react(refs)) and
  // the component is dropped from memoization. The value and the onChange are
  // the two this input genuinely does differently — a checkbox reads
  // "true"/"false" as checked, and writes it back the same way — so they come
  // out and the rest goes on whole.
  const { value: enabledValue, onChange: onEnabledChange, ...enabledRest } = enabledField;
  const enabled = enabledValue === "true";
  const line = realityLine(reality);

  return (
    <fieldset
      aria-label={name}
      className={cn(
        "flex min-w-0 flex-col gap-[9px] border border-border px-3 py-[11px]",
        !enabled && "bg-black/[0.015] dark:bg-white/[0.02]",
      )}
    >
      <label className="flex min-w-0 cursor-pointer items-center gap-[9px]">
        <input
          {...enabledRest}
          type="checkbox"
          checked={enabled}
          onChange={(e) => onEnabledChange(e.target.checked ? "true" : "false")}
          className="peer sr-only"
        />
        <span
          aria-hidden
          className={cn(
            "flex size-[13px] flex-none items-center justify-center border",
            enabled ? "border-primary bg-primary" : "border-border bg-transparent",
          )}
        >
          {enabled && <Check className="size-[9px] stroke-[3.4] text-primary-foreground" />}
        </span>
        <span className="font-mono text-[12.5px] leading-none font-semibold whitespace-nowrap">
          {name}
        </span>
        <span className="truncate font-mono text-[9.5px] leading-none text-muted-foreground">
          {enabledKey}
        </span>
      </label>

      <span className="flex items-center gap-[9px]">
        <Input
          aria-label={`${name} listen address`}
          {...listenField}
          autoComplete="off"
          spellCheck={false}
          className="w-[200px] font-mono text-xs"
        />
        <span className="font-mono text-[9.5px] leading-none whitespace-nowrap text-muted-foreground">
          {listenKey}
        </span>
      </span>

      <span className="flex min-w-0 items-start gap-[7px]">
        <span
          aria-hidden
          className={cn("flex-none font-mono text-[11.5px] leading-[1.35]", line.colorClass)}
        >
          {line.glyph}
        </span>
        <span className={cn("text-pretty font-mono text-[11.5px] leading-[1.35]", line.colorClass)}>
          {line.text}
        </span>
      </span>
    </fieldset>
  );
}

/** The certificate verification line's states — the artboard's four, plus
 * `unknown` for a status that has not answered (see realityLine). Saying
 * "No certificate loaded." when the endpoint is failing states as fact
 * something nobody currently knows, next to a listener that may well be
 * serving one. See the artboard's "Certificate line states, verbatim"
 * table for the rest. `valid`/`expiring` come
 * straight off GET /resolver/status's `certificate` (present, and whether
 * it's within the warning window); `unreadable` comes from the *save-time*
 * rejection (validateCrossField's tls.LoadX509KeyPair error, surfaced
 * through this form's fieldState — see settings.tsx's onSubmit) because
 * the API only ever reports a certificate that loaded, never why one
 * didn't; `none` is the fallback when neither of those has anything to
 * say, which is also the correct read for a fresh install with nothing
 * configured yet. */
function certLine(
  certificate: CertificateStatus | undefined,
  errorMessage: string,
  statusKnown: boolean,
): { text: string; colorClass: string } {
  if (certificate) {
    if (certificate.expiring_soon) {
      const { days, date, expired } = expiringSoonDetail(certificate);
      return {
        text: expired
          ? `Expired — ${date}`
          : `Expires in ${days} day${days === 1 ? "" : "s"} — ${date}`,
        colorClass: expired ? "text-destructive-foreground" : "text-warning-foreground",
      };
    }
    const { date } = expiringSoonDetail(certificate);
    return { text: `Expires ${date}`, colorClass: "text-muted-foreground" };
  }
  if (errorMessage) {
    return { text: errorMessage, colorClass: "text-destructive-foreground" };
  }
  // A save-time rejection is still worth showing when the status is
  // unknown — it came from this form, not from the endpoint that is not
  // answering — which is why this sits below errorMessage rather than
  // above it.
  if (!statusKnown) {
    return { text: "Status unavailable.", colorClass: "text-muted-foreground" };
  }
  return { text: "No certificate loaded.", colorClass: "text-muted-foreground" };
}

export function ProtocolsField({ control }: { control: Control<Record<string, string>> }) {
  const status = useResolverStatus();
  const serving = status.data?.serving;

  const dotEnabled = useController({ control, name: rhfName(DOT_ENABLED) });
  const dotListen = useController({ control, name: rhfName(DOT_LISTEN) });
  const dohEnabled = useController({ control, name: rhfName(DOH_ENABLED) });
  const dohListen = useController({ control, name: rhfName(DOH_LISTEN) });
  const cert = useController({ control, name: rhfName(TLS_CERT) });
  const key = useController({ control, name: rhfName(TLS_KEY) });

  const certId = useId();
  const keyId = useId();

  // A failed save of either certificate path is the only source for
  // "unreadable" — see certLine's doc comment. Both are shown (rather than
  // just the first) because a broken pair can fail both halves of a single
  // save at once, and each half's message names the specific file that
  // wouldn't read.
  const certErrorMessage = [cert.fieldState.error?.message, key.fieldState.error?.message]
    .filter((m): m is string => Boolean(m))
    .join(" ");
  const certBroken = certErrorMessage !== "";
  const line = certLine(status.data?.certificate, certErrorMessage, status.data !== undefined);

  return (
    <div className="flex flex-col gap-3">
      <div className="grid grid-cols-2 gap-[14px]">
        <ProtocolRow
          name="DNS-over-TLS"
          enabledKey={DOT_ENABLED}
          listenKey={DOT_LISTEN}
          enabledField={dotEnabled.field}
          listenField={dotListen.field}
          reality={serving?.dot}
        />
        <ProtocolRow
          name="DNS-over-HTTPS"
          enabledKey={DOH_ENABLED}
          listenKey={DOH_LISTEN}
          enabledField={dohEnabled.field}
          listenField={dohListen.field}
          reality={serving?.doh}
        />
      </div>

      <div className="flex flex-col gap-2.5 border border-border p-3">
        <div className="grid grid-cols-2 gap-[14px]">
          <div className="flex min-w-0 flex-col gap-2">
            <div className="flex items-baseline gap-2">
              <Label htmlFor={certId}>Certificate</Label>
              <span className="font-mono text-xs text-muted-foreground">{TLS_CERT}</span>
            </div>
            <Input
              id={certId}
              {...cert.field}
              placeholder="/etc/letsencrypt/live/adam.dns.e412.in/fullchain.pem"
              autoComplete="off"
              spellCheck={false}
              aria-invalid={certBroken}
              className="font-mono text-xs"
            />
          </div>
          <div className="flex min-w-0 flex-col gap-2">
            <div className="flex items-baseline gap-2">
              <Label htmlFor={keyId}>Private key</Label>
              <span className="font-mono text-xs text-muted-foreground">{TLS_KEY}</span>
            </div>
            <Input
              id={keyId}
              {...key.field}
              placeholder="/etc/letsencrypt/live/adam.dns.e412.in/privkey.pem"
              autoComplete="off"
              spellCheck={false}
              aria-invalid={certBroken}
              className="font-mono text-xs"
            />
          </div>
        </div>
        <span className={cn("text-pretty font-mono text-[11.5px] leading-[1.35]", line.colorClass)}>
          {line.text}
        </span>
      </div>

      <p className="text-[11.5px] leading-[1.45] text-pretty text-muted-foreground">
        Set the certificate before enabling a protocol.
      </p>
    </div>
  );
}
