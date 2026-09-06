import { useEffect, useRef, useState } from "react";
import { Minus, Plus } from "lucide-react";
import { Button, cn, Input } from "@e412/rnui-react";
import {
  buildUpstream,
  parseUpstreams,
  type UpstreamErrorCode,
  type UpstreamScheme,
} from "../lib/upstreams";
import { isValidIPv4, isValidIPv6 } from "../lib/schemas";
import { WarningStrip } from "./warning-strip";

/**
 * The structured editor for the `upstreams` setting — see
 * .superpowers/sdd/2026-09-05-encrypted-upstreams-e1/task-9-brief.md for the
 * artboard this follows.
 *
 * The setting's wire format is one comma-separated string (unchanged: the
 * server still stores and validates exactly that string, via
 * internal/upstream/addr.go's grammar, mirrored here by parseUpstreams). This
 * component's whole job is presenting that string as a transport choice plus
 * a table of address/name pairs, and never handing back a string
 * `parseUpstreams` would reject — see `emit` below.
 */

type Transport = "plain" | "tls" | "https";
type PresetName = "cloudflare" | "quad9" | "google" | "custom";

interface Row {
  /** React key. Rows hold focused text inputs, so identity has to survive a
   * removal: keyed by array index, removing row *i* makes React reuse that
   * DOM node for what was row *i+1*, and the caret jumps to a different
   * row's value mid-edit. Never rendered, never emitted — `emit` builds the
   * wire string from address/serverName/path alone. */
  id: string;
  address: string;
  serverName: string;
  path: string;
}

// A counter rather than crypto.randomUUID(): ids only have to be unique
// within one mounted editor, and a counter is available everywhere and
// deterministic in tests.
let rowSeq = 0;
function rowID(): string {
  rowSeq += 1;
  return `row-${rowSeq}`;
}

// Mirrors upstreams.ts's DEFAULT_DOH_PATH, which is not exported: this file
// only needs it to placeholder an empty Path cell and to recognise "the
// default path" when deciding whether a row matches a preset.
const DEFAULT_DOH_PATH = "/dns-query";

function blankRow(): Row {
  return { id: rowID(), address: "", serverName: "", path: "" };
}

function schemeFor(transport: Transport): UpstreamScheme {
  return transport === "plain" ? "udp" : transport;
}

const TRANSPORT_OPTIONS: { value: Transport; label: string; sample: string; hint: string }[] = [
  { value: "plain", label: "plain", sample: "1.1.1.1:53", hint: "Unencrypted." },
  {
    value: "tls",
    label: "tls",
    sample: "tls://1.1.1.1:853#name",
    hint: "DNS-over-TLS, port 853.",
  },
  {
    value: "https",
    label: "https",
    sample: "https://1.1.1.1/dns-query#name",
    hint: "DNS-over-HTTPS, port 443.",
  },
];

interface PresetRow {
  address: string;
  serverName: string;
  path?: string;
}

// The artboard's own preset data (task-9-brief.md's AUTHORITATIVE section).
// Each round-trips through parseUpstreams — pinned by
// upstreams-field.test.tsx and, independently, by lib/upstreams.test.ts's
// buildUpstream coverage.
const TLS_PRESETS: Record<Exclude<PresetName, "custom">, PresetRow[]> = {
  cloudflare: [
    { address: "1.1.1.1:853", serverName: "cloudflare-dns.com" },
    { address: "1.0.0.1:853", serverName: "cloudflare-dns.com" },
  ],
  quad9: [
    { address: "9.9.9.9:853", serverName: "dns.quad9.net" },
    { address: "149.112.112.112:853", serverName: "dns.quad9.net" },
  ],
  google: [
    { address: "8.8.8.8:853", serverName: "dns.google" },
    { address: "8.8.4.4:853", serverName: "dns.google" },
  ],
};

const HTTPS_PRESETS: Record<Exclude<PresetName, "custom">, PresetRow[]> = {
  cloudflare: [
    { address: "1.1.1.1:443", serverName: "cloudflare-dns.com", path: DEFAULT_DOH_PATH },
  ],
  quad9: [{ address: "9.9.9.9:443", serverName: "dns.quad9.net", path: DEFAULT_DOH_PATH }],
  google: [{ address: "8.8.8.8:443", serverName: "dns.google", path: DEFAULT_DOH_PATH }],
};

function presetsFor(transport: "tls" | "https") {
  return transport === "tls" ? TLS_PRESETS : HTTPS_PRESETS;
}

const PRESET_LABELS: Record<PresetName, string> = {
  cloudflare: "Cloudflare",
  quad9: "Quad9",
  google: "Google",
  custom: "Custom",
};

/**
 * The host portion of an address that may carry a `host:port` or
 * `[ipv6]:port` suffix — just enough to ask "does this look like an IP",
 * not a full parse. upstreams.ts owns the real grammar; this is only used to
 * decide, when leaving Plain, whether an address can carry over as-is.
 */
function hostOf(address: string): string {
  const trimmed = address.trim();
  if (trimmed.startsWith("[")) {
    const end = trimmed.indexOf("]");
    return end >= 0 ? trimmed.slice(1, end) : trimmed;
  }
  const i = trimmed.lastIndexOf(":");
  return i >= 0 ? trimmed.slice(0, i) : trimmed;
}

function isAddressIP(address: string): boolean {
  const host = hostOf(address);
  return host !== "" && (isValidIPv4(host) || isValidIPv6(host));
}

/** The hint beneath the table — a function of the transport alone, per the
 * artboard's correction: it is not the encrypted copy in every mode. */
function hintFor(transport: Transport): string {
  return transport === "plain"
    ? "Plain UDP, with TCP fallback on truncation."
    : "Addresses only — a name here is rejected.";
}

// Camp 1's rejection (spec §3, upstreams.ts's host_not_ip) gets the
// artboard's own short copy rather than parseUpstreams' longer message —
// the two files are allowed to word the same rejection differently (see
// upstreams.ts's module comment). Every other code falls back to
// parseUpstreams' own message, which is at least accurate even if unstyled.
const ROW_ERROR_MESSAGES: Partial<Record<UpstreamErrorCode, string>> = {
  host_not_ip: "Needs an address, not a name. Put the name in Server name.",
};

function errorTarget(code: UpstreamErrorCode): "address" | "serverName" {
  return code === "missing_name" ? "serverName" : "address";
}

interface RowError {
  index: number;
  message: string;
  target: "address" | "serverName";
}

/** Turns the wire string into what the editor shows: which transport, and
 * one row per entry. A value this component can't make sense of (mixed
 * pre-existing data, a hand-edited config) falls back to a single Plain row
 * holding the raw text — still visible and editable rather than vanishing,
 * and still caught by the field-level schema in settings.tsx if left as-is. */
function deriveState(value: string): { transport: Transport; rows: Row[] } {
  const trimmed = value.trim();
  if (trimmed === "") return { transport: "plain", rows: [blankRow()] };
  const result = parseUpstreams(trimmed);
  if (!result.ok) {
    return {
      transport: "plain",
      rows: [{ id: rowID(), address: trimmed, serverName: "", path: "" }],
    };
  }
  const firstScheme = result.entries[0].scheme;
  const transport: Transport = firstScheme === "udp" ? "plain" : firstScheme;
  return {
    transport,
    rows: result.entries.map((e) => ({
      id: rowID(),
      address: e.addr,
      serverName: e.verifyName,
      path: e.path,
    })),
  };
}

/** Which segmented button, if any, the current rows exactly match — derived
 * rather than stored, so editing a filled-in preset by hand correctly falls
 * back to "custom" instead of leaving a stale preset highlighted. */
function activePreset(transport: Transport, rows: Row[]): PresetName {
  if (transport === "plain") return "custom";
  const presets = presetsFor(transport);
  for (const name of Object.keys(presets) as Exclude<PresetName, "custom">[]) {
    const def = presets[name];
    if (def.length !== rows.length) continue;
    const matches = def.every((d, i) => {
      const row = rows[i];
      if (row.address.trim() !== d.address || row.serverName.trim() !== d.serverName) return false;
      if (transport !== "https") return true;
      const rowPath = row.path.trim() === "" ? DEFAULT_DOH_PATH : row.path.trim();
      const wantPath = d.path && d.path !== "" ? d.path : DEFAULT_DOH_PATH;
      return rowPath === wantPath;
    });
    if (matches) return name;
  }
  return "custom";
}

export function UpstreamsField({
  value,
  onChange,
}: {
  value: string;
  onChange: (next: string) => void;
}) {
  const [transport, setTransport] = useState<Transport>(() => deriveState(value).transport);
  const [rows, setRows] = useState<Row[]>(() => deriveState(value).rows);
  const [note, setNote] = useState<string | null>(null);
  const [rowError, setRowError] = useState<RowError | null>(null);
  // An explicit override, not a replacement for activePreset's derivation:
  // clicking Custom means "I am taking manual control" — the rows are left
  // exactly as they are (nothing cleared, nothing rewritten), so they may
  // still match a preset exactly, and activePreset would happily relabel
  // the chip back to that preset. This pins it to Custom until the operator
  // either picks a named preset or changes transport, at which point the
  // pin is stale (it described a row set for a choice that just changed)
  // and is cleared so the derivation in `preset` below takes over again.
  const [presetOverride, setPresetOverride] = useState<"custom" | null>(null);
  // The string we last handed to onChange — how the resync effect below
  // tells "the value prop changed because we changed it" (nothing to do)
  // apart from "it changed out from under us" (a form reset/discard, or a
  // save that moved the baseline), which does need a resync.
  const lastEmitted = useRef(value);

  useEffect(() => {
    if (value === lastEmitted.current) return;
    lastEmitted.current = value;
    const derived = deriveState(value);
    setTransport(derived.transport);
    setRows(derived.rows);
    setNote(null);
    setRowError(null);
    setPresetOverride(null);
  }, [value]);

  /**
   * Builds the wire string from `nextRows` under `nextTransport`, and either
   * hands it to `onChange` or — if `parseUpstreams` would reject it — leaves
   * `onChange` uncalled and records which row said why.
   *
   * A row whose Address is still blank is dropped rather than validated: an
   * empty new row (from "+ Add resolver") is not yet an entry, the same way
   * parseUpstreams itself forgives a trailing comma.
   */
  function emit(nextTransport: Transport, nextRows: Row[]) {
    const scheme = schemeFor(nextTransport);
    const built = nextRows
      .map((row, index) =>
        row.address.trim() === ""
          ? null
          : {
              index,
              entry: buildUpstream(
                scheme,
                row.address.trim(),
                row.serverName.trim(),
                row.path.trim(),
              ),
            },
      )
      .filter((e): e is { index: number; entry: string } => e !== null);
    const joined = built.map((e) => e.entry).join(",");

    if (joined === "") {
      setRowError(null);
      lastEmitted.current = joined;
      onChange(joined);
      return;
    }

    const parsed = parseUpstreams(joined);
    if (parsed.ok) {
      setRowError(null);
      lastEmitted.current = joined;
      onChange(joined);
      return;
    }

    const failed = built.find((e) => e.entry === parsed.error.entry);
    setRowError({
      index: failed ? failed.index : nextRows.length - 1,
      message: ROW_ERROR_MESSAGES[parsed.error.code] ?? parsed.error.message,
      target: errorTarget(parsed.error.code),
    });
  }

  function updateRow(index: number, patch: Partial<Row>) {
    setNote(null);
    const nextRows = rows.map((r, i) => (i === index ? { ...r, ...patch } : r));
    setRows(nextRows);
    emit(transport, nextRows);
  }

  function addRow() {
    setNote(null);
    setRows([...rows, blankRow()]);
    // A blank row cannot change whether the list parses — nothing to emit.
  }

  function removeRow(index: number) {
    setNote(null);
    const nextRows = rows.filter((_, i) => i !== index);
    const finalRows = nextRows.length > 0 ? nextRows : [blankRow()];
    setRows(finalRows);
    emit(transport, finalRows);
  }

  function choosePreset(name: PresetName) {
    setNote(null);
    if (transport === "plain") return; // the preset row isn't shown here at all
    if (name === "custom") {
      setPresetOverride("custom");
      if (rows.length === 0) setRows([blankRow()]);
      return;
    }
    setPresetOverride(null);
    const nextRows = presetsFor(transport)[name].map((p) => ({
      id: rowID(),
      address: p.address,
      serverName: p.serverName,
      path: p.path ?? "",
    }));
    setRows(nextRows);
    emit(transport, nextRows);
  }

  /**
   * Converts the table when the transport changes, per the brief's resolved
   * ambiguity: never a silent discard.
   *
   * Encrypted -> Plain always succeeds (an address is still an address);
   * only the now-meaningless server names are dropped. Plain -> encrypted
   * can fail: a hostname has no address to carry into a `tls://`/`https://`
   * entry, so if *any* row is a hostname the whole table clears rather than
   * half-convert into something the operator didn't ask for.
   */
  function changeTransport(next: Transport) {
    if (next === transport) return;
    const wasEncrypted = transport !== "plain";
    const nowEncrypted = next !== "plain";
    let nextRows = rows;
    let nextNote: string | null = null;

    if (wasEncrypted && !nowEncrypted) {
      const hadNames = rows.some((r) => r.serverName.trim() !== "");
      nextRows = rows.map((r) => ({ id: r.id, address: r.address, serverName: "", path: "" }));
      if (hadNames) nextNote = "Server names dropped.";
    } else if (!wasEncrypted && nowEncrypted) {
      const nonBlank = rows.filter((r) => r.address.trim() !== "");
      const anyHostname = nonBlank.some((r) => !isAddressIP(r.address));
      if (nonBlank.length === 0 || anyHostname) {
        nextRows = [blankRow()];
        if (nonBlank.length > 0) nextNote = "Entries cleared — they had no address.";
      } else {
        nextRows = nonBlank.map((r) => ({
          id: r.id,
          address: r.address.trim(),
          serverName: "",
          path: "",
        }));
      }
    }

    setTransport(next);
    setRows(nextRows);
    setNote(nextNote);
    setPresetOverride(null);
    emit(next, nextRows);
  }

  const preset = presetOverride ?? activePreset(transport, rows);
  const columns =
    transport === "https"
      ? "grid-cols-[1fr_116px_1fr_30px]"
      : transport === "tls"
        ? "grid-cols-[1fr_1fr_30px]"
        : "grid-cols-[1fr_30px]";
  const headers =
    transport === "https"
      ? ["Address", "Path", "Server name"]
      : transport === "tls"
        ? ["Address", "Server name"]
        : ["Address"];

  return (
    <div className="flex flex-col gap-3">
      {/* 1. Transport selector — same control shape as upstream.strategy's
          SettingRadioList (settings.tsx), reimplemented here rather than
          shared because that one is typed to SelectField's option shape. */}
      <fieldset aria-label="Transport" className="flex flex-col border border-border-muted">
        {TRANSPORT_OPTIONS.map((option) => (
          <label
            key={option.value}
            aria-label={option.label}
            className="grid cursor-pointer grid-cols-[14px_1fr] items-start gap-2.5 border-b border-border-muted px-2.5 py-[7px] last:border-b-0 has-[:checked]:bg-accent"
          >
            <input
              type="radio"
              name="upstreams-transport"
              value={option.value}
              checked={transport === option.value}
              onChange={() => changeTransport(option.value)}
              className="peer sr-only"
            />
            <span
              aria-hidden
              className="mt-0.5 size-3 border border-border peer-checked:border-primary peer-checked:bg-primary peer-checked:shadow-[inset_0_0_0_2px_var(--background)]"
            />
            <span className="flex min-w-0 flex-col gap-0.5">
              <span className="flex items-baseline gap-2">
                <span className="font-mono text-[11.5px] peer-checked:font-semibold">
                  {option.label}
                </span>
                <span className="font-mono text-[10px] text-muted-foreground">{option.sample}</span>
              </span>
              <span className="text-[11px] text-pretty text-muted-foreground">{option.hint}</span>
            </span>
          </label>
        ))}
      </fieldset>

      {/* 2. Preset row — encrypted transports only. */}
      {transport !== "plain" && (
        <div className="flex items-center gap-2.5">
          <span className="font-mono text-[9.5px] tracking-[0.12em] text-muted-foreground uppercase">
            Preset
          </span>
          <div className="flex divide-x divide-border-muted overflow-hidden rounded-md border border-border-muted">
            {(["cloudflare", "quad9", "google", "custom"] as const).map((name) => (
              <button
                key={name}
                type="button"
                onClick={() => choosePreset(name)}
                aria-pressed={preset === name}
                className={cn(
                  "h-[26px] px-[11px] font-mono text-xs",
                  preset === name
                    ? "bg-primary font-semibold text-primary-foreground"
                    : "hover:bg-muted",
                )}
              >
                {PRESET_LABELS[name]}
              </button>
            ))}
          </div>
        </div>
      )}

      {/* 5. Switch notice — what converting the list just did, shown until
          the next edit. Sits above the table, not the transport selector,
          so it reads as "here's what's about to be saved" rather than
          "here's what you just clicked". */}
      {note && (
        <WarningStrip
          as="output"
          size="tight"
          className="block font-mono text-[11.5px] text-warning-foreground"
        >
          {note}
        </WarningStrip>
      )}

      {/* 3. Entry table. */}
      <div className="border border-border-muted">
        <div className={cn("grid items-center bg-muted px-2.5 py-1.5", columns)}>
          {headers.map((h) => (
            <span
              key={h}
              className="font-mono text-[9.5px] font-semibold tracking-[0.12em] text-muted-foreground uppercase"
            >
              {h}
            </span>
          ))}
          <span aria-hidden />
        </div>

        {rows.map((row, index) => (
          <div key={row.id} className="border-t border-border-muted">
            <div className={cn("grid items-center gap-2 px-2.5 py-1.5", columns)}>
              <Input
                aria-label="Address"
                aria-invalid={rowError?.index === index && rowError.target === "address"}
                value={row.address}
                onChange={(e) => updateRow(index, { address: e.target.value })}
                autoComplete="off"
                spellCheck={false}
                className="font-mono text-xs"
              />
              {transport === "https" && (
                <Input
                  aria-label="Path"
                  value={row.path}
                  onChange={(e) => updateRow(index, { path: e.target.value })}
                  placeholder={DEFAULT_DOH_PATH}
                  autoComplete="off"
                  spellCheck={false}
                  className="text-xs"
                />
              )}
              {transport !== "plain" && (
                <Input
                  aria-label="Server name"
                  aria-invalid={rowError?.index === index && rowError.target === "serverName"}
                  value={row.serverName}
                  onChange={(e) => updateRow(index, { serverName: e.target.value })}
                  autoComplete="off"
                  spellCheck={false}
                  className="text-xs"
                />
              )}
              <Button
                type="button"
                variant="ghost"
                size="icon-sm"
                title="Remove"
                aria-label="Remove"
                onClick={() => removeRow(index)}
              >
                <Minus />
              </Button>
            </div>
            {rowError?.index === index && (
              <p className="px-2.5 pb-2 text-[11.5px] text-destructive-foreground">
                {rowError.message}
              </p>
            )}
          </div>
        ))}

        <div className="border-t border-border-muted p-1.5">
          <Button type="button" variant="ghost" size="sm" onClick={addRow}>
            <Plus />
            Add resolver
          </Button>
        </div>
      </div>

      {/* 4. The transport-dependent hint. Not the outer FormDescription:
          that reads from the static per-field config in settings.tsx, and
          this has to track the transport that's on screen right now, which
          can lead the saved value while an edit is still being corrected. */}
      <p className="text-xs text-pretty text-muted-foreground">{hintFor(transport)}</p>
    </div>
  );
}
