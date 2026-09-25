package capability

import (
	"strings"

	"github.com/authzed/openagentprimitives/pkg/agent/agentcaps"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/meta"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/web/uicomponents"
)

// pageSectionTitle heads the prompt section the update_view capability
// contributes. The builder bronze bundle asserts on it verbatim.
const pageSectionTitle = "Your page"

// pageSection renders the standing instructions for a page with hooks.
//
// Every per-hook line is meta.HookLine — the same text update_view's schema
// carries — and the rules are generic on purpose: what a specific hook is for
// is its author's intent, printed beside it, never transcribed here.
//
// Two of the rules name a tool, and each is included only when this session
// actually has that tool, so the agent is never told to call one it does not
// have. The read_view rule rides on the class activating read_view
// (readViewActive). The ask rule's "wait for the answer with
// await_user_message" half rides on channelAttached: await_user_message comes
// from channelInteractionCapability.Offer, which returns nothing at all when
// o.Binding is nil (channelinteraction.go), while the UI runtime is wired on
// the class referencing an AgentUI alone — so a kubectl-started session with
// no input channel has the page and no await. It can still ask on the page;
// what goes with the binding is the waiting half AND the "ask in the
// conversation instead" fallback, because with no channel there is no
// conversation to fall back to. The tell rule needs no tool and is always
// printed.
//
// The ask rule presumes nothing about the page's shape either: a hook set
// aside for questions is one page's choice, not a platform guarantee — most
// pages declare no such hook — so the rule names it as the preferred home
// when the page has one and otherwise sends the question to the hook whose
// own intent fits best.
func pageSection(hooks []uicomponents.Hook, readViewOffered, channelAttached bool) PromptSection {
	var b strings.Builder
	b.WriteString("The person may be looking at a page you control. It has these generative hooks — regions only you can fill or clear, with update_view — in page order:\n")
	for _, h := range hooks {
		b.WriteString("- " + meta.HookLine(h) + "\n")
	}
	b.WriteString("\nRules:\n")
	b.WriteString("- Respond by updating the page, not only in chat: the person is looking at the page, and a reply that leaves it unchanged looks like nothing happened.\n")
	if channelAttached {
		b.WriteString("- Ask on the page: put an ap:question in the hook the page sets aside for questions when it has one (its intent says so), otherwise in the hook whose intent best fits, then wait for the answer with await_user_message. One question at a time; clear it once answered. If the page cannot carry a question — a long free-text answer, or something not on the page — ask in the conversation instead, where the person is prompted to reply.\n")
	} else {
		b.WriteString("- Ask on the page: put an ap:question in the hook the page sets aside for questions when it has one (its intent says so), otherwise in the hook whose intent best fits. One question at a time; clear it once answered.\n")
	}
	b.WriteString("- Tell without asking: when you have something to tell the person and are waiting for an event rather than an answer, put an ap:notice in a hook, with buttons for what they can do next, and then wait. Use ap:question only when you need words back. Replace or clear the notice once the thing you were waiting for happens: a notice left on the page keeps the platform from offering the person a place to reply.\n")
	b.WriteString("- Each hook's intent is its standing instruction. Keep a hook that tracks progress current as you advance; clear a hook whose intent says to once its moment has passed (update_view with clear: true).\n")
	b.WriteString("- If the page has a timeline, each step's summary is a fact in a few words, never a sentence; keep every step's summary current whenever you repaint the timeline.\n")
	b.WriteString("- A hook admits only its allowed components; a fill using anything else is refused and nothing changes.\n")
	if readViewOffered {
		b.WriteString("- Call read_view before your first update_view in a session, and after anything you did not write may have changed the page. It returns the page as JSX.\n")
	}
	return PromptSection{Title: pageSectionTitle, Body: b.String()}
}

// readViewActive answers whether class also activates read_view, with the
// same agentcaps fold Assemble applies to the capability itself (opt-in:
// granted AND not disabled). A nil class or a malformed grant answers false —
// fail-closed, matching Assemble's treatment of the same grant.
func readViewActive(class *spiceboxv1alpha1.AgentClass) bool {
	g, err := agentcaps.GrantOf(class, "read_view")
	if err != nil {
		return false
	}
	return agentcaps.Active(false, g)
}
