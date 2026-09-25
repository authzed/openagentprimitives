package bronzethread

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"sync"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
)

// Canning a META tool's reply: the LAST resort, made reachable and never
// silent.
//
// standin.go states the three answers to a value a replay cannot derive, in
// order: reproduce it by RUNNING our own code, SEED a stand-in and let our code
// compose the reply from it, or CAN the reply outright. This file is the third,
// and it is an admission rather than a primitive — a canned reply is served
// whatever the code does, so a regression in the very code the bundle exists to
// cover is handed the recorded answer and passes.
//
// Three properties keep that admission from becoming a hole, and each is
// structural rather than a rule somebody has to remember:
//
//   - Only a tool whose OWN table says its output is replayed may be canned.
//     DispositionFor's metaOverrides has said since it was written that
//     query_memory, search_memory, query_knowledge and load_skill "read state
//     rather than being the code under test"; MetaToolCannable is that table's
//     first reader. update_plan, select_phase and the gate they drive are the
//     system under test and cannot be canned at all.
//
//   - A canned reply is REFUSED without a matching Assert.ToolsCalled entry.
//     See validateMetaToolReplies. That rule was written down in
//     Assertions.ToolsCalled long before anything could break it, and this is
//     the change that gives it a subject; making it a load-time refusal is what
//     stops the next canning from arriving without it.
//
//   - An ERROR result is never canned, in either direction. A gate refusal was
//     authored by the gate, and canning it would let a gate that STOPPED
//     refusing be served that text back and satisfy the assertion meant to
//     catch it — the same ruling ToolErrors takes for an MCP call. A tool's OWN
//     refusal is composed by our code from our own inputs and reproduces at
//     replay, so canning it would mask a regression in that composition. Both
//     say the same thing, which is why this type carries no error field at all
//     rather than a field the capture declines to set.

// MetaToolReply is what a replay hands the model for one META tool call instead
// of running the tool.
//
// One field, and the absence of the others is the design. There is no error
// half for the reason above; there is no per-call sequence because a meta tool
// that did not answer the same way every time is refused at capture rather than
// half-expressed (see steelthread's CodeMetaReplyNotUniform); and there is no
// argument matching, because a canned reply that varied by argument would be a
// simulation of the tool rather than an admission that it is not running.
type MetaToolReply struct {
	// Content is the tool_result body the model is handed, byte for byte, on
	// every call of the tool.
	//
	// Served as UNTRUSTED, which is what every cannable tool's success path
	// returns: the bytes are stored entries or graph facts relayed to the
	// model, not framework-controlled prose, so the runner wraps them in the
	// untrusted-output envelope exactly as it wrapped the recorded ones.
	Content string `json:"content"`
}

// MetaToolCannable reports whether a meta tool's reply may be canned at all.
//
// It reads metaOverrides — the table that already declares which meta tools
// "read state rather than being the code under test" — so the set of cannable
// tools and the set whose outputs are replayed cannot drift into disagreement.
// A tool that gains a row there becomes cannable in the same edit; one that
// loses it stops being cannable, and any bundle canning it is refused at load.
func MetaToolCannable(name string) bool {
	d, ok := metaOverrides[name]
	return ok && d == ReplayOutput
}

// CannableMetaTools names every meta tool a bundle may can, sorted — the same
// table MetaToolCannable reads, rendered for an error message.
func CannableMetaTools() []string {
	var out []string
	for name, d := range metaOverrides {
		if d == ReplayOutput {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}

// validateMetaToolReplies refuses every canned reply the replay could not serve
// honestly.
//
// A load precondition rather than a runtime check, for the reason
// validateStandIn is one: each failure below is SILENT at replay. A reply for a
// tool that runs for real is ignored; one with no ToolsCalled entry replays
// green while proving nothing about the call it stands for.
func (b Bundle) validateMetaToolReplies() error {
	if len(b.MetaToolReplies) == 0 {
		return nil
	}
	called := make(map[string]bool, len(b.Assert.ToolsCalled))
	for _, n := range b.Assert.ToolsCalled {
		called[n] = true
	}
	// Sorted so a bundle with two bad entries always reports the same one
	// first; Go's map order would otherwise make the message depend on the run.
	for _, name := range slices.Sorted(maps.Keys(b.MetaToolReplies)) {
		if !MetaToolCannable(name) {
			return fmt.Errorf("metaToolReplies[%q] cans a tool that is not cannable: only a meta tool whose "+
				"disposition is ReplayOutput may be canned (%v), because every other meta tool IS the code "+
				"under test and serving it a recorded answer would let a regression in it pass",
				name, CannableMetaTools())
		}
		if b.MetaToolReplies[name].Content == "" {
			return fmt.Errorf("metaToolReplies[%q] has no content; an empty reply is indistinguishable from "+
				"the tool answering with nothing, so the step it stands for would assert against a body "+
				"neither side chose", name)
		}
		if !called[name] {
			return fmt.Errorf("metaToolReplies[%q] cans a reply and assert.toolsCalled does not name it. A "+
				"canned reply is served whether or not the call was made the way the recording made it, so "+
				"without a separate statement that the call happened the bundle proves nothing about it — "+
				"add %q to assert.toolsCalled", name, name)
		}
	}
	return nil
}

// MetaToolCanner serves a bundle's canned meta replies and reports which ones
// it never got to serve.
//
// Guarded because the runner assembles tools on its own goroutine while the
// test goroutine constructed it, the same reason ToolCatalogCheck is.
type MetaToolCanner struct {
	mu      sync.Mutex
	replies map[string]MetaToolReply
	applied map[string]bool
}

// NewMetaToolCanner builds the canner for one replay, or nil when the bundle
// cans nothing.
func NewMetaToolCanner(replies map[string]MetaToolReply) *MetaToolCanner {
	if len(replies) == 0 {
		return nil
	}
	return &MetaToolCanner{replies: maps.Clone(replies), applied: map[string]bool{}}
}

// Replacer returns the func to wire into the harness's assembly seam, or nil
// when the bundle cans nothing.
//
// nil is the compatibility path AND the honest one, exactly as
// ToolCatalogCheck.Filter's is: a bundle that cans nothing leaves every
// assembled tool as assembled.
func (c *MetaToolCanner) Replacer() func(tool.Tool) (tool.Tool, error) {
	if c == nil {
		return nil
	}
	return c.replace
}

// replace swaps one assembled tool for its canned stand-in, or hands it back
// untouched.
func (c *MetaToolCanner) replace(t tool.Tool) (tool.Tool, error) {
	if t == nil {
		return nil, nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	reply, ok := c.replies[t.Name()]
	if !ok {
		return t, nil
	}
	if t.Kind() != tool.KindMeta {
		// The bundle's own validation already refused a name outside
		// metaOverrides, so reaching here means a name in that table is now
		// carried by a tool of another kind. Serving it anyway would can a
		// transport's reply through a path built for meta tools, and would do
		// it silently.
		return nil, fmt.Errorf("metaToolReplies cans %q, which this run assembled as a %s tool rather than a "+
			"meta tool; a canned meta reply must not stand in for a transport", t.Name(), t.Kind())
	}
	if iface, dropped := droppedOptionalInterface(t); dropped {
		// Fail closed rather than wrap. Every optional interface a Tool may
		// implement is consulted by TYPE ASSERTION on the concrete value, and a
		// wrapper cannot forward one it does not name — embedding tool.Tool
		// promotes only tool.Tool's own methods. Wrapping such a tool would
		// change what the gate, the interrupt path or introspect_tool sees
		// while looking like it changed only the reply.
		return nil, fmt.Errorf("metaToolReplies cans %q, which implements the optional interface %s; canning "+
			"it would drop that interface, changing behaviour the reply was never meant to touch. Reproduce "+
			"or seed this tool's state instead of canning it", t.Name(), iface)
	}
	c.applied[t.Name()] = true
	return cannedMetaTool{Tool: t, reply: reply}, nil
}

// Unapplied names the canned tools this replay never assembled, sorted.
//
// It exists because a canned reply that was never installed is INVISIBLE: the
// real tool runs, answers from an empty fixture, and the step fails somewhere
// downstream naming the tool rather than the wiring. A driver asks after the
// run, the same reason ToolCatalogCheck.Consulted is asked.
func (c *MetaToolCanner) Unapplied() []string {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for name := range c.replies {
		if !c.applied[name] {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}

// cannedMetaTool is one meta tool with its Execute replaced and everything else
// — name, kind, description, input schema, declared permission — left as the
// capability assembled it.
//
// Only Execute, and that is what keeps the gate intact: the dispatcher runs
// authz, the plan gate and the approval flow against Permission() and
// PermissionVariants() BEFORE it calls Execute (pkg/agent/runner/
// loop_dispatch.go), so a call this reply would answer is refused at exactly
// the same point the real tool's would be, and a refused call never reaches
// the canned bytes at all.
type cannedMetaTool struct {
	tool.Tool
	reply MetaToolReply
}

func (c cannedMetaTool) Execute(context.Context, json.RawMessage, *tool.SessionContext) (tool.Result, error) {
	// Untrusted, matching every cannable tool's success path: the bytes are
	// stored entries or graph facts relayed to the model, so the runner's
	// untrusted-output envelope wraps them exactly as it wrapped the recorded
	// ones. A Trusted result would reach the model as different bytes than the
	// recording did, on a path whose whole purpose is to reproduce them.
	return tool.Result{Content: c.reply.Content}, nil
}

// droppedOptionalInterface names an optional Tool interface the wrapper would
// silently drop, if the tool implements one.
//
// The list is every interface this repo type-asserts a Tool against. It is
// checked rather than forwarded because forwarding requires naming each one in
// the wrapper too, and a wrapper that forwards four of five is worse than one
// that refuses: the missing one is invisible until behaviour changes.
func droppedOptionalInterface(t tool.Tool) (string, bool) {
	switch t.(type) {
	case tool.Introspectable:
		return "tool.Introspectable", true
	case tool.OriginTool:
		return "tool.OriginTool", true
	case tool.PerCallPermission:
		return "tool.PerCallPermission", true
	case tool.NamedCallArgs:
		return "tool.NamedCallArgs", true
	case tool.Cancellable:
		return "tool.Cancellable", true
	}
	return "", false
}
