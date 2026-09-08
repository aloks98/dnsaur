import { TriangleAlert } from "lucide-react";
import { useResolverStatus } from "../hooks/use-settings";
import { expiringSoonDetail, servingState } from "../lib/serving";
import { WarningStrip } from "./warning-strip";

/**
 * The Protocols group's own facts, followed from the shell rather than
 * pinned to Settings — the same reasoning as EncryptionDowngradeBanner
 * (which this sits beside in app-shell.tsx): an operator who never opens
 * Settings still needs to learn that a protocol they turned on never
 * actually started, or that the certificate keeping every encrypted
 * answer trusted is about to lapse.
 *
 * A *list*, not a single banner — see the Protocols artboard's own
 * `sc-for` loop over `shellBanners`. DoT and DoH fail independently (the
 * whole reason ProtocolStatus reports them separately rather than as one
 * bool — see its Go doc comment), and the certificate can be expiring at
 * the same time either of those is broken; all three conditions are
 * independent and, when several are true, all shown together.
 */
export function ServingBanners() {
  const status = useResolverStatus();
  const serving = status.data?.serving;
  const certificate = status.data?.certificate;

  const banners: string[] = [];

  if (serving && servingState(serving.dot) === "failed") {
    banners.push(
      `DNS-over-TLS is enabled but not listening${serving.dot.error ? ` — ${serving.dot.error}` : ""}`,
    );
  }
  if (serving && servingState(serving.doh) === "failed") {
    banners.push(
      `DNS-over-HTTPS is enabled but not listening${serving.doh.error ? ` — ${serving.doh.error}` : ""}`,
    );
  }
  if (certificate?.expiring_soon) {
    // The server reports a certificate that has already lapsed as
    // ok=true, expiring_soon=true (internal/app/serve.go's CertExpiry), so
    // without the past tense this banner said "expires in 0 days" about
    // one that expired last week.
    const { days, date, expired } = expiringSoonDetail(certificate);
    banners.push(
      expired
        ? `TLS certificate expired — ${date}`
        : `TLS certificate expires in ${days} day${days === 1 ? "" : "s"} — ${date}`,
    );
  }

  return (
    <>
      {banners.map((text) => (
        <WarningStrip
          key={text}
          as="div"
          role="alert"
          size="roomy"
          className="flex shrink-0 items-center gap-2 border-b border-border"
        >
          <TriangleAlert className="size-4 shrink-0 text-warning-foreground" aria-hidden />
          <span className="font-mono text-[12.5px] text-warning-foreground">{text}</span>
        </WarningStrip>
      ))}
    </>
  );
}
