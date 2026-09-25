package capability

import (
	"context"

	"github.com/go-logr/logr"

	"github.com/authzed/openagentprimitives/pkg/agent/agentcaps"
	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// AssembleDeps is the full input to Assemble.
type AssembleDeps struct {
	Class        *spiceboxv1alpha1.AgentClass
	Session      *spiceboxv1alpha1.AgentSession
	Binding      *spiceboxv1alpha1.ChannelBinding // nil when not channel-attached
	Env          RunnerEnv
	NonMetaTools []tool.Tool // sandbox/mcp/sidecar; introspection must see these too
	Logger       logr.Logger
}

// Assembly is the complete output of one capability assembly: the merged tool
// list and every prompt section the active capabilities contributed, in the
// same registry order as their tools.
type Assembly struct {
	Tools    []tool.Tool
	Sections []PromptSection
}

// Assemble returns AssembleAll's tools alone — the shape a caller with no
// prompt to compose wants (oap session capture).
func Assemble(ctx context.Context, deps AssembleDeps) []tool.Tool {
	return AssembleAll(ctx, deps).Tools
}

// AssembleAll builds the complete merged tool list from the capability
// registry, and collects the prompt sections offered beside those tools.
// Order: active capabilities' tools in registry order (infra core first), then
// NonMetaTools, then introspection last (over the union). Skips are logged.
//
// Name-based dedup, because two capabilities can legitimately offer the same
// tool: artifacts and attachments both call modalityMetaTools, so a class
// granting both would otherwise offer fetch_artifact twice — which providers
// either reject or silently shadow. First occurrence in assembly order wins and
// later duplicates are dropped without a log line: two capabilities agreeing on
// a tool is not a failure. Sections are keyed to the capability's decision
// (offerOne's "contributed at least one tool"), not to dedup.
func AssembleAll(ctx context.Context, deps AssembleDeps) Assembly {
	out := Assembly{Tools: make([]tool.Tool, 0, 16)}
	seen := make(map[string]struct{}, 16)
	var introspection Capability

	appendUnique := func(tools []tool.Tool) {
		for _, t := range tools {
			if _, dup := seen[t.Name()]; dup {
				continue
			}
			seen[t.Name()] = struct{}{}
			out.Tools = append(out.Tools, t)
		}
	}

	for _, c := range Ordered() {
		if c.Name() == "introspection" {
			introspection = c // defer to the very end so it sees NonMetaTools
			continue
		}
		if tools, sections, ok := offerOne(ctx, c, deps); ok {
			appendUnique(tools)
			out.Sections = append(out.Sections, sections...)
		}
	}

	appendUnique(deps.NonMetaTools)

	if introspection != nil {
		env := deps.Env
		env.AllToolsSoFar = out.Tools // introspection resolves over the full set
		idDeps := deps
		idDeps.Env = env
		if tools, sections, ok := offerOne(ctx, introspection, idDeps); ok {
			appendUnique(tools)
			out.Sections = append(out.Sections, sections...)
		}
	}
	return out
}

// offerOne resolves grant + config for one capability, decides whether it is
// active, and invokes its offer — OfferWithSections when the capability is a
// SectionOfferer, Offer otherwise. It returns the tools and sections to append
// and a bool that is true only when they should actually be appended. The bool
// is false for an inactive capability (not an error, not logged) and for a
// capability that contributed no tool.
//
// A SkipReason is logged and the tools that came back with it are still
// appended. The two are independent: a capability that withholds ONE of the
// several tools it contributes returns the rest plus the reason for the one it
// held back, and dropping those would punish the session for a narrowing that
// was deliberate. A capability that contributes nothing at all returns
// (nil, skip) and lands on the same log line, exactly as before.
//
// A section that arrives with NO tool is dropped, and the drop is logged: text
// telling the agent how to use tools it does not have is worse than no text,
// and a capability that does this has a bug worth a log line.
//
// Infrastructural capabilities (core, introspection) are "always-on, cannot
// be disabled" per the Capability interface contract, so they bypass the
// granted/DefaultOn gate in agentcaps.Active entirely — they always run their
// offer regardless of what (or whether) the class's capabilities map says.
func offerOne(ctx context.Context, c Capability, deps AssembleDeps) ([]tool.Tool, []PromptSection, bool) {
	octx := offerContextFor(ctx, c, deps)
	if !c.Infrastructural() && !agentcaps.Active(c.DefaultOn(), agentcaps.Grant{Granted: octx.Granted, Enabled: octx.Enabled}) {
		return nil, nil, false
	}
	var (
		tools    []tool.Tool
		sections []PromptSection
		skip     *SkipReason
	)
	if so, ok := c.(SectionOfferer); ok {
		tools, sections, skip = so.OfferWithSections(octx)
	} else {
		tools, skip = c.Offer(octx)
	}
	if skip != nil {
		logSkip(deps.Logger, deps.Session, skip)
	}
	if len(tools) == 0 {
		if len(sections) > 0 {
			deps.Logger.Info("capability offered prompt text with no tool; text dropped",
				"capability", c.Name(), "sections", len(sections))
		}
		return nil, nil, false
	}
	return tools, sections, true
}

func offerContextFor(ctx context.Context, c Capability, deps AssembleDeps) OfferContext {
	grant, grantErr := agentcaps.GrantOf(deps.Class, c.Name())
	if grantErr != nil {
		// The common {enabled} blob is malformed JSON. agentcaps.GrantOf already
		// fail-closed (Enabled=false), but for a capability whose ParseConfig is
		// a no-op the ParseConfig branch below never fires, so this is the only
		// place the error surfaces — log it or it is silently dropped (AGENTS.md:
		// never silently drop errors). Capability stays inactive.
		deps.Logger.Info("capability config parse failed at assembly; treating inactive",
			"capability", c.Name(), "err", grantErr.Error())
	}
	enabled := grant.Enabled
	var cfg Config
	if cfgParsed, err := c.ParseConfig(grant.Raw); err == nil {
		cfg = cfgParsed
	} else {
		// Controller already validated; at runtime treat a bad config as
		// inactive rather than panicking. Never silent.
		deps.Logger.Info("capability config parse failed at assembly; treating inactive",
			"capability", c.Name(), "err", err.Error())
		enabled = false
	}
	return OfferContext{
		Ctx: ctx, Granted: grant.Granted, Enabled: enabled, Config: cfg,
		Class: deps.Class, Session: deps.Session, Binding: deps.Binding,
		OutBinding: outBindingFor(deps), Env: deps.Env,
	}
}

// outBindingFor resolves which binding will RENDER this session's outbound:
// spec.outputChannel when the session has one, else the input binding it was
// given. Same precedence as resolve.ForSession, and for the same reason — it is
// the transport that shows a reply that decides what a reply can be.
//
// Nil in, nil out: a session with no input binding is not channel-attached and
// has no outbound surface to describe.
func outBindingFor(deps AssembleDeps) *spiceboxv1alpha1.ChannelBinding {
	if deps.Binding == nil {
		return nil
	}
	if deps.Session != nil && deps.Session.Spec.OutputChannel != nil {
		return deps.Session.Spec.OutputChannel
	}
	return deps.Binding
}

func logSkip(l logr.Logger, sess *spiceboxv1alpha1.AgentSession, s *SkipReason) {
	name := ""
	if sess != nil {
		name = sess.Namespace + "/" + sess.Name
	}
	l.Info("capability granted but unavailable; tool not injected",
		"session", name, "capability", s.Capability, "reason", s.Reason)
}
