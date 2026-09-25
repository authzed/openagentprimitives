package steelthread

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"strings"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	bt "github.com/authzed/openagentprimitives/pkg/bronzethread"
	"github.com/authzed/openagentprimitives/pkg/tools/skills/canonical"
)

// SkillElision is one operator-declared substitution of a third-party skill
// source: every reference to a git authority is rewritten to a stand-in, and
// the skills that came from it are emptied of their CONTENT.
//
// # Why this is not a Redaction
//
// A Redaction is a textual replacement over the final bytes, and for a skill
// source that is precisely the wrong tool. The authority appears in a
// SkillSource's spec.repoURL, in each Skill's spec.canonicalName, in
// spec.source.repoLocator, and in the AgentClass's spec.skills[].ref — and the
// object NAMES beside them (the SkillSource's metadata.name, each Skill's, and
// the controller owner-ref that binds the two) carry the third party's name
// too, in a form no substring rule can rewrite consistently.
//
// Those references are not incidental. A Skill goes Valid=True only when
// pkg/tools/skills/materialize confirms a controller owner-ref to a
// same-namespace SkillSource that EXISTS and whose normalized spec.repoURL
// equals the canonical name's authority. Desynchronise any one of them and the
// gate fails, the Skill parks Valid=False, and the AgentClass parks at
// AgentClassSkillInvalid — a replay that never reaches step one. So the rewrite
// is structural: it parses the canonical name, moves its authority, and
// re-derives every name that referred to it.
//
// # Operator-declared, never automatic
//
// Nothing here guesses. A capture has no way to tell a third-party repo from a
// first-party one, and a heuristic that tried would either miss the ones that
// matter or rewrite the scenario away. The human who knows what the session
// pulled in names the authority; the capture rewrites exactly that one.
//
// # It REFUSES to elide a skill the agent could see
//
// Eliding is only ever safe because of a fact about the AgentClass link, not
// about the skill: a `target: sandbox` skill is staged to disk and excluded
// from both the system prompt and load_skill (internal/cmd/runner's
// resolveSkills), so the recorded run depended on its EXISTENCE and nothing
// else. A `target: agent` or `target: both` link puts the skill's description
// into the composed system prompt and its body behind load_skill; emptying one
// there replays a DIFFERENT prompt than the one recorded, and nothing
// downstream re-derives the prompt to notice.
//
// So elideSkillSources refuses on any link that is not sandbox-targeted —
// including a link whose Target is empty, which the API server defaults to
// agent. The cost of the refusal is an operator who genuinely wants it having
// to say so in a future flag; the cost of not having it is a bundle that looks
// faithful and is not.
//
// # The stand-in authority is the SAME BYTE LENGTH as the one it replaces
//
// The same rule Redaction holds, enforced the same way and for the same reason,
// because the authority half of an elision IS a redaction: one operator-supplied
// string swapped for another, over bytes that get emitted. Measured on a real
// capture of the redaction flag, before it was constrained: a handle appearing
// three times inside a rendered artifact, replaced by a token five bytes
// shorter, moved that artifact's recorded size from 5291 to 5276 and the replay
// diverged — at the artifact step, many turns from the rule that caused it, with
// nothing in the message naming a substitution.
//
// Nothing counts a repo authority's bytes TODAY. That is the whole argument for
// constraining it now rather than after: the rewrite puts the authority into
// spec.repoURL, into every canonical name, into spec.source.repoLocator and into
// the AgentClass's own skill refs, and the day any of those lands somewhere a
// count is derived from — the composed prompt, a rendered artifact, a staged
// SKILL.md — a length-changing rule starts producing exactly the failure above,
// and no test in this package would see it. A constraint that holds only while
// nobody adds a counter is not a constraint.
//
// It also keeps ONE stand-in per authority across the two flags. An operator who
// elides an authority structurally and then wants the same string gone from the
// transcript reaches for --redact, which refuses a length change; with elision
// unconstrained the two flags would demand different stand-ins for one repo, and
// the emitted fixture and the emitted transcript would disagree about what was
// there.
//
// # The CONTENT elision is deliberately NOT length-preserving
//
// The bodies, descriptions and repo instructions this drops are not padded back
// to their original size, and that asymmetry is the design rather than an
// omission. See elideSkillContent for the argument.
type SkillElision struct {
	// Old is the git authority to remove, in canonical repo-locator form
	// ("github.com/someorg/somerepo" — no scheme, no .git). Never recorded in
	// the bundle.
	Old string
	// New is the stand-in authority that replaces it — the value a reader will
	// find in the emitted manifests, and the only half the bundle records.
	//
	// EMPTY means "generate one": the operator named an authority to remove and
	// left the stand-in to the capture. ResolveSkillElisions fills it in, and
	// nothing downstream may see an empty New, because rewriting an authority
	// to the empty string would emit manifests no gate can read.
	New string
}

// ParseSkillElision reads one `old=new` rule, or a bare `old` whose stand-in the
// capture generates.
//
// Split on the FIRST `=`, matching ParseRedaction: neither half of a repo
// locator may contain one, so the asymmetry costs nothing and keeps one
// spelling across the two flags.
//
// A spec with NO `=` at all is the generated form, again matching
// ParseRedaction. The separator is a promise that a replacement follows, so
// `old=` — a promise not kept, which is what a truncated line or an interrupted
// edit looks like — stays an error, while `old` alone is the deliberate request
// for a stand-in. That form matters more here than it does for a redaction: a
// same-length replacement for a company name is a moment's work, while one for
// `github.com/someorg/some-long-repo-name` means hand-building a plausible repo
// locator to an exact byte count, and an operator made to do that by hand will
// reach for a shorter one and reintroduce the bug.
//
// Both halves must be canonical repo locators — the exact form
// canonical.Normalize produces and canonical.Parse accepts as an authority.
// Refusing anything else here rather than later is what keeps the failure
// legible: a rule written as a URL ("https://github.com/org/repo") would match
// no SkillSource, because the comparison this rewrite makes is against the
// NORMALIZED repoURL, and the operator would be left looking at a bundle that
// still carried the name they asked to remove.
//
// The reserved "local" authority is refused on both sides. A local// skill is
// hand-authored, has no SkillSource and no provenance owner-ref, so there is
// nothing here to rewrite; and moving a git skill ONTO local// would make
// validate.CheckProvenance demand the opposite of what the emitted owner-ref
// provides.
//
// A replacement of a DIFFERENT BYTE LENGTH is refused, with the exact count
// required — see SkillElision for why, and standInLengthError, which is the one
// message both flags raise so an operator meets the same rule twice rather than
// two rules that happen to agree.
func ParseSkillElision(spec string) (SkillElision, error) {
	old, replacement, found := strings.Cut(spec, "=")
	if err := validElisionAuthority("original", spec, old); err != nil {
		return SkillElision{}, err
	}
	if !found {
		// The generated form. Everything checked below about a supplied
		// replacement, ResolveSkillElisions checks about a generated one — at
		// the point it knows the whole rule set, which is what uniqueness needs.
		return SkillElision{Old: old}, nil
	}
	if replacement == "" {
		return SkillElision{}, fmt.Errorf("steelthread: skill elision %q has an empty replacement authority; "+
			"name the stand-in (old=github.com/exampleorg/examplerepo), or write the original alone (%q) to "+
			"have a same-length one generated", spec, old)
	}
	if err := validElisionAuthority("replacement", spec, replacement); err != nil {
		return SkillElision{}, err
	}
	if old == replacement {
		return SkillElision{}, fmt.Errorf("steelthread: skill elision %q replaces an authority with itself, "+
			"so every reference to it would survive the rewrite", spec)
	}
	if len(replacement) != len(old) {
		return SkillElision{}, standInLengthError(elisionStandIn.what, old, replacement)
	}
	return SkillElision{Old: old, New: replacement}, nil
}

// validElisionAuthority rejects anything that is not already a canonical repo
// locator. The check is made by round-tripping through canonical.Parse — the
// same parser the provenance gate uses — rather than by a private regexp, so a
// rule this accepts is a rule that gate can read.
func validElisionAuthority(half, spec, authority string) error {
	if authority == "" {
		return fmt.Errorf("steelthread: skill elision %q has an empty %s authority; "+
			"name the repo locator (github.com/someorg/somerepo), not a bare org", spec, half)
	}
	if canonical.Normalize(authority) != authority {
		return fmt.Errorf("steelthread: skill elision %q has a non-canonical %s authority %q; "+
			"write it the way a canonical skill name does — no scheme, no trailing slash, no .git — "+
			"because the rewrite compares it against the NORMALIZED spec.repoURL", spec, half, authority)
	}
	n, err := canonical.Parse(authority + "//placeholder")
	if err != nil {
		return fmt.Errorf("steelthread: skill elision %q has an unparseable %s authority %q: %w", spec, half, authority, err)
	}
	if n.Authority != authority {
		return fmt.Errorf("steelthread: skill elision %q has a %s authority %q that does not parse as one; "+
			"it must not contain the '//' repo/subpath separator", spec, half, authority)
	}
	if n.IsLocal {
		return fmt.Errorf("steelthread: skill elision %q uses the reserved local authority as its %s; "+
			"a local// skill has no SkillSource to rewrite, and a git skill moved onto local// would fail "+
			"the provenance gate it currently passes", spec, half)
	}
	return nil
}

// elisionStandIn is --elide-skill's generated-token layout: a reserved-TLD host
// and a path separator, so the token reads as the repo locator it stands in for
// and the emitted SkillSource still looks like the thing it is.
//
// ".example" is reserved by RFC 2606 and can never be delegated, so a generated
// authority cannot collide with a real repo — which matters more here than in a
// redaction, because this value is emitted as a spec.repoURL that a reader could
// otherwise mistake for somewhere to go and look.
//
// The trailing "/" is part of the word rather than something appended
// afterwards: when the length allows the whole word, the digest lands in a path
// segment; when it does not, the word truncates from the right like any other
// and the "/" is simply the first thing lost. Either way the accept test below
// is what decides whether the result is admissible, so no length needs a special
// case here.
var elisionStandIn = standInStem{
	word:   "elided.example/",
	domain: "steelthread-skill-elision",
	what:   "skill elision",
	accept: isElidableAuthority,
}

// isElidableAuthority reports whether a generated candidate would have been
// accepted had an operator typed it.
//
// Asks validElisionAuthority rather than re-stating its rules, so a generated
// stand-in can never be something the parser would have refused — a candidate
// ending in "/", carrying a "//", or landing on the reserved local authority.
// Truncating the layout's word at an arbitrary length is exactly what could
// produce the first two, and the generator's widening loop is what steps past a
// rejected candidate.
func isElidableAuthority(candidate string) bool {
	return validElisionAuthority("generated", candidate, candidate) == nil
}

// ResolveSkillElisions fills in a stand-in authority for every rule that did not
// name one, and validates the ones that did.
//
// The elision counterpart of ResolveRedactions, and deliberately the same shape
// — idempotent, supplied-before-generated, checked against every original — so
// the two flags cannot drift into two contracts.
//
// Idempotent: a fully-supplied rule set comes back unchanged, so the CLI can
// resolve early (to refuse a bad rule before a port-forward, a SpiceDB dial and
// a capture) and elideSkillSources can resolve again for every caller that did
// not, including this package's own tests.
//
// # What a generated authority holds, and why each one
//
// SAME BYTE LENGTH, which is the point — see SkillElision.
//
// DETERMINISTIC: a function of the original, the rule set, and nothing else. A
// re-capture of the same session with the same rules must produce byte-identical
// manifests, or every re-capture is a diff nobody can read.
//
// COLLISION-FREE across the rule set. Two authorities elided onto one stand-in
// merge two distinct skills into one canonical name; noEmittedNameCollisions
// catches that after the rewrite, but catching it by construction here means an
// operator never sees it at all.
//
// ADMISSIBLE as an authority, checked by the parser's own validator rather than
// assumed from the layout.
//
// # What a candidate is checked against
//
// Every replacement already claimed, and every rule's ORIGINAL. The second is
// not fussiness: elideOne runs the rules in order over one fixture, so a later
// rule whose original equalled an earlier rule's stand-in would rewrite that
// stand-in and emit an authority the bundle does not record.
//
// Two SUPPLIED replacements may be equal — an operator deliberately collapsing
// two repos onto one stand-in is their call, and the collision guard downstream
// is what decides whether the result is emittable. Only generated tokens are
// held to uniqueness here, because only there is the collision an accident
// nobody chose.
func ResolveSkillElisions(rules []SkillElision) ([]SkillElision, error) {
	if len(rules) == 0 {
		return nil, nil
	}
	out := make([]SkillElision, len(rules))
	copy(out, rules)

	olds := make([]string, 0, len(out))
	for _, r := range out {
		// Re-checked here and not only in the parser, because a programmatic
		// caller — every test in this package — builds SkillElision values
		// directly and must meet the same rules the CLI does.
		if err := validElisionAuthority("original", r.Old, r.Old); err != nil {
			return nil, err
		}
		olds = append(olds, r.Old)
	}

	// Supplied replacements are claimed FIRST, so a generated token can never
	// take one of them. Order matters here and nowhere else: claiming before
	// generating is what makes the outcome independent of where in the list the
	// generated rules happen to sit.
	taken := make(map[string]bool, len(out))
	for _, r := range out {
		if r.New == "" {
			continue
		}
		if err := validElisionAuthority("replacement", r.New, r.New); err != nil {
			return nil, err
		}
		if r.New == r.Old {
			return nil, fmt.Errorf("steelthread: the skill elision replacing %q with itself would leave "+
				"every reference to it in the emitted manifests", r.Old)
		}
		if len(r.New) != len(r.Old) {
			return nil, standInLengthError(elisionStandIn.what, r.Old, r.New)
		}
		taken[r.New] = true
	}

	for i, r := range out {
		if r.New != "" {
			continue
		}
		tok, err := generateStandIn(r.Old, taken, olds, elisionStandIn)
		if err != nil {
			return nil, err
		}
		taken[tok] = true
		out[i].New = tok
	}
	return out, nil
}

// Placeholder content every elided skill gets.
//
// Both are NON-EMPTY on purpose, and neither is arbitrary: pkg/tools/skills/
// validate.Skill runs for real at replay (the harness registers the Skill
// controller) and refuses an empty body, an empty description, a description
// over 1024 chars, one carrying an XML/HTML tag, and one carrying a reserved
// provider word. An elision that emptied the fields outright would emit a
// Skill that parks Valid=False and an AgentClass that parks with it.
const (
	elidedSkillDescription = "Content elided at capture time. This skill's description, body and repo " +
		"instructions were removed by an operator-declared skill elision; see capture.skillElisions in " +
		"bundle.json. Only the skill's existence is reproduced."

	elidedSkillBody = "# Content elided\n\n" +
		"This skill's SKILL.md body was removed by an operator-declared skill elision at capture time.\n\n" +
		"The AgentClass link that names it targets the sandbox, so none of this content reached the\n" +
		"recorded system prompt or load_skill, and the replay depends only on the skill existing.\n"
)

// elideSkillSources applies every rule to the fixture and reports what each one
// removed.
//
// Returns a fixture whose Class, Skills and SkillSources are DEEP COPIES: the
// caller's manifests were read off a live cluster and are used elsewhere in the
// capture (the meta-tool prediction, the live-secret scan), so a rewrite that
// mutated them in place would change what those saw depending on call order.
//
// Refuses, rather than doing part of the job, on:
//
//   - any AgentClass link to the elided authority that is not sandbox-targeted
//     (see SkillElision — this is the whole safety gate);
//   - a gathered Skill on the elided authority that NO class link names, whose
//     target is therefore unknowable and so cannot be shown to be safe;
//   - a rule that matched nothing. Unlike a redaction — where a rule matching
//     no prose is ordinary, and is recorded with a zero count — an elision
//     names a STRUCTURED object the fixture either has or does not. The only
//     way to reach zero is a mistyped authority, and the cost of accepting it
//     is shipping the name the operator asked to remove;
//   - a rewrite that would collide two object names or two canonical names,
//     which would emit two manifests the API server cannot both hold.
func elideSkillSources(f FixtureInput, rules []SkillElision) (FixtureInput, []bt.SkillElision, error) {
	if len(rules) == 0 {
		return f, nil, nil
	}
	// HERE rather than only in the CLI, so the length rule and the generated
	// stand-in reach every caller — Capture, and every programmatic caller that
	// built SkillElision values without going through ParseSkillElision. A rule
	// set the CLI already resolved comes back unchanged.
	rules, err := ResolveSkillElisions(rules)
	if err != nil {
		return FixtureInput{}, nil, err
	}
	if f.Class == nil {
		return FixtureInput{}, nil, fmt.Errorf("steelthread: skill elision needs the AgentClass, " +
			"because the target that says whether eliding is safe lives on its spec.skills[] link, not on the Skill")
	}

	out := f
	out.Class = f.Class.DeepCopy()
	out.Skills = deepCopyEach(f.Skills)
	out.SkillSources = deepCopyEach(f.SkillSources)

	records := make([]bt.SkillElision, 0, len(rules))
	for _, rule := range rules {
		rec, err := elideOne(&out, rule)
		if err != nil {
			return FixtureInput{}, nil, err
		}
		records = append(records, rec)
	}

	if err := noEmittedNameCollisions(out); err != nil {
		return FixtureInput{}, nil, err
	}
	return out, records, nil
}

// elideOne applies a single rule to the (already copied) fixture in place.
func elideOne(f *FixtureInput, rule SkillElision) (bt.SkillElision, error) {
	if err := checkElisionTargets(f, rule); err != nil {
		return bt.SkillElision{}, err
	}

	rec := bt.SkillElision{Authority: rule.New}

	// Sources first: each Skill's owner-ref and spec.source.sourceName name a
	// SkillSource by object name, so the new names have to exist before the
	// skills that point at them are rewritten.
	renamedSources := map[string]string{}
	for _, src := range f.SkillSources {
		if canonical.Normalize(src.Spec.RepoURL) != rule.Old {
			continue
		}
		newName := elidedSourceName(rule.New, src.Spec.Subpath, src.Spec.Ref)
		renamedSources[src.Name] = newName
		src.Name = newName
		// Rebuilt from the stand-in authority rather than patched, so the value
		// canonical.Normalize sees is exactly what the canonical names below
		// carry — which is the equality the provenance gate makes.
		src.Spec.RepoURL = "https://" + rule.New
		rec.Sources++
	}

	for _, sk := range f.Skills {
		n, err := canonical.Parse(sk.Spec.CanonicalName)
		if err != nil || n.Authority != rule.Old {
			continue
		}
		n.Authority = rule.New
		sk.Spec.CanonicalName = n.String()
		// The same derivation the SkillSource controller uses for a skill it
		// materializes, so the emitted name is the one production would have
		// given this canonical name.
		sk.Name = n.SafeSlug()
		for i := range sk.OwnerReferences {
			ref := &sk.OwnerReferences[i]
			if ref.Kind != "SkillSource" || ref.APIVersion != spiceboxv1alpha1.SchemeGroupVersion.String() {
				continue
			}
			if renamed, ok := renamedSources[ref.Name]; ok {
				ref.Name = renamed
			}
		}
		if src := sk.Spec.Source; src != nil {
			if canonical.Normalize(src.RepoLocator) == rule.Old {
				src.RepoLocator = rule.New
			}
			if renamed, ok := renamedSources[src.SourceName]; ok {
				src.SourceName = renamed
			}
			// A commit in the real repo, which the stand-in authority does not
			// have. Informational at replay — nothing reads spec.source — so
			// dropping it costs nothing and keeps the emitted provenance from
			// pointing back at the repo the elision removed.
			src.ResolvedSHA = ""
		}
		rec.ElidedBytes += elideSkillContent(&sk.Spec)
		rec.Skills++
	}

	// The class's refs are the same strings, rewritten the same way, so the
	// emitted ref and the emitted canonicalName cannot disagree. Rewritten even
	// where no Skill CR was gathered for the ref: leaving one behind would emit
	// a class carrying the original authority, which is the one thing this
	// whole mechanism exists to prevent. A ref left pointing at a skill nobody
	// emitted is caught downstream by CodeSkillsNotCaptured.
	for i := range f.Class.Spec.Skills {
		link := &f.Class.Spec.Skills[i]
		n, err := canonical.Parse(link.Ref)
		if err != nil || n.Authority != rule.Old {
			continue
		}
		n.Authority = rule.New
		link.Ref = n.String()
	}

	if rec.Sources == 0 && rec.Skills == 0 {
		return bt.SkillElision{}, fmt.Errorf("steelthread: skill elision matched no SkillSource and no Skill; "+
			"nothing was rewritten and the authority it names is still in the emitted manifests. "+
			"The comparison is against the NORMALIZED spec.repoURL and the canonical name's authority, "+
			"so check the spelling against one of those: %v", authoritiesInFixture(*f))
	}
	return rec, nil
}

// checkElisionTargets is the safety gate. See SkillElision for why the AgentClass
// link's target — not anything on the Skill — is what decides whether eliding
// this source is faithful.
func checkElisionTargets(f *FixtureInput, rule SkillElision) error {
	named := map[string]bool{}
	for _, link := range f.Class.Spec.Skills {
		n, err := canonical.Parse(link.Ref)
		if err != nil || n.Authority != rule.Old {
			continue
		}
		named[link.Ref] = true
		target := link.Target
		if target == "" {
			// An AgentSkill read before the API server applies the CRD's
			// +kubebuilder:default=agent carries "", and every consumer in the
			// tree treats that as the explicit agent default (skillsToStage,
			// validateSkillsSpec, resolveSkills). Treating it as "unset, so
			// probably harmless" is the one reading that would let a
			// prompt-visible skill through.
			target = spiceboxv1alpha1.SkillTargetAgent
		}
		if target != spiceboxv1alpha1.SkillTargetSandbox {
			return fmt.Errorf("steelthread: refusing to elide skill %q (AgentClass %s spec.skills[%s]): "+
				"its target is %q, so its description goes into the composed system prompt and its body is "+
				"served by load_skill. Emptying it would replay a DIFFERENT prompt than the one recorded, and "+
				"nothing downstream re-derives the prompt to notice. Only a target=sandbox skill — staged to "+
				"disk, excluded from the prompt and from load_skill — depends on nothing but its own existence",
				link.Ref, f.Class.Name, link.Name, target)
		}
	}

	for _, sk := range f.Skills {
		n, err := canonical.Parse(sk.Spec.CanonicalName)
		if err != nil || n.Authority != rule.Old {
			continue
		}
		if !named[sk.Spec.CanonicalName] {
			return fmt.Errorf("steelthread: refusing to elide Skill %q (%s): no AgentClass %s spec.skills[] link "+
				"names it, so the target that decides whether eliding is faithful cannot be read. A skill this "+
				"capture gathered but the class does not opt into should not be in the fixture at all",
				sk.Name, sk.Spec.CanonicalName, f.Class.Name)
		}
	}
	return nil
}

// elideSkillContent empties the fields that ARE the third party's material and
// returns how many bytes went.
//
// # This does NOT preserve byte length, and must not
//
// The authority half of an elision is held to the same byte length as what it
// replaces (see SkillElision). The CONTENT half is not, and the asymmetry is the
// design. Three answers were available and only one of them is right:
//
// PAD the placeholder back to the original's size. This defeats the feature. A
// 59 KB skill body padded to 59 KB of filler commits the same bulk to the repo
// the elision existed to keep out, with none of the content — the same review
// burden, the same diff weight, and a fixture whose bytes are now noise a reader
// has to scroll past. "Some byte total is preserved" was never the goal; "no
// derived count can silently desync" was, and padding buys the second only by
// paying the entire price of the first.
//
// RECOMPUTE the counts that depend on the content. There are none to recompute,
// which is the next paragraph.
//
// REFUSE when a recorded count depends on the content. This is what already
// happens, and it is why eliding is safe at all — it is just spelled as the
// target gate rather than as a length check. checkElisionTargets refuses every
// link that is not sandbox-targeted, and for a sandbox-targeted skill
// internal/cmd/runner's resolveSkills `continue`s BEFORE it reads Description,
// Body or RepoInstructions. None of it reaches ComposeSystem, none of it reaches
// load_skill, and nothing downstream derives a count from it. The gate is the
// same-length guarantee for content, expressed as the structural fact that makes
// it true rather than as an arithmetic coincidence.
//
// The stronger point: a padded body would be LESS faithful, not more. It is
// still not the recorded body, so anything measuring it would get the right
// number over the wrong bytes — the worst failure of the three, because the
// count agrees and nothing is left to notice that the content does not.
//
// # What stays, and why each one has to
//
//   - frontmatter.name, and with it DisplayName, which production sets from it.
//     validate.Skill requires it to equal the canonical name's last subpath
//     segment, and the sandbox staging path requires the AgentClass link's own
//     Name to equal it (Claude Code discovers a skill only when they agree).
//     It is a directory basename, not content.
//   - spec.source's subpath and ref, which describe where in the stand-in repo
//     this came from. Rewriting them would require rewriting the canonical name's
//     subpath, hence frontmatter.name, hence the class link's Name — a chain
//     with no third-party ORG in it and every opportunity to desynchronise.
func elideSkillContent(spec *spiceboxv1alpha1.SkillSpec) int {
	n := len(spec.Body) + len(spec.Description)
	spec.Body = elidedSkillBody
	spec.Description = elidedSkillDescription

	if ri := spec.RepoInstructions; ri != nil {
		n += len(ri.Content)
		// Dropped whole rather than emptied: the field is +optional and nil is
		// what a skill with no repo-root instructions file carries, so nil is
		// the shape the rest of the tree already handles.
		spec.RepoInstructions = nil
	}

	fm := &spec.Frontmatter
	n += len(fm.License) + len(fm.Compatibility) + len(fm.AllowedTools)
	for k, v := range fm.Metadata {
		n += len(k) + len(v)
	}
	*fm = spiceboxv1alpha1.SkillFrontmatter{Name: fm.Name}
	return n
}

// unsafeSourceNameRe matches every run this rewrite must not put in a
// metadata.name. It mirrors canonical's own (unexported) sanitizer rather than
// calling Name.SafeSlug, because SafeSlug's uniqueness comes from hashing a
// CANONICAL NAME and a SkillSource has none — two sources on one repo differ
// only by subpath and ref, which is what elidedSourceName hashes instead.
var unsafeSourceNameRe = regexp.MustCompile(`[^a-z0-9.-]+`)

// elidedSourceName derives the stand-in SkillSource's metadata.name.
//
// Deterministic — a re-capture of the same session must be byte-identical — and
// unique per (authority, subpath, ref), which is the triple that distinguishes
// two SkillSources on one repo. The base is the stand-in repo's own name so the
// emitted manifest still reads like the thing it is.
func elidedSourceName(authority, subpath, ref string) string {
	base := authority
	if i := strings.LastIndex(base, "/"); i >= 0 {
		base = base[i+1:]
	}
	base = strings.Trim(unsafeSourceNameRe.ReplaceAllString(strings.ToLower(base), "-"), "-.")
	if base == "" {
		base = "skillsource"
	}
	sum := sha256.Sum256([]byte(authority + "\x00" + subpath + "\x00" + ref))
	name := base + "-" + hex.EncodeToString(sum[:])[:8]
	if len(name) > 253 {
		name = name[:253]
	}
	return name
}

// noEmittedNameCollisions refuses a rewrite that produced two objects the API
// server cannot both hold, or two Skills claiming one identity.
//
// Reachable without a bug here: an operator eliding two different authorities
// onto ONE stand-in, or onto an authority the fixture already carries, merges
// two distinct skills into one canonical name. The rewrite is otherwise
// perfectly consistent, so nothing later would notice — the harness would apply
// the second manifest over the first and the replay would run against a fixture
// missing a skill.
func noEmittedNameCollisions(f FixtureInput) error {
	sourceNames := map[string]bool{}
	for _, src := range f.SkillSources {
		if sourceNames[src.Name] {
			return fmt.Errorf("steelthread: skill elision produced two SkillSource manifests named %q; "+
				"eliding two authorities onto one stand-in merges them", src.Name)
		}
		sourceNames[src.Name] = true
	}
	skillNames := map[string]bool{}
	canonicalNames := map[string]bool{}
	for _, sk := range f.Skills {
		if skillNames[sk.Name] {
			return fmt.Errorf("steelthread: skill elision produced two Skill manifests named %q", sk.Name)
		}
		skillNames[sk.Name] = true
		if canonicalNames[sk.Spec.CanonicalName] {
			return fmt.Errorf("steelthread: skill elision produced two Skills whose canonical name is %q; "+
				"the AgentClass ref would resolve to whichever manifest the harness applied last",
				sk.Spec.CanonicalName)
		}
		canonicalNames[sk.Spec.CanonicalName] = true
	}
	return nil
}

// authoritiesInFixture is what a no-match refusal offers instead of a bare
// "nothing matched": the authorities the fixture actually carries, so an
// operator who mistyped one can see the spelling the comparison is against.
func authoritiesInFixture(f FixtureInput) []string {
	seen := map[string]bool{}
	for _, src := range f.SkillSources {
		if a := canonical.Normalize(src.Spec.RepoURL); a != "" {
			seen[a] = true
		}
	}
	for _, sk := range f.Skills {
		if n, err := canonical.Parse(sk.Spec.CanonicalName); err == nil {
			seen[n.Authority] = true
		}
	}
	out := make([]string, 0, len(seen))
	for a := range seen {
		out = append(out, a)
	}
	sort.Strings(out)
	return out
}

// deepCopyEach copies a slice of CRs so the rewrite cannot reach the caller's
// live objects. See elideSkillSources.
func deepCopyEach[T interface{ DeepCopy() T }](in []T) []T {
	if in == nil {
		return nil
	}
	out := make([]T, 0, len(in))
	for _, v := range in {
		out = append(out, v.DeepCopy())
	}
	return out
}
