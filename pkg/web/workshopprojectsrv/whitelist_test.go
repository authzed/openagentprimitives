package workshopprojectsrv

import (
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// projectedFields is the explicit "yes" list: every AgentClassSpec field
// projectStandin copies from the source class onto the stand-in. Kept in
// this _test.go file (not the production file) so a reviewer sees the
// accounting right next to the guard that enforces it, but the two MUST
// name the same fields as projectStandin's own doc comment — that doc
// comment is the other half of this contract, read by a human; this map is
// read by the compiler-adjacent reflection check below.
var projectedFields = map[string]bool{
	"DisplayName":  true,
	"Description":  true,
	"SystemPrompt": true,
	"Skills":       true,
	// UserPreferences is credential-free by construction (the field's own
	// doc forbids declaring a secret as a preference) and values live
	// per-user in the memory plane, not on the class itself, so the schema
	// carries nothing sensitive. Rehearsal wants behavior parity — the
	// preferences capability offers tools and a prompt section that change
	// agent behavior — and a stand-in's distinct class name keys different
	// preference entry IDs than the source's, so a rehearsal save can never
	// collide with the real class's own values.
	"UserPreferences": true,
}

// notProjectedFields is the explicit "no" list: every AgentClassSpec field
// this route deliberately leaves at zero value on the stand-in, because
// projecting it would leak the source's credentials, tools, or authz
// policy, or because it is simply irrelevant to a rehearsal stand-in
// (Model/Harness/ToolSessionLog fall back to defaults; Subagents/
// SubagentModes/BoundEntities are the roster, dropped rather than
// rewritten — see the package doc's Ruling B).
var notProjectedFields = map[string]bool{
	"Model":                  true,
	"Harness":                true,
	"AgentIdentity":          true,
	"IdentityMode":           true,
	"CredentialLinkTimeout":  true,
	"IdentityRecommendation": true,
	"IdentityChoiceTimeout":  true,
	"ToolBundles":            true,
	"MCPServers":             true,
	"CredentialExplanations": true,
	"SidecarToolboxes":       true,
	"WorkspaceSource":        true,
	"ToolSessionLog":         true,
	"Budget":                 true,
	"BoundEntities":          true,
	"Channels":               true,
	"Authz":                  true,
	"Subagents":              true,
	"SubagentModes":          true,
	"ToolGuard":              true,
	"Capabilities":           true,
	"CompletionRequirements": true,
	"AgentUI":                true,
	// Config/ConfigSchema: the config plane exists to feed the CEL
	// constraints of bound toolspecs, which a stand-in never carries;
	// dropped together so the reconciler's config-against-schema check
	// sees neither keys nor schema.
	"Config":       true,
	"ConfigSchema": true,
}

// assertFieldSetDecided is the future-field guard's shared reflection
// check, extended to cover nested types (not just AgentClassSpec's own
// top-level fields): for typ's own exported field set, every field name
// must appear in EXACTLY ONE of projected/notProjected, so a new field
// fails BY NAME the day it appears, rather than silently riding along on
// whatever a bare struct-value copy happens to do with it.
//
// This is the difference between a whitelist that stays a whitelist and one
// that rots into a blacklist the moment the type grows a field: without
// this check, the copying code's field-by-field construction (see
// projectStandin's own doc comment) is the ONLY thing enforcing the
// boundary, and nothing stops a future edit from "simplifying" it into a
// bare struct copy followed by a few field clears — a blacklist wearing a
// whitelist's clothes, per this package's own doc.
func assertFieldSetDecided(t *testing.T, typeName string, typ reflect.Type, projected, notProjected map[string]bool) {
	t.Helper()
	for i := 0; i < typ.NumField(); i++ {
		name := typ.Field(i).Name
		inProjected := projected[name]
		inDenied := notProjected[name]
		assert.False(t, inProjected && inDenied,
			"%s.%s is listed in BOTH projected and notProjected — pick exactly one", typeName, name)
		assert.True(t, inProjected || inDenied,
			"%s grew a field (%q) this projection has not explicitly decided about: "+
				"add it to EXACTLY ONE of the projected/notProjected maps in whitelist_test.go, "+
				"after deciding whether a credential-free rehearsal stand-in may safely carry it, "+
				"and update projectStandin's own copy logic and doc comment to match", typeName, name)
	}
}

// TestAgentClassSpec_EveryFieldIsExplicitlyDecided is the top-level
// future-field guard: see assertFieldSetDecided's own doc for what it
// enforces and why.
func TestAgentClassSpec_EveryFieldIsExplicitlyDecided(t *testing.T) {
	assertFieldSetDecided(t, "AgentClassSpec", reflect.TypeOf(spiceboxv1alpha1.AgentClassSpec{}), projectedFields, notProjectedFields)
}

// promptSourceProjectedFields/promptSourceNotProjectedFields are
// PromptSource's OWN whitelist. The top-level guard only reflects
// over AgentClassSpec's own fields — but SystemPrompt (a PromptSource) is
// copied onto the stand-in BY VALUE (projectedFields["SystemPrompt"],
// above), so a future field on PromptSource itself would ride along
// silently on that bare struct-value copy unless pinned here too.
// PromptSource has already grown once (ConfigMapRef, after Inline).
var promptSourceProjectedFields = map[string]bool{
	"Inline": true,
	// ConfigMapRef rides along structurally with the rest of the struct (it
	// is not itself stripped out by projectStandin) — but
	// pkg/web/workshopprojectsrv's serveHTTP refuses the WHOLE request
	// outright whenever a reachable source's ConfigMapRef is set, before
	// projectStandin is ever called for it. So a
	// stand-in never actually carries a live ConfigMapRef in practice; it is
	// marked "projected" here because the FIELD rides along structurally —
	// the business rule that neuters it lives at the call site, not in this
	// whitelist.
	"ConfigMapRef": true,
}

var promptSourceNotProjectedFields = map[string]bool{}

func TestPromptSource_EveryFieldIsExplicitlyDecided(t *testing.T) {
	assertFieldSetDecided(t, "PromptSource", reflect.TypeOf(spiceboxv1alpha1.PromptSource{}), promptSourceProjectedFields, promptSourceNotProjectedFields)
}

// agentSkillProjectedFields/agentSkillNotProjectedFields are AgentSkill's
// OWN whitelist. Skills is copied onto the stand-in BY VALUE,
// element by element (projectedFields["Skills"], above) — a future field on
// AgentSkill (e.g. an authority-scoping field) would ride along silently on
// that bare struct-value copy unless pinned here too.
var agentSkillProjectedFields = map[string]bool{
	"Name":   true,
	"Ref":    true,
	"Target": true,
}

var agentSkillNotProjectedFields = map[string]bool{}

func TestAgentSkill_EveryFieldIsExplicitlyDecided(t *testing.T) {
	assertFieldSetDecided(t, "AgentSkill", reflect.TypeOf(spiceboxv1alpha1.AgentSkill{}), agentSkillProjectedFields, agentSkillNotProjectedFields)
}
