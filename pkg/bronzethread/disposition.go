package bronzethread

import "github.com/authzed/openagentprimitives/pkg/agent/tool"

// Disposition decides whether a tool's recorded output is replayed or the tool
// runs for real.
type Disposition int

const (
	// ReplayOutput hands back the bundle's canned result. For tools whose
	// output is stateful or non-deterministic — the network, a sandbox, a
	// remote MCP server.
	ReplayOutput Disposition = iota

	// RunReal executes the tool. For tools that ARE the code under test.
	RunReal
)

// metaOverrides are the meta tools that read state rather than being the code
// under test, so their outputs are replayed like any other stateful source.
//
// This table is the one place a tool declares an exception. A new tool adds a
// row or accepts the Kind default; no consumer branches on a tool name.
//
// It DECIDES something: MetaToolCannable reads it to answer whether a bundle
// may can that tool's reply, so a row here is what makes canning reachable and
// its absence is what refuses it. Adding a row is therefore a claim that the
// tool's answer comes from state a replay cannot reproduce — not a note.
var metaOverrides = map[string]Disposition{
	"query_memory":    ReplayOutput,
	"search_memory":   ReplayOutput,
	"query_knowledge": ReplayOutput,
	"load_skill":      ReplayOutput,
}

// DispositionFor decides how a tool behaves during replay.
//
// Meta tools RUN FOR REAL by default, and that is the whole point: update_plan,
// select_phase, and the gate they drive are the system under test. Replaying
// their outputs would test nothing — the bundle would be asserting against its
// own input.
//
// Sandbox and MCP tools replay: they reach outside the process, so their output
// is exactly the non-determinism a bundle exists to pin.
func DispositionFor(t tool.Tool) Disposition {
	if t == nil {
		return ReplayOutput
	}
	if d, ok := metaOverrides[t.Name()]; ok {
		return d
	}
	switch t.Kind() {
	case tool.KindMeta:
		return RunReal
	default:
		return ReplayOutput
	}
}
