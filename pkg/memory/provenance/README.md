# `provenance` — signing and verifying append-only entries

Per-publisher Ed25519 signatures over a canonical entry digest, hash-chained per
`(scope, publisher)` so gaps, reordering, fabrication, modification and
truncation are all detectable.

| File             | Role                                                                                 |
| ---------------- | ------------------------------------------------------------------------------------ |
| `digest.go`      | The canonical entry digest — what actually gets signed                               |
| `signer.go`      | Signs an entry: publisher, keyID, monotonic seq, prevHash, signature                 |
| `signingmem.go`  | A `memory.Memory` decorator that signs append-only writes on the way through         |
| `verifier.go`    | Offline chain verification — what `oap audit verify` walks                           |
| `writeverify.go` | **Verify-on-write**: the facade's rejection of unsigned or forged append-only writes |
| `chainheads.go`  | The tail anchors recorded at session completion                                      |

## Key custody

| Publisher                                   | Key source                                                                                                | Trust root                                                                                                                      |
| ------------------------------------------- | --------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------- |
| A session                                   | The AgentSession reconciler mints a per-session keypair into the per-session Secret (`audit-signing-key`) | `AgentSession.status.auditPublicKey` / `auditKeyID` — witnessed by Kubernetes                                                   |
| A component (channelsd / authzd / operator) | Minted at startup                                                                                         | Registered via `POST /memory/_publisher_key` into the `publisher-keys` ConfigMap — see [`../publisherkeys/`](../publisherkeys/) |

Chain heads land on `AgentSession.status.auditChainHeads` at completion; that
tail anchor is what makes **truncation** detectable, not just modification.

## Non-obvious constraints

- **Verification is on the write path, not only on audit.** The operator's
  facade rejects unsigned or forged append-only writes from token callers _and_
  from in-process writers. This is not an after-the-fact check.
- **The digest is canonical, not byte-literal.** A postgres round trip re-emits
  content from a JSONB column and `created_at` from a `TIMESTAMPTZ` one, so the
  stored bytes are never identical to what was written. Comparisons and
  signatures both work on the canonical form.
- **`oap audit verify <session>` exits non-zero on hard findings** and runs
  entirely offline against the recorded public keys.
