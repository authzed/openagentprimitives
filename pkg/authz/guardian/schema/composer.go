package schema

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/authzed/spicedb/pkg/schemadsl/compiler"
	"github.com/authzed/spicedb/pkg/schemadsl/generator"
	"github.com/authzed/spicedb/pkg/schemadsl/input"
	"github.com/go-logr/logr"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	authzschema "github.com/authzed/openagentprimitives/pkg/authz/spicedb/schema"
)

const (
	agentSessionDefinitionName = "agentsession"
	checkHashCaveatName        = "check_hash"
)

// Compose appends/updates the agentsession definition in src so that the
// union of pairs is represented as `grant_<perm>_<resType>` relations
// (each with `with check_hash and expiration`) and `check_<perm>_<resType>`
// permissions arrowing into the resource's permission of the same name.
//
// Returns the rewritten schema text and changed=true if any pair was
// added that wasn't already present. Existing relations/permissions on
// agentsession are preserved unchanged. Pairs whose ResourceType or
// Permission isn't present in the live schema are skipped; use
// ComposeWithSkipped to receive the skip list.
//
// Compose is a pure function — no I/O. Callers (the guardian controller)
// do the read-from-SpiceDB → Compose → write-to-SpiceDB orchestration.
func Compose(src string, pairs []GrantPair) (string, bool, error) {
	out, changed, _, err := ComposeWithSkipped(src, pairs)
	return out, changed, err
}

// ComposeWithSkipped is Compose plus the slice of pairs that were skipped
// because their resourceType or permission isn't declared in src.
//
// sessionLinks is an optional list of additional subject-type entries
// (e.g. "slack_channel#member") to union into the agentsession
// subject-bearing relation lines (see sessionSubjectRelations). Entries
// already present in the line are skipped. An empty or nil slice is a no-op.
func ComposeWithSkipped(src string, pairs []GrantPair, sessionLinks ...string) (string, bool, []GrantPair, error) {
	// Parse the existing schema to validate target definitions + perms.
	parsed, err := compiler.Compile(compiler.InputSchema{
		Source:       input.Source("agentsession-schema"),
		SchemaString: src,
	}, compiler.AllowUnprefixedObjectType())
	if err != nil {
		return "", false, nil, fmt.Errorf("parse schema: %w", err)
	}

	// Build a lookup: definition name → set of permission names.
	// A Relation entry whose UsersetRewrite is non-nil is a permission;
	// otherwise it is a direct relation. Same pattern as pkg/authz/spicedb/schema.go.
	defPerms := make(map[string]map[string]struct{}, len(parsed.ObjectDefinitions))
	var hasAgentSession bool
	for _, def := range parsed.ObjectDefinitions {
		perms := map[string]struct{}{}
		for _, rel := range def.GetRelation() {
			if rel.GetUsersetRewrite() != nil {
				perms[rel.GetName()] = struct{}{}
			}
		}
		defPerms[def.GetName()] = perms
		if def.GetName() == agentSessionDefinitionName {
			hasAgentSession = true
		}
	}

	if !hasAgentSession {
		return "", false, nil, fmt.Errorf("schema is missing definition %q", agentSessionDefinitionName)
	}
	if _, hasCheckHash := caveatNames(parsed)[checkHashCaveatName]; !hasCheckHash {
		return "", false, nil, fmt.Errorf("schema is missing caveat %q (operator should ensure base schema includes it)", checkHashCaveatName)
	}

	// Validate sessionLinks reference defined object types. A channel kind that
	// declares a SessionRelationLink (e.g. "slack_channel#member") MUST also
	// contribute the referenced definition via SchemaContributor. compiler.Compile
	// above validates DSL syntax but NOT cross-definition references, so an
	// undefined type would otherwise only fail later at WriteSchema against live
	// SpiceDB. Catch it here, fail-closed at compose.
	for _, link := range sessionLinks {
		linkType := link
		if i := strings.IndexByte(link, '#'); i >= 0 {
			linkType = link[:i]
		}
		if linkType == "" {
			return "", false, nil, fmt.Errorf("invalid session relation link %q (empty object type)", link)
		}
		if _, ok := defPerms[linkType]; !ok {
			return "", false, nil, fmt.Errorf("session relation link %q references undefined object type %q — the channel kind must contribute its definition via SchemaContributor", link, linkType)
		}
	}

	// Validate + filter pairs.
	pairs = DedupAndSort(pairs)
	var skipped []GrantPair
	var validPairs []GrantPair
	for _, p := range pairs {
		perms, ok := defPerms[p.ResourceType]
		if !ok {
			skipped = append(skipped, p)
			continue
		}
		if _, ok := perms[p.Permission]; !ok {
			skipped = append(skipped, p)
			continue
		}
		validPairs = append(validPairs, p)
	}

	// Find the existing agentsession block in src; we replace its body.
	blockStart, blockEnd, ok := findDefinitionBlock(src, agentSessionDefinitionName)
	if !ok {
		return "", false, nil, fmt.Errorf("could not locate %q definition in source text", agentSessionDefinitionName)
	}
	existingBlock := src[blockStart:blockEnd]

	// Parse what relation+permission lines we ALREADY have grant_*/check_*.
	existingGrantPairs := parseExistingGrantPairsFromBlock(existingBlock)

	desired := DedupAndSort(append(append([]GrantPair{}, existingGrantPairs...), validPairs...))

	// No change? Bail out early — but only if sessionLinks would also
	// produce no change to the existing owner/participant lines.
	if pairsEqual(existingGrantPairs, desired) && !linksChangeBlock(existingBlock, sessionLinks) {
		return src, false, skipped, nil
	}

	// Compose new block body.
	var inner strings.Builder
	// Keep the existing non-grant lines (started_by, participant, interact, …).
	for _, line := range strings.Split(strings.TrimSuffix(existingBlock, "\n"), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "definition ") || strings.HasPrefix(trimmed, "}") {
			continue // skip the wrapping braces; we re-emit them
		}
		if strings.HasPrefix(trimmed, "relation grant_") {
			continue // strip; we re-emit from desired
		}
		if strings.HasPrefix(trimmed, "permission check_") {
			// Tolerant: only strip lines matching one of our pairs by name.
			if isComposedCheckPermLine(trimmed) {
				continue
			}
		}
		if trimmed == "" {
			continue
		}
		// Union channel link-types into every subject-bearing relation.
		if isSessionSubjectRelation(trimmed) {
			trimmed = appendLinks(trimmed, sessionLinks)
		}
		inner.WriteString("    " + trimmed + "\n")
	}
	for _, p := range desired {
		fmt.Fprintf(&inner, "    relation %s: %s with %s and expiration\n",
			p.RelationName(), p.ResourceType, checkHashCaveatName)
		fmt.Fprintf(&inner, "    permission %s = %s->%s\n",
			p.PermissionName(), p.RelationName(), p.Permission)
	}

	newBlock := fmt.Sprintf("definition %s {\n%s}", agentSessionDefinitionName, inner.String())
	out := src[:blockStart] + newBlock + src[blockEnd:]
	return out, true, skipped, nil
}

// schemasEquivalent reports whether two schema texts describe the same schema,
// ignoring the differences SpiceDB itself introduces.
//
// The comparison cannot be textual. SpiceDB stores the COMPILED schema and
// re-renders it on read, so what comes back is never the bytes that went in:
// the scaffold's comment blocks are gone and formatting is normalized (in
// practice ~1.2KB of a ~11KB composed schema). A raw `cur == desired` is
// therefore false even at a fixed point, and the guardian rewrites the whole
// schema on every reconcile forever — churning SpiceDB's schema watch on a
// cluster where nothing changed.
//
// Both sides are canonicalized through the same compile+render the server uses,
// which makes the comparison agree with the server's own notion of identity.
//
// Fails toward writing: an empty or unparseable `cur` (fresh cluster, or a
// schema written by something else) reports not-equivalent, so the caller
// re-establishes the desired state rather than skipping the write and leaving
// the cluster silently un-composed.
func schemasEquivalent(cur, desired string) bool {
	if strings.TrimSpace(cur) == "" {
		return false
	}
	if cur == desired {
		return true // exact match: skip two compiles
	}
	canonCur, err := canonicalizeSchema(cur)
	if err != nil {
		return false
	}
	canonDesired, err := canonicalizeSchema(desired)
	if err != nil {
		return false
	}
	return canonCur == canonDesired
}

// canonicalizeSchema compiles src and re-renders it, yielding a form stable
// across the three ways two texts can describe the same schema: comments,
// whitespace, and definition ORDER.
//
// Order matters as much as the rest. generator.GenerateSchema emits definitions
// in the order given, and compiler.Compile preserves source order — but SpiceDB
// does not return definitions in the order they were written, so rendering both
// sides in their own source order still reports a difference between two
// identical schemas (observably: same byte length, differing at the first
// caveat). Sorting by name before rendering is what makes this a canonical form
// rather than merely a normalized one.
func canonicalizeSchema(src string) (string, error) {
	compiled, err := compiler.Compile(compiler.InputSchema{
		Source:       input.Source("canonicalize"),
		SchemaString: src,
	}, compiler.AllowUnprefixedObjectType())
	if err != nil {
		return "", err
	}
	defs := make([]compiler.SchemaDefinition, len(compiled.OrderedDefinitions))
	copy(defs, compiled.OrderedDefinitions)
	sort.Slice(defs, func(i, j int) bool { return defs[i].GetName() < defs[j].GetName() })
	out, _, err := generator.GenerateSchema(context.Background(), defs)
	if err != nil {
		return "", err
	}
	return out, nil
}

// caveatNames returns a set of caveat names in the compiled schema.
func caveatNames(c *compiler.CompiledSchema) map[string]struct{} {
	out := make(map[string]struct{}, len(c.CaveatDefinitions))
	for _, cav := range c.CaveatDefinitions {
		out[cav.GetName()] = struct{}{}
	}
	return out
}

// findDefinitionBlock returns the [start, end) byte indices of the text
// block matching `definition <name> { ... }` including the trailing brace,
// or ok=false if not present.
func findDefinitionBlock(src, name string) (int, int, bool) {
	marker := "definition " + name + " {"
	i := strings.Index(src, marker)
	if i < 0 {
		return 0, 0, false
	}
	depth := 0
	for j := i; j < len(src); j++ {
		switch src[j] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return i, j + 1, true
			}
		}
	}
	return 0, 0, false
}

// parseExistingGrantPairsFromBlock scans the agentsession definition block
// for lines like `relation grant_<perm>_<rt>: <rt> with check_hash and expiration`
// and returns the implied GrantPairs.
func parseExistingGrantPairsFromBlock(block string) []GrantPair {
	var out []GrantPair
	for _, line := range strings.Split(block, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "relation grant_") {
			continue
		}
		// Format: "relation grant_<perm>_<rt>: <rt> ..."
		afterRelation := strings.TrimPrefix(trimmed, "relation ")
		colonIdx := strings.Index(afterRelation, ":")
		if colonIdx < 0 {
			continue
		}
		relName := strings.TrimSpace(afterRelation[:colonIdx])
		// Subject type appears right after the colon up to the next space.
		rest := strings.TrimSpace(afterRelation[colonIdx+1:])
		subj := rest
		if sp := strings.Index(rest, " "); sp > 0 {
			subj = rest[:sp]
		}
		// grant_<perm>_<subj>  ⇒ permission = the middle slice.
		if !strings.HasPrefix(relName, "grant_") {
			continue
		}
		suffix := "_" + subj
		if !strings.HasSuffix(relName, suffix) {
			continue
		}
		perm := relName[len("grant_") : len(relName)-len(suffix)]
		out = append(out, GrantPair{ResourceType: subj, Permission: perm})
	}
	// Stable order helps subsequent reasoning.
	sort.Slice(out, func(i, j int) bool {
		if out[i].ResourceType != out[j].ResourceType {
			return out[i].ResourceType < out[j].ResourceType
		}
		return out[i].Permission < out[j].Permission
	})
	return out
}

// isComposedCheckPermLine reports whether a `permission check_X_Y = ...`
// line was emitted by us (so we can re-emit it from the desired set).
func isComposedCheckPermLine(trimmedLine string) bool {
	if !strings.HasPrefix(trimmedLine, "permission check_") {
		return false
	}
	// Heuristic: our check perms follow the pattern
	//   permission check_<perm>_<rt> = grant_<perm>_<rt>->...
	// External hand-authored "permission check_*" are unlikely; this is
	// a conservative match against our own naming.
	return strings.Contains(trimmedLine, "= grant_")
}

// appendLinks appends sorted, de-duplicated " | <link>" segments to a
// `relation x: <subjects>` line. Links already present in the line are
// skipped. Leading indentation on the line is preserved because
// appendLinks operates on the trimmed form (the caller re-adds indent).
func appendLinks(relationLine string, links []string) string {
	if len(links) == 0 {
		return relationLine
	}
	uniq := make([]string, 0, len(links))
	seen := map[string]bool{}
	for _, l := range links {
		if l == "" || seen[l] || strings.Contains(relationLine, l) {
			continue
		}
		seen[l] = true
		uniq = append(uniq, l)
	}
	sort.Strings(uniq)
	out := relationLine
	for _, l := range uniq {
		out += " | " + l
	}
	return out
}

// linksChangeBlock reports whether any of the supplied sessionLinks would
// actually add new subject-type entries to the owner or participant relation
// lines in block. It is used to determine whether to skip the early-exit
// short-circuit when grant pairs are unchanged but links have been added.
func linksChangeBlock(block string, links []string) bool {
	if len(links) == 0 {
		return false
	}
	for _, line := range strings.Split(block, "\n") {
		trimmed := strings.TrimSpace(line)
		if isSessionSubjectRelation(trimmed) {
			if appendLinks(trimmed, links) != trimmed {
				return true
			}
		}
	}
	return false
}

// sessionSubjectRelations are the agentsession relations that hold SUBJECTS,
// and therefore receive every subject type a channel kind registers via
// channelkinds.SessionRelationLinker.
//
// denied belongs here with owner and participant, and its absence was a real
// hole rather than an oversight of taste. A registered type flowed into both
// GRANTING relations automatically and silently not into the SUBTRACTING one,
// so one slack_channel#member tuple could mean an entire channel while denied
// — accepting concrete users only — had no way to name that channel back out.
// Rescinding was possible one member at a time, which does not terminate for a
// channel that keeps growing.
//
// Naming the set once is what keeps that from recurring: the parity between
// what can be granted and what can be revoked now holds by construction, and a
// future subject-bearing relation is one entry here rather than three scattered
// prefix checks to remember.
var sessionSubjectRelations = []string{"owner", "participant", "denied"}

// isSessionSubjectRelation reports whether a trimmed schema line declares one
// of the subject-bearing agentsession relations.
func isSessionSubjectRelation(trimmedLine string) bool {
	for _, rel := range sessionSubjectRelations {
		if strings.HasPrefix(trimmedLine, "relation "+rel+":") {
			return true
		}
	}
	return false
}

func pairsEqual(a, b []GrantPair) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// SchemaIO is the I/O contract the controller injects. Production
// implementation wraps pkg/authz/spicedb.Client; tests use a fake.
type SchemaIO interface {
	// ReadSchema returns the schema text currently live on the instance, used to
	// decide whether the newly composed text differs. An error must abort the
	// compose — writing without knowing the current state can drop definitions
	// another reconciler just added.
	ReadSchema(ctx context.Context) (string, error)

	// WriteSchema replaces the live schema with text WHOLESALE (SpiceDB has no
	// partial schema write), so text must always be scaffold + every fragment.
	// An error leaves the previous schema in place.
	WriteSchema(ctx context.Context, text string) error
}

// Result reports what the controller did. PrePairCount and PostPairCount are
// never written by any code path — do not read them.
type Result struct {
	Changed       bool
	AddedPairs    []GrantPair
	SkippedPairs  []GrantPair
	PrePairCount  int
	PostPairCount int
}

// Run reads the current schema, composes the desired text, and writes back
// if changed. Skipped pairs (missing definition or missing permission) are
// returned in Result.SkippedPairs so the controller can patch
// AgentSessionGrants.status accordingly.
func Run(ctx context.Context, io SchemaIO, pairs []GrantPair) (Result, error) {
	cur, err := io.ReadSchema(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("read schema: %w", err)
	}
	desired, changed, skipped, err := ComposeWithSkipped(cur, pairs)
	if err != nil {
		return Result{}, fmt.Errorf("compose: %w", err)
	}
	if !changed {
		return Result{Changed: false, SkippedPairs: skipped}, nil
	}
	if err := io.WriteSchema(ctx, desired); err != nil {
		return Result{}, fmt.Errorf("write schema: %w", err)
	}
	return Result{Changed: true, SkippedPairs: skipped}, nil
}

// The base scaffold — the code-owned definitions every composed schema starts
// from (caveats, user, group, agentsession, memory_entry, artifact,
// infoleakage_grant) — is the single canonical embedded schema in
// pkg/authz/spicedb/schema. ComposeAll assembles the dynamic MCPServer /
// channel-kind fragments + grant relations onto it (via composeFragmentSet,
// a compile through ComposeFragments, not a text concatenation). It declares
// `definition user {}`, so EmitSpicedbSchema must NOT also emit one (the
// composer would double-declare).

// ComposeAll returns the unified SpiceDB schema for the cluster: the
// code-owned scaffold (`use expiration`, `check_hash` caveat, minimal
// `agentsession` definition) assembled with the MCPServer-declared resource
// fragments and any channel-kind-contributed fragments through
// composeFragmentSet (a validating compile via ComposeFragments, not a text
// concatenation through EmitSpicedbSchema — EmitSpicedbSchema's structured-
// resource merge is still reused underneath, see composeFragmentSet's doc),
// then run through Compose to inject the `grant_<perm>_<resType>` relations
// and `check_<perm>_<resType>` permissions for each pair.
//
// mcpFragments are IdentifiedFragment entries gathered from the cluster
// (MCPServer, SidecarToolbox, SpiceboxToolkit, SpiceDBBootstrap) — each
// carries a Key/Tier identifying its contributor. Key IS read on this path:
// composeFragmentSet uses it as the fragment's name in the synthetic
// filesystem it compiles (see that function's doc for the naming scheme), so
// a Key left empty falls into the "baseline" naming branch instead of being
// named for its contributor, and two fragments sharing a Key is a hard
// duplicate-name error that fails the WHOLE compose — Key is no longer
// merely cosmetic. Tier is still unread on this path (partition.go is the
// only Tier-sorting consumer). Built-in toolkits do NOT flow through
// mcpFragments: they are compile-time, not cluster-sourced, and the caller
// folds them into channelKindFragments instead (see composeAllWithSkipped
// below for why). channelKindFragments are the static fragments contributed
// by registered channel kinds (e.g. the Slack kind's channel_slack_workspace
// definition), plus whatever else the caller unions into its baseline.
// Either may be nil.
//
// Pairs whose resourceType or permission isn't declared in the
// composed scaffold are silently dropped; callers that need to react
// to skipped pairs should use RunAll (which surfaces them in
// Result.SkippedPairs) or invoke composeAllWithSkipped via RunAll.
func ComposeAll(mcpFragments []IdentifiedFragment, channelKindFragments []*spiceboxv1alpha1.SpiceDBSchemaFragment, pairs []GrantPair, sessionLinks ...string) (string, error) {
	desired, _, err := composeAllWithSkipped(mcpFragments, channelKindFragments, pairs, nil, sessionLinks...)
	return desired, err
}

// ErrComposeFailed marks a RunAll failure that happened BEFORE any write was
// attempted: the composed schema text itself failed assembly or validation
// (composeAllWithSkipped), not the WriteSchema call. A caller can tell the two
// apart with errors.Is(err, ErrComposeFailed) and report a status reason that
// actually points at the right place — the fragment set or the composer, not
// SpiceDB, which never saw a write in this case.
var ErrComposeFailed = errors.New("compose schema (no write attempted)")

// RunAll is the fragments-aware sibling of Run: the desired schema is
// derived entirely from (mcpFragments, channelKindFragments, pairs) —
// the scaffold + MCPServer/SidecarToolbox/SpiceboxToolkit/SpiceDBBootstrap
// resources + the channel-kind/built-in-toolkit baseline + grant relations
// — and written back via io.WriteSchema.
//
// mcpFragments are IdentifiedFragment entries — each carries a Key/Tier
// identifying its contributor. Key IS read on this path, exactly as
// ComposeAll's own doc describes: composeFragmentSet names each fragment's
// synthetic file by it, an empty Key falls into the baseline naming branch,
// and a duplicate Key across fragments fails the whole compose.
//
// It reads the current schema purely to short-circuit a redundant
// write when the live text already matches; the inputs are the
// authoritative source, not the live schema.
//
// SkippedPairs is populated when a pair references a resourceType or
// permission not present in the composed fragments (e.g., the
// MCPServer that declared it was removed).
func RunAll(ctx context.Context, io SchemaIO, mcpFragments []IdentifiedFragment, channelKindFragments []*spiceboxv1alpha1.SpiceDBSchemaFragment, pairs []GrantPair, slots []SlotPair, sessionLinks ...string) (Result, error) {
	desired, skipped, err := composeAllWithSkipped(mcpFragments, channelKindFragments, pairs, slots, sessionLinks...)
	if err != nil {
		// Wrapped in ErrComposeFailed, distinguishable from the io.WriteSchema
		// failure below ("write schema: %w") on purpose: composeAllWithSkipped's
		// assembly (composeFragmentSet) now VALIDATES before this function ever
		// calls io.WriteSchema, so this branch means the write was never
		// attempted at all — no round trip, no SpiceDB error to go look at. The
		// controller distinguishes the two with errors.Is(runErr,
		// ErrComposeFailed) and reports a different status condition reason
		// (SchemaComposeFailed vs. the write-time SpiceDBWriteFailed) so an
		// operator is pointed at the compose plumbing or the fragment set,
		// not at SpiceDB's own logs, which would show nothing, because nothing
		// was sent.
		return Result{}, fmt.Errorf("%w: %w", ErrComposeFailed, err)
	}
	cur, err := io.ReadSchema(ctx)
	if err != nil {
		// Read failure isn't fatal — the inputs are authoritative, so we
		// proceed to write the desired schema. But the read error is a real
		// diagnostic (e.g. SpiceDB unreachable) the controller's runErr-based
		// log never sees, since we deliberately don't propagate it. Log it
		// here with context so it isn't silently lost.
		logr.FromContextOrDiscard(ctx).Info("guardian.schema.RunAll: ReadSchema failed; proceeding to write desired schema",
			"err", err.Error(), "pairs", len(pairs), "skipped", len(skipped))
		cur = ""
	}
	if schemasEquivalent(cur, desired) {
		return Result{Changed: false, SkippedPairs: skipped}, nil
	}
	if err := io.WriteSchema(ctx, desired); err != nil {
		return Result{}, fmt.Errorf("write schema: %w", err)
	}
	return Result{Changed: true, SkippedPairs: skipped}, nil
}

// composeFragmentSet assembles the code-owned scaffold with every fragment's
// contribution through ComposeFragments — a compile over a synthetic
// filesystem that also validates the result — rather than concatenating
// fragment text and handing the result to the old text-based compiler.
//
// It is the ONE place both the runtime path (composeAllWithSkipped, and so
// ComposeAll/RunAll) and the compile-time path (ComposeBase) assemble
// fragments, so the two cannot diverge.
//
// Two different emission shapes are combined:
//
//   - Structured Resources[] are merged/deduped across the WHOLE fragment
//     set exactly as EmitSpicedbSchema does (emitStructuredResources is the
//     shared merge logic — see spicedb_schema.go), then emitted as ONE
//     fragment named "010-resources". Two fragments that declare
//     byte-identical structured resources dedupe today; emitting Resources[]
//     per-fragment would turn that silent dedupe into a hard
//     duplicate-definition collision.
//   - Each fragment's non-empty RawZed becomes its own fragment: named
//     "100-<contributorKey>" for a CR-sourced fragment (Key set — a "/" in
//     the key, as in "namespace/name", is replaced so the name stays a
//     single path segment; ComposeFragments rejects names containing "/"),
//     or "200-baseline-<NN>" (NN = the fragment's position in frags) for a
//     compile-time baseline fragment, which carries no Key. RawZed blocks
//     were already concatenated with no dedupe, so nothing changes there.
//
// The numeric prefixes make sort order explicit for readability of the
// composed output only — ComposeFragments sorts fragment names itself, and
// correctness does not depend on the order fragments are supplied in.
//
// A fragment whose contribution is empty after trimming (no structured
// resources anywhere in the set, or a RawZed that is blank) contributes no
// file — a no-op fragment doesn't need one, and ComposeFragments treats an
// all-whitespace fragment as harmless anyway (see
// TestComposeFragments_EmptyFragmentTextIsHarmless).
func composeFragmentSet(frags []IdentifiedFragment) (string, error) {
	flat := make([]*spiceboxv1alpha1.SpiceDBSchemaFragment, 0, len(frags))
	for _, f := range frags {
		flat = append(flat, f.Fragment)
	}

	structured, err := emitStructuredResources(flat)
	if err != nil {
		return "", fmt.Errorf("emit structured resources: %w", err)
	}

	named := make([]NamedFragment, 0, len(frags)+2)
	named = append(named, NamedFragment{Name: "000-scaffold", ZED: authzschema.Schema})
	if strings.TrimSpace(structured) != "" {
		named = append(named, NamedFragment{Name: "010-resources", ZED: structured})
	}

	for i, f := range frags {
		if f.Fragment == nil || strings.TrimSpace(f.Fragment.RawZed) == "" {
			continue
		}
		var name string
		if f.Key != "" {
			name = "100-" + strings.ReplaceAll(f.Key, "/", "_")
		} else {
			name = fmt.Sprintf("200-baseline-%02d", i)
		}
		named = append(named, NamedFragment{Name: name, ZED: f.Fragment.RawZed})
	}

	return ComposeFragments(named)
}

// composeAllParseOnly is composeFragmentSet's PARSE-ONLY predecessor,
// preserved (not merely reused as a historical artifact) for the two callers
// that need to trial-compose a PARTIAL view of the eventual fragment set:
// ValidateFragment (one fragment, alone, against the bare scaffold) and
// PartitionCompatibleFragments (a candidate, or the accepted-so-far prefix,
// against the baseline).
//
// Both exist specifically because a fragment can legitimately reference a
// type only a SIBLING fragment declares — a channel kind's session type, or a
// candidate not yet walked — and composeFragmentSet's validation (this file,
// above) cannot tell that apart from a genuine bug: it resolves references
// against exactly the fragment set it was given, and a partial view makes a
// perfectly good cross-fragment reference look dangling. Both callers already
// run their OWN, more nuanced analysis afterward (UnresolvedReferences,
// deliberately biased against false positives for this exact reason:  see its
// doc) — they need PARSED TEXT to hand it, not a hard refusal with no text at
// all. Composing through ComposeFragments here would turn every legitimate
// cross-fragment reference into an immediate, unconditional rejection, before
// that analysis ever runs.
//
// This does not reopen the gap Task 4 closes. RunAll — the only path that
// actually reaches a live SpiceDB — composes through composeFragmentSet
// (validating) via composeAllWithSkipped below, so an accepted set that
// somehow still carries a genuinely dangling reference (composeAllParseOnly
// and the heuristic resolver are both best-effort, same as before this
// change) is caught there, in-process, before any write — strictly better
// than the pre-Task-4 world, where the only backstop was a live WriteSchema
// rejection.
func composeAllParseOnly(mcpFragments []IdentifiedFragment, channelKindFragments []*spiceboxv1alpha1.SpiceDBSchemaFragment, pairs []GrantPair, sessionLinks ...string) (string, error) {
	flat := make([]*spiceboxv1alpha1.SpiceDBSchemaFragment, 0, len(mcpFragments)+len(channelKindFragments))
	for _, f := range mcpFragments {
		flat = append(flat, f.Fragment)
	}
	flat = append(flat, channelKindFragments...)
	fragmentText, err := EmitSpicedbSchema(flat)
	if err != nil {
		return "", fmt.Errorf("emit fragments: %w", err)
	}
	base := authzschema.Schema + "\n" + fragmentText
	desired, _, _, err := ComposeWithSkipped(base, pairs, sessionLinks...)
	if err != nil {
		return "", fmt.Errorf("compose grants over fragments: %w", err)
	}
	return desired, nil
}

// composeAllWithSkipped is the shared core of ComposeAll/RunAll: it
// returns the desired schema text and the slice of pairs that were
// skipped because their resourceType or permission isn't declared in
// the composed fragment scaffold.
//
// mcpFragments are cluster-sourced; channelKindFragments are the
// compile-time baseline — the real caller (the guardian controller) passes
// registered channel kinds' static contributions UNIONED with the built-in
// toolkit fragments (toolkits/*.yaml, //go:embed). It has to be exactly the
// baseline: the isolation partition trial-composes each mcpFragments
// candidate against this same set before RunAll ever runs, so the two must
// agree on what's in it or a candidate that's fine against RunAll's actual
// baseline can be wrongly rejected (or the reverse). Both are merged before
// being fed to composeFragmentSet.
//
// composeAllWithSkipped previously took a logr.Logger, to receive a
// report-only unresolved-reference diagnostic over the fully composed text
// (fragments + grants + slots), logged rather than enforced.
// ValidateComposedSchema below now gates that same text, so the diagnostic
// was retired rather than left to duplicate a check that already refuses the
// write, and the parameter was removed with it rather than left threaded
// through for a difference that no longer does anything.
func composeAllWithSkipped(mcpFragments []IdentifiedFragment, channelKindFragments []*spiceboxv1alpha1.SpiceDBSchemaFragment, pairs []GrantPair, slots []SlotPair, sessionLinks ...string) (string, []GrantPair, error) {
	// channelKindFragments (the compile-time baseline) go FIRST, at fixed
	// positions, so their "200-baseline-<NN>" name (see composeFragmentSet)
	// stays the same across reconciles regardless of how many CR-sourced
	// mcpFragments are accepted that cycle. mcpFragments — each already
	// carrying the Key/Tier identity composeFragmentSet names them by — follow.
	all := make([]IdentifiedFragment, 0, len(channelKindFragments)+len(mcpFragments))
	for _, f := range channelKindFragments {
		all = append(all, IdentifiedFragment{Fragment: f})
	}
	all = append(all, mcpFragments...)
	// composeFragmentSet's own error already names what failed ("emit
	// structured resources: …" or ComposeFragments' own "compose fragments:
	// …" prefix) — wrapping it again here used to double that prefix
	// ("compose fragments: compose fragments: …"). Returned as-is.
	base, err := composeFragmentSet(all)
	if err != nil {
		return "", nil, err
	}
	desired, _, skipped, err := ComposeWithSkipped(base, pairs, sessionLinks...)
	if err != nil {
		return "", nil, fmt.Errorf("compose grants over fragments: %w", err)
	}
	// Slots emit onto the RESOURCE definitions, so they compose after the
	// fragments have been merged in and the agentsession block rebuilt.
	desired, _, skippedSlots, err := ComposeSlots(desired, slots)
	if err != nil {
		return "", nil, fmt.Errorf("compose slots over fragments: %w", err)
	}
	// Surface skipped slots through the SAME channel as skipped grant pairs, so
	// a declared-but-inert slot is reported rather than silently doing nothing.
	for _, s := range skippedSlots {
		skipped = append(skipped, GrantPair{ResourceType: s.ResourceType, Permission: s.Permission})
	}
	// The hard gate. ComposeWithSkipped's grant injection and ComposeSlots'
	// slot injection both rewrite already-assembled, already-validated text as
	// string surgery, and neither is a full type-check of its own rewrite —
	// composeOneSlot checks that the target definition declares `owner` before
	// writing that leg (definitionDeclaresName, slots.go), which closes the one
	// dangling-reference shape that check was added for, but a rewrite can
	// still land a permission/relation name collision or some other surgery
	// this stage's own checks were never meant to cover. There is no single
	// contributor to isolate at this stage the way ValidateFragment isolates a
	// bad MCPServer fragment before assembly, so the only honest response to a
	// rewrite SpiceDB would refuse is to refuse the write here and leave
	// whatever schema is already live serving.
	// A validation failure here is deliberately NOT folded into `skipped`:
	// a skipped pair/slot is a per-contributor report ("this one entry did
	// nothing"), whereas this means the compose as a WHOLE cannot be trusted.
	if err := ValidateComposedSchema(desired); err != nil {
		return "", nil, fmt.Errorf("grant/slot composition produced invalid schema: %w", err)
	}
	return desired, skipped, nil
}
