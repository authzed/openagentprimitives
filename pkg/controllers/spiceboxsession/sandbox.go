package spiceboxsession

import (
	"fmt"
	"strings"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/tools/sandboxkinds"
	sandboxregistry "github.com/authzed/openagentprimitives/pkg/tools/sandboxkinds/registry"
)

// applySandboxOverride folds a session-level sandbox override onto a class
// snapshot before it is frozen into status.resolvedClass. override is the
// AgentSession reconciler's tier-resolved decision, stamped onto
// spec.sandbox by BuildBundleSession; nil for a directly-created session,
// which keeps the class's own setting.
//
// The override replaces the class's backend wholesale rather than merging
// into it — cross-tier config merging already happened in the settings
// resolver, so merging again here would be a second, differently-shaped
// source of truth.
//
// class is taken and returned by value so the caller's snapshot is never
// mutated: it is shared with the SpiceboxSession object being reconciled.
func applySandboxOverride(class spiceboxv1alpha1.SpiceboxClassSpec, override *spiceboxv1alpha1.SandboxBackend) spiceboxv1alpha1.SpiceboxClassSpec {
	if override != nil {
		class.Sandbox = *override
	}
	return class
}

// validateResolvedSandbox re-runs the class-vs-backend validation chain against
// the kind that will actually run, given the already-overridden class snapshot,
// PLUS a third check the class controller has no way to run at all: the
// session's own mounts (sessionMounts, legacySkillBundles) against that same
// kind. A staged skill lands on SpiceboxSessionSpec.Mounts (always tarGz), not
// on the class's spec.mounts, so it is invisible to ValidateClassAgainstKind
// and must be checked here instead — see ValidateSessionAgainstKind's doc.
//
// The spiceboxclass controller runs the class half of this chain, but only
// ever against the kind the CLASS declared. applySandboxOverride can replace
// that kind wholesale with the AgentSession's tier-resolved decision, and the
// runtime lookup later in Reconcile proves only that the new kind is
// registered — not that it can run this class or this session's mounts. So
// the override is the one moment the final (kind, classSpec, sessionMounts)
// tuple exists, and this is where it gets checked.
//
// All three checks run, in order, with the same messages as the class
// controller for the first two: ValidateClassAgainstKind for what the class
// asks generically (Supports(Feature)), ValidateSessionAgainstKind for what
// the session's own mounts ask, then the backend's own ValidateClass for
// whatever it rejects for its own reasons.
func validateResolvedSandbox(class spiceboxv1alpha1.SpiceboxClassSpec, sessionMounts []spiceboxv1alpha1.SpiceboxMount, legacySkillBundles bool) error {
	kindName := class.Sandbox.ResolvedKind()
	// An unregistered kind is refused rather than downgraded to the built-in
	// backend, matching the class controller: silently running on a different
	// substrate than the one asked for is worse than refusing to run.
	sbKind, ok := sandboxregistry.Get(kindName)
	if !ok {
		return fmt.Errorf("sandbox kind %q is not a registered sandbox backend (known: %s)",
			kindName, strings.Join(sandboxregistry.Keys(), ", "))
	}
	if err := sandboxkinds.ValidateClassAgainstKind(sbKind, class); err != nil {
		return err
	}
	if err := sandboxkinds.ValidateSessionAgainstKind(sbKind, sessionMounts, legacySkillBundles); err != nil {
		return err
	}
	if err := sbKind.ValidateClass(class); err != nil {
		return fmt.Errorf("sandbox kind %q rejected the class: %w", sbKind.Name(), err)
	}
	return nil
}
