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
2. Replace `OAP_AGENT_SIGNING_KEY` with `new.key`'s contents and this file
   with `new.pub` in the same PR.
3. After merge, the publish workflow re-signs every example under the new key.
   Bundles signed only by the old key stop verifying against this file.
