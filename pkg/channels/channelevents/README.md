# `channelevents` — the NATS wire format

Every message between the runner, channelsd and the channel kinds travels as a
JSON `Envelope` on a NATS subject. This package owns that envelope, the closed
enum of sub-channel `Kind`s (40 today), and each kind's payload struct.

| File                   | Role                                                                                                                                                |
| ---------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------- |
| `envelope.go`          | `Envelope`, `SessionRef`, `Validate`, `BuildEnvelope`. Version 1 only                                                                               |
| `kinds.go`             | The closed `Kind` enum and its publishing discipline, kind by kind                                                                                  |
| `subject_authority.go` | `AuthorizeInSubject` / `AuthorizeOutSubject` and the subject parsers                                                                                |
| `interaction.go`       | The semantic Interaction model — the request/applied/decision triple every prompt type rides                                                        |
| everything else        | One file per payload family: `plan_update.go`, `monitoring.go`, `notice.go`, `metaagent.go`, `credential_request.go`, `tool_approval_details.go`, … |

## The subject is the routing authority, not `Envelope.Session`

`Envelope.Session` is **advisory**, publisher-controlled JSON. The NATS subject
is the only session identity a publisher's per-session JWT authorizes (its
`PubAllow` covers `ap.session.<ns>.<own-name>.>`, both `.in.` and `.out.`).

Any consumer taking an envelope **off the bus** must cross-check the two —
`AuthorizeInSubject` / `AuthorizeOutSubject` over the subject parsers — and drop
an envelope whose `Session` disagrees. Routing off the body instead lets a
publisher permitted on one session's subject act on another's. This applies to
concrete per-session subscriptions too, not just wildcards: the subject pins
where a message came from, the field is whatever its publisher wrote.

The field stays on the wire because envelopes also travel _without_ a subject —
`BuildEnvelope` hands them straight to a `channelkinds.Sender`.

## Adding a kind

The enum is closed on purpose: publish-side validation rejects an unknown kind,
and the taxonomy is a security-boundary document. A new kind is a code change in
`kinds.go` plus its payload type, not a string a caller invents.

For a new _prompt_, prefer a new
[`channelinteractions`](../channelinteractions/) category over a new envelope
kind — the Interaction triple already carries every prompt type, and channel
kinds render the semantic payload without per-category logic.
