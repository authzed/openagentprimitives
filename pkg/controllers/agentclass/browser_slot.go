package agentclass

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/tools/browserschema"
)

// browserSchemaToolkits returns the builtin browser schema toolkit — carrying
// the one web_host resource — when this class references a BrowserToolbox, and
// nothing otherwise.
//
// It exists so web_host's SESSION-ONLY standing and its DISPLAY flow into the
// class's status through the exact resolvers a real toolkit's resources use
// (resolveResourceStandings, resolveResourceDisplays, resolvePermissionTitles,
// and resolveStandingFor, which resolveSlotValueKeying calls for a declared
// web_host slot). Those resolvers read only MCPServer / SpiceboxToolkit /
// SidecarToolbox fragments, and the browser toolbox is none of those, so a
// class declaring a web_host slot would otherwise hit "resource type
// \"web_host\" declares no standing" and be refused.
//
// The toolkit is SYNTHETIC — built in memory from browserschema.Fragment(),
// never a persisted CR — exactly as embeddedToolkitAsCR represents a built-in
// toolkit. It carries ONLY the schema fragment (no tools), so the keying and
// fact-source resolvers that also read keyingToolkits contribute nothing from
// it.
//
// GATED on the BrowserToolbox reference so web_host's standing and display never
// appear on a class that has no browser toolbox — mirroring how git_repo's
// standing/display only appear on a class that references the git toolkit. The
// cluster SCHEMA carries web_host unconditionally (the guardian controller's
// schemaBaseline); this is the per-class RESOLUTION half, which is reference-
// scoped. Both read the ONE declaration in browserschema, so they cannot drift.
func browserSchemaToolkits(ac *spiceboxv1alpha1.AgentClass) []spiceboxv1alpha1.SpiceboxToolkit {
	if ac == nil || len(ac.Spec.BrowserToolboxes) == 0 {
		return nil
	}
	return []spiceboxv1alpha1.SpiceboxToolkit{{
		ObjectMeta: metav1.ObjectMeta{Name: browserschema.SyntheticToolkitName},
		Spec:       spiceboxv1alpha1.SpiceboxToolkitSpec{SpiceDBSchema: browserschema.Fragment()},
	}}
}
