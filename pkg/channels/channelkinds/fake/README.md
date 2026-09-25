# `fake` — the recording channel kind

A channel kind that records instead of transmitting. Used by unit tests, the e2e
harness, and any scenario that needs a real registered kind without a real
transport.

| File                                                          | Role                                                                                                                          |
| ------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------- |
| `kind.go`                                                     | The `channelkinds.Kind` implementation and the recorder                                                                       |
| `interaction.go`                                              | The `interaction` sub-channel sender — the generic Interaction model's recorder, plus the recorded types and public accessors |
| `sender_credential_request.go`, `sender_credential_linked.go` | Sub-channel senders for the credential flow                                                                                   |
| `channel_history.go`                                          | `ChannelHistoryReader`                                                                                                        |
| `session_owner.go`                                            | `SessionOwnerProvider`                                                                                                        |
| `webauth.go`                                                  | `WebAuthenticator`                                                                                                            |
| `wizard.go`                                                   | `oap channel create --kind fake`                                                                                              |

## Non-obvious constraints

- **One sub-channel name routes both interaction legs.** `interaction` carries
  the request leg (`KindInteractionRequest`) _and_ the applied leg
  (`KindInteractionApplied`), mirroring how the outbound relay dispatches. The
  sender, the recorded types and the accessors live together in one file for
  that reason — do not split them back into `kind.go`.
- **This kind deliberately implements only some optional interfaces.** That is a
  feature: it is what lets a test assert the graceful path a kind takes when a
  capability is absent. Adding an interface here to make one test simpler
  removes that coverage everywhere else.
