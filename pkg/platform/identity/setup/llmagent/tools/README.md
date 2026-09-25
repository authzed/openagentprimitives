# `pkg/platform/identity/setup/llmagent/tools`

The LLM-tool implementations for the setup agent. Each tool is a trio of
package-level identifiers — `<Name>Name`, `<Name>Schema`, and a `<Name>Run`
function — that [`../agent.go`](../) adapts to `tool.Tool`. Package-level seams
(openers, HTTP clients, prompters) exist for test injection.

## Tools

| File | Tool | What it does |
| ---- | ---- | ------------ |
| `fetch_url.go` | `fetch_url` | Read-only http/https GET, for provider docs. |
| `web_search.go` | `web_search` | Search via `pkg/tools/websearch`, query capped at 200 chars. |
| `open_browser.go` | `open_browser` | Opens a URL on the user's machine. |
| `prompt_user.go` | `prompt_user` | Asks the human a question — `text`, `secret`, or `choice`. |
| `run_shell.go` | `run_shell` | Runs a command that clears the dual-lock gate: it matches the provider's allowlist **and** the user confirms it. |
| `local_callback.go` | `local_callback` | Stands up the loopback listener for an OAuth redirect and performs the code→token exchange. |
| `store_credential.go` | `store_credential` | The terminal action: hands the credential to the setup engine's store seam. Shapes are `bearer`, `oauth`, `kubeconfig`. |

## Constraints

- **Every model-supplied URL goes through `pkg/x/safehttp`.** `fetch_url` and the
  OAuth token exchange in `local_callback` both take their target from model
  output, so the guarded dialer (private/loopback/link-local refused, redirects
  re-validated per hop) is the only correct client. Tests override the package
  seam; production must not.
- **`run_shell` is deny-by-default and dual-locked.** A provider with no
  allowlist gets an error, not a free shell; a matching command still needs user
  confirmation unless it is in the per-session always-approved set. Execution is
  capped by a 30-second hard timeout layered over the caller's context.
- **Provider-supplied `run_shell` regexes must be anchored at both ends.**
  `CompileAllowlist` compiles them as written and `regexp.MatchString` only
  requires a match *somewhere*, so an unanchored `^echo ` lets
  `echo legit; rm -rf /` through. Nothing enforces the anchors — it is a
  review property of the provider catalog.
- **Schemas are prompt.** The `description` text in each `<Name>Schema` is what
  the model reads; edit it with the same care as the system prompt.
- Tools return errors as strings the model can act on, but must never return
  credential bytes in an error.
