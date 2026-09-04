/**
 * How long the pointer has to rest on a trigger before its tooltip opens.
 *
 * rnui's TooltipProvider defaults this to **0**, which is wrong for a table:
 * the zones list puts a trigger on every status cell, so a pointer crossing
 * the rows on its way somewhere else fires one popup per row it passes over.
 * A short delay makes the tooltip answer a question that was actually asked,
 * and Base UI's grouping means only the first one in a run waits it out — a
 * pointer moving between two triggers gets the second instantly.
 *
 * Lives here rather than inline because the app and the test harness both
 * mount the provider (app.tsx, test/render.tsx) and a tooltip test's timing
 * should be the app's, not a second number that drifts from it.
 */
export const TOOLTIP_DELAY_MS = 300;
