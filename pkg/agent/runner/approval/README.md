# `pkg/agent/runner/approval`

The approval-prompt helpers the runner needs but that must not run as part of
the primary agent. The directory itself holds no Go code — the approval
publish/await machinery lives in
[`../host_approval.go`](../host_approval.go); what lands here is the isolated
LLM behind the prompt's wording.

## Subpackages

- [`summarizer`](./summarizer/) — the "What this tool call will do" LLM.

## See also

- [`pkg/agent/runner`](../) — the turn loop.
