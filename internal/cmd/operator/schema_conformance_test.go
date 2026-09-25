package main

import (
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	guardianschema "github.com/authzed/openagentprimitives/pkg/authz/guardian/schema"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	channelregistry "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/registry"
	"github.com/authzed/openagentprimitives/toolkits"
)

// toolkitDefinitionNameRe matches a `definition <name>` declaration inside a
// toolkit fragment's RawZed text — the same lightweight lexical scan
// pkg/authz/guardian/schema/validate_fragment.go's (unexported)
// definitionNameRe runs, reimplemented here because this file cannot reach
// that identifier from package main.
var toolkitDefinitionNameRe = regexp.MustCompile(`(?m)^\s*definition\s+([A-Za-z_][A-Za-z0-9_]*)\b`)

// toolkitDefinitionNames DERIVES every definition name the given toolkit
// fragments declare — from structured Resources[] and from a RawZed's
// `definition <name>` lines alike — rather than transcribing a hand-written
// list. A hand-written list is the thing that rots the moment a toolkit adds,
// renames, or removes a definition; deriving it from the actual fragments
// means the assertion this feeds can never drift out of sync with what
// toolkits.All() + ToolkitFragments really produce.
func toolkitDefinitionNames(frags []*spiceboxv1alpha1.SpiceDBSchemaFragment) []string {
	seen := map[string]struct{}{}
	var names []string
	add := func(n string) {
		if n == "" {
			return
		}
		if _, ok := seen[n]; ok {
			return
		}
		seen[n] = struct{}{}
		names = append(names, n)
	}
	for _, f := range frags {
		if f == nil {
			continue
		}
		for _, r := range f.Resources {
			add(r.Name)
		}
		for _, m := range toolkitDefinitionNameRe.FindAllStringSubmatch(f.RawZed, -1) {
			add(m[1])
		}
	}
	return names
}

// The compile-time set composes. A failure here is a BUILD bug: these
// fragments ship inside the binary, there is no CR to mark, and the operator
// would discover it as a cluster-wide schema freeze at startup instead.
//
// This lives here, not in pkg/controllers/guardian, because THIS is the
// binary whose compile-time fragment set actually matters — main.go already
// blank-imports every one of the seven channel kinds (agent, bento, browser,
// fake, github, local, slack; see the imports above), so this file adds no
// blank imports of its own. A duplicate copy that DID blank-import them
// living in an untagged pkg/controllers/guardian test file would leak those
// kinds into that package's whole test binary — including under
// -tags=integration, where several tests assert registry.All() is empty and
// therefore that no schema write fires. See channelkinds_registered_test.go
// and cloudimports_test.go in this package for the same "assert the real
// binary-level import set" idiom this test follows.
//
// Deliberately does NOT include SpiceboxToolkit CRs. Those are installed, not
// compiled in, and they belong to the runtime partition.
func TestCompileTimeFragmentsCompose(t *testing.T) {
	var compileTime []*spiceboxv1alpha1.SpiceDBSchemaFragment
	for _, k := range channelregistry.All() {
		sc, ok := k.(channelkinds.SchemaContributor)
		if !ok {
			continue
		}
		if frag := sc.SpiceDBSchemaFragment(); frag != nil {
			compileTime = append(compileTime, frag)
		}
	}
	toolkitFrags := guardianschema.ToolkitFragments(toolkits.All())
	compileTime = append(compileTime, toolkitFrags...)

	out, err := guardianschema.ComposeBase(compileTime)
	require.NoError(t, err, "every compile-time fragment must compose into the base")
	assert.Contains(t, out, "definition agentsession")
	assert.Contains(t, out, "definition slack_channel",
		"the Slack fragment is load-bearing: audience resolution reads slack_channel#view")

	// Every definition a built-in toolkit fragment declares must actually
	// reach the composed base. Until this loop existed, nothing in this test
	// proved that: deleting the ToolkitFragments line above (or a future
	// change that silently made toolkits.All() or ToolkitFragments return
	// nothing) left every other assertion here green, even though this test's
	// whole reason for existing (see the comment on ValidateComposedSchema
	// below) is that a bad built-in toolkit fragment has no CR to isolate it
	// and this is the only net before it reaches a cluster as a startup-time
	// schema freeze.
	require.NotEmpty(t, toolkitFrags, "at least one built-in toolkit must declare a schema fragment, or the compile-time set proves nothing about toolkits")
	toolkitNames := toolkitDefinitionNames(toolkitFrags)
	require.NotEmpty(t, toolkitNames, "at least one built-in toolkit fragment must declare a definition, or this assertion guards nothing")
	for _, name := range toolkitNames {
		assert.Containsf(t, out, "definition "+name,
			"built-in toolkit definition %q must reach the composed base", name)
	}

	// The require.NoError above is the actual net for the compile-time set:
	// composeFragmentSet — what ComposeBase composes through — validates
	// against spicedb's own type system BEFORE ComposeBase ever returns, so a
	// dangling relation/permission name or subject type in a compile-time
	// fragment would already have failed that require.NoError, before this
	// point. Neither check below repeats that class of failure; each exists
	// for what it does NOT cover.
	//
	// ValidateComposedSchema re-parses and re-validates `out` — the STRING
	// generator.GenerateSchema produced from the already-validated compiled
	// form, not the compiled form itself. Its value here is generator
	// round-trip fidelity: proving the text ComposeBase actually returns (and
	// that a real cluster receives at WriteSchema) still validates once
	// re-parsed. It is not a second, independent net over a different failure
	// class — that role belongs to UnresolvedReferences below.
	require.NoError(t, guardianschema.ValidateComposedSchema(out),
		"the composed text must still validate once re-parsed — a generator round-trip bug would surface here")

	// UnresolvedReferences catches what neither check above does. spicedb's
	// type system validates that an arrow's LEFT half (the tupleset relation)
	// resolves, but never resolves the RIGHT half: `permission view =
	// link->no_such_permission` type-checks clean and simply resolves to
	// nothing at Check time (pinned directly in
	// pkg/authz/guardian/schema/fragmentcompose_test.go,
	// TestValidateComposedSchema_AcceptsAnUnresolvedArrowRightHalf). A
	// cross-fragment dangling arrow is exactly the shape this compile-time set
	// could introduce — one channel kind or toolkit fragment arrowing into a
	// permission another fragment's definition does not declare — and exactly
	// what a bare ValidateComposedSchema swap would stop catching. Latent
	// today (no shipped fragment arrows into another fragment's definition),
	// but the check stays so a future one that does gets caught here, at
	// build time, rather than as a startup-time schema freeze on a live
	// cluster.
	unresolved, err := guardianschema.UnresolvedReferences(out)
	require.NoError(t, err)
	assert.Empty(t, unresolved,
		"a compile-time fragment must not arrow into a permission/relation name absent from its target — "+
			"see the comment above for why this is checked separately from ValidateComposedSchema")

	// Regression guard for the leading-comment leak: a comment block placed
	// immediately ABOVE a `definition` in a .zed file is parsed as that
	// definition's doc comment and re-emitted verbatim into the composed
	// schema text by composeFragmentSet's generator round-trip. Task 7 found
	// exactly this — 1.6KB of Slack's explanatory prose leaking into the
	// composed schema — and the fix was moving the block to a TRAILING
	// position (see the comment at the bottom of
	// pkg/channels/channelkinds/slack/schema.zed explaining why it must stay
	// there). Nothing else prevents that regression; this assertion is it. A
	// marker phrase lifted from the middle of that trailing block — distinct
	// enough that it appears nowhere else — must never appear in `out`.
	assert.NotContains(t, out, "connectors-prototype/schema.zed",
		"a marker phrase from slack/schema.zed's TRAILING explanatory comment must never reach the "+
			"composed schema — its presence means the block moved back above a definition and is leaking again")
}
