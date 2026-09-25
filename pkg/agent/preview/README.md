# `pkg/agent/preview`

Preview generation for artifacts the agent produces. The directory itself holds
no Go code.

## Subpackages

- [`markup`](./markup/) — the isolated secondary LLM that generates sample HTML
  body markup so a `css` artifact has something to render against in the browser
  preview.

## Constraint

Like the approval summarizer, `markup` is a **second, isolated LLM call**: the
input CSS is treated as user data, the output is structural HTML only (no
scripts), and the result is re-sanitized through the `html` renderer before
display. A compromised primary agent cannot reach this call.

## See also

- [`pkg/agent`](../) — group overview.
