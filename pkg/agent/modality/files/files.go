package files

import (
	"github.com/authzed/openagentprimitives/pkg/agent/llm"
	"github.com/authzed/openagentprimitives/pkg/agent/modality"
	"github.com/authzed/openagentprimitives/pkg/agent/modality/registry"
	"github.com/authzed/openagentprimitives/pkg/agent/tool"
)

func init() { registry.Register(New()) }

type filesModality struct{}

// New constructs the files modality.
func New() modality.Modality { return filesModality{} }

func (filesModality) Name() string { return "files" }

// MetaTools contributes the read-by-reference tool (fetch_artifact) whenever a
// reader is available, plus the files-in tool (mount_artifact) when native file
// handling is opted in and the resolved model advertises CapNativeFileIn.
func (filesModality) MetaTools(env modality.Env) []tool.Tool {
	var out []tool.Tool
	if env.Reader != nil {
		out = append(out, NewFetchArtifact(env.Reader))
	}
	if env.NativeActive(llm.CapNativeFileIn) && env.Bridge != nil {
		out = append(out, NewMountArtifact(env.Bridge))
	}
	return out
}

// Instructions is the modality-owned prompt guidance. The read-by-reference
// text is always present; the files-out and files-in paragraphs are gated
// independently, each matching its own MetaTools gate. Files-out needs no
// Bridge — the runner inlines the downloaded bytes — while files-in requires
// both an active CapNativeFileIn and a wired Bridge.
func (filesModality) Instructions(env modality.Env) string {
	out := "## Working with large files\n" +
		"Large outputs and inputs are referenced by handle, not pasted inline. " +
		"Use `fetch_artifact(handle, start, length)` to read portions on demand; " +
		"do not expect full contents to appear in the conversation."
	if env.NativeActive(llm.CapNativeFileOut) {
		out += "\n\nYou also have a code-execution environment. To create a large or data-derived " +
			"artifact, generate it as a file there and call " +
			"`artifact_prepare(source: \"container_file\", payload: \"<file_id>\")` instead of pasting its " +
			"contents."
	}
	if env.NativeActive(llm.CapNativeFileIn) && env.Bridge != nil {
		out += "\n\nTo work over a large input, call `mount_artifact(<handle>)` to preload it into " +
			"your environment and read it there."
	}
	return out
}
