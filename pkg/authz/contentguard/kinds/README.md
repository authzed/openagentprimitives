# `pkg/authz/contentguard/kinds`

The built-in content-guard inspectors. Each self-registers into
[`../registry`](../registry/) from its `init()`; binaries opt in with a blank
import. Nothing here is referenced by name from a consumer — settings name an
inspector by its registry ID.

| Kind                                  | What it inspects                                                                                                                                                                                                                                                                                                                                          |
| ------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| [`promptinjection`](promptinjection/) | Classifies tool I/O for prompt-injection risk by POSTing text to a co-located ONNX detector sidecar and mapping the risk score to a `Finding`. The endpoint comes from `CONTENTGUARD_DETECTOR_ENDPOINT`, a full URL the operator sets when it injects the sidecar; the inspector implements `DetectorProvider` so the operator knows which image that is. |
| [`urlallowlist`](urlallowlist/)       | Enforces a URL policy on tool I/O: an ordered, first-match-wins rule list (same shape as `ToolGuardPolicy`), each rule matching by domain glob / regex / CEL and carrying its own `allow` \| `deny` \| `approve` action. Per-URL actions fold to one wholesale result by **strictest wins**: `deny` > `approve` > `allow`.                                |

Both are subject to the framework's 32 KiB `MaxInspectBytes` cap, applied by the
`Capped` decorator rather than by the inspector — see
[`../README.md`](../README.md).
