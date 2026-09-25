// pkg/agent/runner/definitionreport.go
//
// Reporting a broken agent definition to the people who can fix it.
//
// A gate that cannot be evaluated fails closed, which is correct and which also
// makes it invisible: every layer behaves exactly as it would for a legitimate
// refusal, so nothing distinguishes "you may not do this" from "this agent is
// misconfigured and nobody may do this" unless something goes looking. The
// pipeline carries that distinction (pipeline.Decision.Definition); this file
// acts on it, putting the fault on the monitoring channel with the locus and the
// authored expression an operator needs to find the rule.
package runner

import (
	"errors"
	"log/slog"
	"strconv"
	"time"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
)

// MonitoringCategoryDefinition groups agent-definition faults on the
// monitoring channel — a rule that cannot be evaluated, a spec that cannot be
// applied. It sits alongside "reconcile" rather than inside it because the
// object involved reconciles perfectly well: the CRD is valid, its conditions
// are healthy, and the defect only appears when a real call runs the rule
// against a real argument.
const MonitoringCategoryDefinition = "definition"

// reportDefinitionError publishes one monitoring event when a tool-call gate
// denied because the agent's own definition could not be evaluated.
//
// Deduplicated per distinct fault for the life of the runner. A broken CEL rule
// fires on every single call that touches its tool — a dashboard with six bound
// sections produces six identical faults before the viewer has done anything —
// and an operator channel that repeats one defect indefinitely gets muted. The
// fault is permanent until someone edits the spec, so repeating it gains
// nothing.
//
// The dedup is in-memory by design: losing it on restart costs exactly one
// repeated notification about a fault that is still true, and means a genuine
// re-break after a failed fix is reported again on the next runner.
func (l *Loop) reportDefinitionError(sess *tool.SessionContext, toolName string, out pipeline.Outcome) {
	if out.Definition == nil {
		return
	}
	ns, name := sessionRefOf(sess)

	// Logged unconditionally, published best-effort. The log is the record
	// that survives a missing NATS handle, a dropped publish, or a cluster
	// with no monitoring Channel configured at all — none of which should be
	// able to make a broken definition silent.
	slog.Default().Info("tool call denied by an unevaluatable agent definition",
		"session", ns+"/"+name, "tool", toolName, "hook", out.FiredHook,
		"definition", out.Definition.Error())

	if l.seenDefinitionError(out.Definition.Error()) {
		return
	}
	if l.EchoPublish == nil {
		// Not an error: kubectl-driven sessions and tests have no bus. The log
		// above already carries the fault; say why the channel will not.
		slog.Default().Info("agent definition fault not published to monitoring: no NATS publish handle",
			"session", ns+"/"+name, "tool", toolName)
		return
	}

	ev := channelevents.MonitoringEvent{
		Level:      channelevents.MonitoringLevelError,
		Category:   MonitoringCategoryDefinition,
		Transition: channelevents.MonitoringTransitionFailed,
		Source:     l.definitionSourceRef(ns, name, toolName),
		Condition:  "ToolDefinition",
		Reason:     "UnevaluatableRule",
		Summary:    definitionSummary(toolName, out.Definition),
		Hint:       definitionHint(out.Definition),
		Timestamp:  time.Now(),
	}
	if err := channelevents.PublishMonitoring(l.EchoPublish, ev); err != nil {
		slog.Default().Info("publishing an agent definition fault to monitoring failed",
			"session", ns+"/"+name, "tool", toolName, "err", err.Error())
	}
}

// definitionSourceRef points the monitoring event at the MCPServer whose spec
// carries the broken rule, when the tool's origin is known — that CR is the
// file an operator has to edit. Origin lookup is best-effort (a tool with no
// MCP origin, a Loop without the lookup wired), and the session is the honest
// fallback: it is where the fault was observed, and it is always resolvable.
func (l *Loop) definitionSourceRef(ns, sessionName, toolName string) channelevents.MonitoringSourceRef {
	if origin := l.lookupOrigin()(toolName); origin != "" {
		return channelevents.MonitoringSourceRef{Kind: "MCPServer", Namespace: ns, Name: origin}
	}
	return channelevents.MonitoringSourceRef{Kind: "AgentSession", Namespace: ns, Name: sessionName}
}

// definitionSummary is the one line an operator reads first. It names the tool
// and the locus, and says plainly that the tool is unusable — a rule that
// cannot be evaluated is not a rule that is sometimes strict.
func definitionSummary(toolName string, err error) string {
	var de *pipeline.DefinitionError
	if errors.As(err, &de) && de.Locus != "" {
		return "the rule at " + de.Locus + " on tool " + strconv.Quote(toolName) +
			" cannot be evaluated, so every call to this tool is refused"
	}
	return "tool " + strconv.Quote(toolName) +
		" has a definition that cannot be evaluated, so every call to it is refused"
}

// definitionHint carries the parts that let an operator act: the authored
// expression to search for, and the underlying failure. Both are configuration
// and framework text — never caller data — which is why they can be quoted
// here in full while the browser-facing copy stays generic.
func definitionHint(err error) string {
	var de *pipeline.DefinitionError
	if !errors.As(err, &de) {
		return err.Error()
	}
	hint := ""
	if de.Detail != "" {
		hint = "expression: " + de.Detail + " — "
	}
	if de.Err != nil {
		return hint + de.Err.Error()
	}
	return hint + de.Error()
}

// seenDefinitionError records key and reports whether it had already been
// seen, so each distinct fault is published once. Keyed on the full error text
// rather than the tool name: one tool can hold several broken rules, and an
// operator who fixes the first should hear about the second.
func (l *Loop) seenDefinitionError(key string) bool {
	l.definitionErrMu.Lock()
	defer l.definitionErrMu.Unlock()
	if l.definitionErrSeen == nil {
		l.definitionErrSeen = map[string]bool{}
	}
	if l.definitionErrSeen[key] {
		return true
	}
	l.definitionErrSeen[key] = true
	return false
}

// sessionRefOf reads the namespace/name off a SessionContext, tolerating nil —
// this runs on a deny path, and a reporting helper must never be the thing
// that panics a fail-closed branch.
func sessionRefOf(sess *tool.SessionContext) (string, string) {
	if sess == nil {
		return "", ""
	}
	return sess.Namespace, sess.Name
}
