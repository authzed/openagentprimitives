package capability

import (
	"encoding/json"
	"log/slog"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/meta"
	"github.com/authzed/openagentprimitives/pkg/web/uicomponents"
	"github.com/authzed/openagentprimitives/pkg/web/uiview"
)

func init() {
	Register(&updateViewCapability{})
	Register(&readViewCapability{})
	Register(&setViewParamsCapability{})
}

var _ SectionOfferer = updateViewCapability{}

// updateViewCapability and readViewCapability are TWO registrations, not one
// with a sub-config: reading what a human is being shown and REWRITING it are
// different privileges, and a UI that only wants the agent to discuss its
// dashboard should not have to grant mutation. A single capability with
// {"write": true} would make the dangerous half an easily-missed flag.
//
// Both are opt-in and neither is infrastructural: absent from
// spec.capabilities, the tool is never constructed and never reaches the
// model's catalog.
type updateViewCapability struct{}
type readViewCapability struct{}

func (updateViewCapability) Name() string                                { return "update_view" }
func (updateViewCapability) DefaultOn() bool                             { return false }
func (updateViewCapability) Infrastructural() bool                       { return false }
func (updateViewCapability) ParseConfig(json.RawMessage) (Config, error) { return nil, nil }

// Offer is the plain-Capability view of OfferWithSections: the same tools,
// the same skip, the section dropped. Assemble never calls it for this
// capability (it dispatches SectionOfferer), but a caller holding only a
// Capability must see exactly what Assemble would inject.
func (c updateViewCapability) Offer(o OfferContext) ([]tool.Tool, *SkipReason) {
	tools, _, skip := c.OfferWithSections(o)
	return tools, skip
}

// OfferWithSections never offers an inert tool, so it skips when no AgentUI
// is wired for this session, or when the AgentUI's page declares no
// generative hook. The second case matters on its own: a `hook` enum with no
// members can never succeed, and the loud skip is what tells a bundle author
// their page has nothing agent-writable, instead of leaving them to find out
// when the first update_view fails.
//
// The AgentUI is read ONCE here, purely to build the hook enum in the tool's
// schema — never as the authorization. Runtime.Write/Clear re-read the CR
// live on every call and re-derive the page's hooks there, because a bundle
// redeploy can add, remove, or narrow one mid-session.
//
// It is also where a page's own authoring gap is surfaced: a hook with no
// Intent gives the agent no instruction for that region, and CompileSlots
// (the Spec.Slots shim) never sets one — so every slots-authored page hits
// this. Logged once per session, at assembly, rather than per turn or per
// call, because Offer itself runs exactly once per session.
//
// The "Your page" prompt section is emitted HERE, from the same gate as the
// tool: no AgentUI, or a page with no hook, yields neither. It lists every
// hook's line (meta.HookLine — the same text the schema carries) and the
// standing rules; see pageSection.
func (updateViewCapability) OfferWithSections(o OfferContext) ([]tool.Tool, []PromptSection, *SkipReason) {
	if o.Env.UIView == nil {
		return nil, nil, &SkipReason{Capability: "update_view", Reason: "this agent class references no agent UI"}
	}
	ui, err := o.Env.UIView.Base(o.Ctx)
	if err != nil {
		return nil, nil, &SkipReason{Capability: "update_view", Reason: "the agent UI could not be read"}
	}
	decl, err := uiview.DeclarationFromSpec(ui)
	if err != nil {
		return nil, nil, &SkipReason{Capability: "update_view", Reason: "the agent UI could not be read"}
	}
	hooks := uicomponents.Hooks(decl)
	if len(hooks) == 0 {
		return nil, nil, &SkipReason{Capability: "update_view", Reason: "this agent UI declares no generative hook"}
	}
	sessNS, sessName := "", ""
	if o.Session != nil {
		sessNS, sessName = o.Session.Namespace, o.Session.Name
	}
	for _, h := range hooks {
		if h.Intent == "" {
			// Once per session — Offer runs at assembly — so an author who
			// left a region unexplained hears it exactly once, with the
			// page and hook named.
			slog.Info("agentui: hook without intent; the agent gets no instruction for this region",
				"namespace", sessNS, "session", sessName, "ui", ui.Name, "hook", h.Name)
		}
	}
	return []tool.Tool{meta.NewUpdateView(meta.UpdateViewConfig{View: o.Env.UIView, Hooks: hooks})},
		[]PromptSection{pageSection(hooks, readViewActive(o.Class), o.Binding != nil)}, nil
}

func (readViewCapability) Name() string                                { return "read_view" }
func (readViewCapability) DefaultOn() bool                             { return false }
func (readViewCapability) Infrastructural() bool                       { return false }
func (readViewCapability) ParseConfig(json.RawMessage) (Config, error) { return nil, nil }

// Offer skips when RunnerEnv.UIView is nil — the same "no agent UI wired"
// case updateViewCapability declines for. Unlike update_view, read_view
// needs no per-hook enumeration: it always returns the whole merged
// declaration, so there is no "zero hooks" case that makes it unable to
// succeed.
func (readViewCapability) Offer(o OfferContext) ([]tool.Tool, *SkipReason) {
	if o.Env.UIView == nil {
		return nil, &SkipReason{Capability: "read_view", Reason: "this agent class references no agent UI"}
	}
	return []tool.Tool{meta.NewReadView(meta.ReadViewConfig{View: o.Env.UIView})}, nil
}

// setViewParamsCapability is a THIRD registration alongside read and write,
// for the same reason those two are separate: it is a distinct privilege.
//
// It is not a weaker update_view. update_view changes what a hook CONTAINS;
// this changes what the page's own controls are SET TO, which leaves the
// author's declaration untouched and instead moves the values every binding
// resolves with. An agent that should discuss a dashboard but never touch it
// gets read_view alone; one that should compose prose into a hook gets
// update_view; one that should be able to open the page on what a person
// actually asked for gets this. Folding it into update_view would hand every
// hook-composing agent the ability to move the filters under a reader.
type setViewParamsCapability struct{}

func (setViewParamsCapability) Name() string                                { return "set_view_params" }
func (setViewParamsCapability) DefaultOn() bool                             { return false }
func (setViewParamsCapability) Infrastructural() bool                       { return false }
func (setViewParamsCapability) ParseConfig(json.RawMessage) (Config, error) { return nil, nil }

// Offer skips when no AgentUI is wired, and — like update_view's
// zero-hooks case — when the declaration drives no parameters at all.
// A view with no controls is one where every call to this tool can only fail,
// and the loud skip tells a bundle author their page has no parameter to set
// rather than leaving them to find out from the agent's first rejected call.
//
// The declaration is read ONCE here to publish the key list in the tool's
// description. It is never the authorization: Runtime.WriteParams re-reads the
// AgentUI live and re-derives the legal keys, because a bundle redeploy can
// remove a control mid-session.
func (setViewParamsCapability) Offer(o OfferContext) ([]tool.Tool, *SkipReason) {
	if o.Env.UIView == nil {
		return nil, &SkipReason{Capability: "set_view_params", Reason: "this agent class references no agent UI"}
	}
	ui, err := o.Env.UIView.Base(o.Ctx)
	if err != nil {
		return nil, &SkipReason{Capability: "set_view_params", Reason: "the agent UI could not be read"}
	}
	decl, err := uiview.DeclarationFromSpec(ui)
	if err != nil {
		return nil, &SkipReason{Capability: "set_view_params", Reason: "the agent UI could not be read"}
	}
	keys := uicomponents.ParamKeys(decl)
	if len(keys) == 0 {
		return nil, &SkipReason{Capability: "set_view_params", Reason: "this agent UI declares no controls to set"}
	}
	return []tool.Tool{meta.NewSetViewParams(meta.SetViewParamsConfig{View: o.Env.UIView, ParamKeys: keys})}, nil
}
