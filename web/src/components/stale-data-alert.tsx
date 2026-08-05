import type { ReactNode } from "react";
import { TriangleAlert } from "lucide-react";
import { Alert, AlertDescription, AlertTitle, Button } from "@e412/rnui-react";

/**
 * A *background* refetch failed while data from an earlier successful fetch
 * is still in hand.
 *
 * query-core flips `status` to "error" on that failure even though `data` is
 * intact, so gating a destructive "couldn't load" Alert on `isError` alone
 * would replace a populated, still-correct section with an error card the
 * moment one refetch blips — a mutation-triggered invalidation, a
 * refetchOnReconnect, or (on the dashboard) one missed beat of the 30s stats
 * poll. The destructive Alert is reserved for `isError && data === undefined`
 * — genuinely nothing to show — and this quiet banner covers the rest,
 * sitting above content that's still worth reading.
 *
 * Every page that reads a query into visible content uses this pair, so it
 * lives here rather than being redefined per page (it was copy-pasted six
 * times before, drifting in wording).
 */
export function StaleDataAlert({
  what,
  onRetry,
  isRetrying,
  // Overridable because settings adds a reassurance the other callers don't
  // need: its form keeps unsaved edits across the failed refetch.
  description = "Showing what last loaded successfully.",
}: {
  /** Names the content in the title: "Couldn't refresh {what}". */
  what: string;
  onRetry: () => void;
  isRetrying: boolean;
  description?: ReactNode;
}) {
  return (
    <Alert variant="warning">
      <TriangleAlert />
      <AlertTitle>Couldn&apos;t refresh {what}</AlertTitle>
      <AlertDescription>
        <p>{description}</p>
        <Button type="button" variant="outline" size="sm" onClick={onRetry} disabled={isRetrying}>
          {isRetrying ? "Retrying…" : "Try again"}
        </Button>
      </AlertDescription>
    </Alert>
  );
}
