package toolcall

import (
	"fmt"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	tcwebhook "github.com/authzed/openagentprimitives/pkg/controllers/webhooks/toolcall"
)

// validateSessionBinding refuses a ToolCall whose spec.session names a bundle
// belonging to some OTHER AgentSession than the one that owns the call.
//
// Both fields are written by the creator, and until this check nothing
// required them to agree. That mattered because everything privileged
// downstream keys on spec.session: executorFor picks the sandbox pod to exec
// into from it, and checkTokenUse derives the AgentSession whose use_token
// grant it consults from the bundle's parent label. Naming a sibling's bundle
// therefore ran the call in the sibling's sandbox, with the sibling's
// credentials injected, authorized by the sibling's own grant.
//
// This is defense in depth rather than the primary gate — the admission
// webhook binds the CREATOR to the owning session, which is the stronger
// check because it consults the authenticated principal. This one runs
// unconditionally in the reconcile path, so a webhook that is down,
// unregistered, or excluded by a namespaceSelector does not become a bypass.
//
// Fail-closed on both unknowns: a bundle with no parent label, and a ToolCall
// with no AgentSession owner, are refused rather than treated as matching.
// Reading "unknown" as "fine" is precisely how the original defect worked.
func validateSessionBinding(tc *spiceboxv1alpha1.ToolCall, bundle *spiceboxv1alpha1.SpiceboxSession) error {
	owner := tcwebhook.AgentSessionOwnerName(tc)
	if owner == "" {
		return fmt.Errorf("ToolCall %s/%s carries no AgentSession ownerReference, so the session it acts for cannot be established",
			tc.Namespace, tc.Name)
	}
	parent := bundle.Labels[agentSessionLabel]
	if parent == "" {
		return fmt.Errorf("bundle SpiceboxSession %s/%s carries no %s label, so it cannot be shown to belong to AgentSession %s",
			bundle.Namespace, bundle.Name, agentSessionLabel, owner)
	}
	if parent != owner {
		return fmt.Errorf("ToolCall %s/%s is owned by AgentSession %s but names bundle %s, which belongs to AgentSession %s: a session may only run tools in its own sandbox",
			tc.Namespace, tc.Name, owner, bundle.Name, parent)
	}
	return nil
}
