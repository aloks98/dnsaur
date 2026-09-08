/**
 * react-hook-form's field `name` is a dot-path into a nested object: its
 * internal get/set treat "." as a path separator, the way lodash's
 * `_.get`/`_.set` do. Every settings key here (`blocking.ttl`,
 * `serve.dot.enabled`) is the literal, flat key the API expects, not a
 * nested path — so using one directly as an RHF name silently produces both
 * a stray top-level `"blocking.ttl"` and a nested `{ blocking: { ttl } }`,
 * corrupting handleSubmit's values.
 *
 * Every RHF-facing name goes through this sanitizer. `field.key` remains the
 * one true API key, recovered from the field definition (never from the form
 * values) wherever a PUT payload is built — see settings.tsx's onSubmit.
 *
 * It lives in lib/ rather than beside either caller because it had two
 * copies: settings.tsx renders protocols-field.tsx, so protocols-field
 * importing back would have made the two modules circular, and each grew
 * its own. The Protocols tests build their harness from one copy and the
 * page uses the other, so a drift between them would have blanked every
 * control in that group with nothing failing.
 */
export function rhfName(key: string): string {
  return key.replaceAll(".", "__");
}
