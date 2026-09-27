# `categories` — the production interaction rows

The declarative rows registered into the process-wide
[`channelinteractions`](../) registry. Blank-importing this package from a
binary that renders or decides interactions is all the wiring there is.

| File                        | Role                                                                         |
| --------------------------- | ---------------------------------------------------------------------------- |
| `categories.go`             | The 10 **prompt** categories and their name constants                        |
| `notices.go`                | The 24 **notice** categories (zero-action, one-way) and their name constants |
| `register.go`               | `init` → `RegisterAll` → `registerPrompts` + `registerNotices`               |
| `outcome_label.go`          | The user-facing label for a settled interaction                              |
| `identity_choice_labels.go` | Per-option copy for the `identity_choice` prompt                             |

## Prompt categories

`credential_link`, `credential_update`, `identity_choice`, `portal_access`,
`permission_request`, `provider_error_retry`, `queued_messages`,
`content_inspection`, `tool_approval`, `info_leakage`.

## Non-obvious constraints

- **The name constants here are the single source of truth** shared by
  publishers, renderers and `Bind` sites. Never spell a category name as a
  literal at a call site.
- **`RegisterAll` is exported for tests only.** `init` has already called it;
  registration panics on a duplicate, so calling it from production code is a
  crash. It exists because an `init` cannot be re-invoked after a test calls
  `channelinteractions.Reset` — without it, one `Reset` leaves every later test
  looking at an empty registry.
- **`credential_link` and `credential_update` are two rows, not one.** They are
  raised by different producers for different reasons and their cards carry
  different content; they share only the `AwaitingCredentials` park. The tones
  differ accordingly — `credential_update` is degraded (something the user
  already connected was verified dead), `credential_link` is routine.
- **Grant handlers are bound in `internal/cmd/channelsd`, not here.**
  `tool_approval` side-effects a grant write on approve; `info_leakage` binds
  the pure decision handler because its grant is written runner-side via the
  `interaction_applied` bridge.
