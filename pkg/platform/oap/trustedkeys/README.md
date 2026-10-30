# Trusted agent-signing keys

`oap-agents.pub` is the ECDSA P-256 public key (PKIX PEM) that verifies the
example agents CI publishes to `ghcr.io/authzed/openagentprimitives/agents/`
(`.github/workflows/publish-agents.yaml`). The private half lives only in the
`OAP_AGENT_SIGNING_KEY` repository secret.

Check a published agent by hand:

```bash
oap agent verify ghcr.io/authzed/openagentprimitives/agents/codebot:latest \
  --key pkg/platform/oap/trustedkeys/oap-agents.pub
```

## Rotating

1. Generate a new keypair outside the repo:
   `openssl ecparam -name prime256v1 -genkey -noout | openssl pkcs8 -topk8 -nocrypt -out new.key`
   and `openssl ec -in new.key -pubout -out new.pub`.
2. Open a PR replacing this file with `new.pub`, and merge it. The repository
   secret is not part of the PR: it changes the moment it is saved, so the two
   halves can't land together. Until step 3, publishes sign with the old key and
   fail their verify against the new file. That is the safe direction: the
   publish script moves `:latest` only after a bundle verifies, so `:latest`
   keeps its last good bundles. Only that run's `:<short-sha>` tags get the
   bundles that failed.
3. Replace the `OAP_AGENT_SIGNING_KEY` secret with `new.key`'s contents, then
   re-run the failed "Publish example agents" run for the tip of `main` (or
   merge anything to `main`). It re-signs every example under the new key.
4. Delete `new.key` from wherever it was generated.

Bundles signed only by the old key stop verifying against this file once step 2
merges. Their old signatures stay attached in the registry, though, and still
verify against the old public key for anyone who kept a copy.

If the rotation is because the old private key leaked, do step 3 before step 2
so the leaked key stops being used to sign as soon as possible, and accept that
publishes fail until step 2 merges. Then delete every signature the old key made
from the registry (the `application/vnd.dev.cosign.artifact.sig.v1+json`
referrers on each `ghcr.io/authzed/openagentprimitives/agents/*` digest), since
anyone holding the leaked key can produce more that look just as valid.
