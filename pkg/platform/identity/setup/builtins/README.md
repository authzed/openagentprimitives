# `pkg/platform/identity/setup/builtins`

Hand-coded credential-acquisition flows for curated providers, plus the `Flow`
contract they implement, the registry they self-register into, and the live
token-verification helpers every flow (and the LLM agent) funnels through.

A `Flow` owns no I/O. It **describes** itself as a sequence of `tui.Screen`s;
`pkg/cli/tui`'s sequencer presents them and owns chrome, theme and driver
selection. Branching lives in ordinary Go between screens, which is what lets a
caveat that applies to one provider be shown only to the users who hit it.

## Subpackages

| Package                                     | Flow name                                                                                                                                                    |
| ------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| [`anthropic_oauth`](anthropic_oauth/)       | `anthropic-oauth`                                                                                                                                            |
| [`github_pat`](github_pat/)                 | `github-pat`                                                                                                                                                 |
| [`kubectl_kubeconfig`](kubectl_kubeconfig/) | `kubectl-kubeconfig`                                                                                                                                         |
| [`oauth_mcp`](oauth_mcp/)                   | `oauth-mcp`                                                                                                                                                  |
| [`tailscale_authkey`](tailscale_authkey/)   | `tailscale-authkey`                                                                                                                                          |
| [`flowscreens`](flowscreens/)               | Not a flow — the screens more than one flow takes ("open the page where this is generated"), plus the per-provider vocabulary a question is configured with. |
| [`loader`](loader/)                         | Not a flow — blank-imports every flow above so a binary registers all of them with one import. **New flows are added here.**                                 |

## Constraints

- **A flow's `Name()` must match the provider's `builtin:` field**, or the
  provider will never route to it.
- **`Screens` returning a nil error must return at least one screen.** A flow
  that asks nothing would go on to store whatever an unanswered `State` yielded;
  a flow with nothing to offer returns an error saying so.
- **`Result` is the fail-closed choke point.** It reads the answered `State` and
  persists through `req.Store` — the setup engine's single writer. It is called
  only after a successful screen run, on the same `Flow` value.
- **A step lands in `flowscreens` when a _second_ flow needs it.** Steps
  specific to one provider stay in that provider's package.
- `verify.go`'s `VerifyStatus` switches all carry a `default:` arm, so a newly
  added status cannot fall open at a consumer that has not been taught what it
  means. Note the split: 401 is `VerifyRejected` (the credential is bad); 403 is
  `VerifyForbidden` (the credential is fine, the request is not).
