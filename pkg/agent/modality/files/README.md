# `pkg/agent/modality/files`

The `files` modality: moving artifact bytes between the artifact store and a
model that advertises native file input/output.

| File                                       | Holds                                                                                                             |
| ------------------------------------------ | ----------------------------------------------------------------------------------------------------------------- |
| [`files.go`](./files.go)                   | The `Modality` impl and its `init()` registration; decides which of the two tools to offer from the turn's `Env`. |
| [`fetch_artifact.go`](./fetch_artifact.go) | `fetch_artifact` — reads a byte range of a stored artifact into the turn.                                         |
| [`mount_artifact.go`](./mount_artifact.go) | `mount_artifact` — hands an artifact to the provider's own file store via a `modality.Bridge`.                    |
| [`reader.go`](./reader.go)                 | `StoreReader`, the `modality.ArtifactReader` over `pkg/platform/artifactstore`.                                   |

## Subpackages

- [`anthropicbridge`](./anthropicbridge/) — `modality.Bridge` against
  Anthropic's Files API. The transport logic sits behind a local `FilesClient`
  interface so it unit-tests with a fake and no network; `SDKFilesClient` is the
  thin production adapter.

## See also

- [`pkg/agent/modality`](../) — the contract and the registry.
