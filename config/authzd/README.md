# `config/authzd/` — the async authorization worker

`authzd` is the asynchronous authorization daemon. It does two jobs: per-turn
entity extraction (subscribing to user-message events on NATS and writing
`extracted_entity` / `extraction_state` memory entries for binding autofill),
and metaagent scope orchestration (the cold-start / mid-session scope lifecycle,
checking `agentsession#manage_scope` in SpiceDB before routing an approve/deny
decision). It refuses to start without `SPICEDB_ENDPOINT` rather than route
decisions it cannot authorize.

## Files

- `serviceaccount.yaml` — the `agentprimitives-authzd` ServiceAccount. No Role
  or RoleBinding: authzd reaches NATS, SpiceDB, and the operator's memory
  endpoint, not the Kubernetes API.
- `deployment.yaml` — the Deployment. Wires NATS creds, the operator memory URL
  plus its bearer token, the SpiceDB endpoint and token, the Anthropic API key,
  and the metaagent extractor/composer models.

Both land in `agentprimitives-system` (set by `kustomization.yaml`). Nothing
here is generated.

[← config/](../README.md)
