package steelthread

import (
	"fmt"
	"regexp/syntax"
	"strings"

	"github.com/authzed/openagentprimitives/pkg/platform/identity/passthroughcatalog"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/provider"
)

// fixtureMarker is what a generated placeholder spells wherever the declared
// shape leaves a free choice, so the value reads as obviously fake to a person
// and to a secret scanner. A fixture Secret holding "ghp_AAAAAAAA" looks like a
// leaked token; one holding "ghp_fixtureplaceholder" does not.
const fixtureMarker = "fixtureplaceholder"

// placeholderCredentialValue returns the value a fixture Secret carries for one
// credential key.
//
// The plain "fixture-placeholder-<key>" is right for almost every credential and
// wrong for the ones whose provider declares a TOKEN SHAPE. The AgentIdentity
// reconciler validates a stored value against that shape (checkCredentialShape)
// precisely because a Secret written out of band reaches it having passed
// through none of the interactive entry points — and a fixture Secret is exactly
// such a Secret. Left generic, the replayed AgentIdentity goes
// Valid=False/CredentialShapeMismatch, the AgentClass never reaches Valid=True,
// and the bundle is SKIPPED: a suite that found a scenario and ran none.
//
// The provider is resolved through the EMBEDDED toolkit catalog only
// (passthroughcatalog.ProviderIDFromToolkits), which is a pure lookup needing no
// cluster. That is a deliberate limit rather than an oversight: RewriteFixture
// is a pure function of FixtureInput — its determinism promise depends on it —
// so the cluster-consulting half of provider resolution (an MCPServer's
// spec.auth.provider) is out of reach here. A credential resolving only that way
// keeps the generic placeholder, which is what it had before; the same failure
// would then surface as it did here, in the replay, naming the credential.
//
// Returns an error rather than a best-effort value when a shape IS declared and
// no value satisfying it could be built. Emitting one the provider's own
// validator rejects would put the failure minutes downstream, in a suite skip
// that names an AgentClass condition instead of naming this credential.
func placeholderCredentialValue(credName, key string) (string, error) {
	generic := genericPlaceholderPrefix + key

	id := passthroughcatalog.ProviderIDFromToolkits(credName)
	if id == "" {
		return generic, nil
	}
	p, ok := provider.ByID(id)
	if !ok || p.TokenShape == nil || p.TokenShape.Pattern == "" {
		return generic, nil
	}

	sample, err := sampleMatching(p.TokenShape.Pattern)
	if err != nil {
		return "", fmt.Errorf("steelthread: RewriteFixture: credential %q resolves to provider %q, whose "+
			"declared token shape %q could not be turned into a placeholder value (%w). The replayed "+
			"AgentIdentity would go Valid=False/CredentialShapeMismatch and the bundle would be skipped",
			credName, id, p.TokenShape.Pattern, err)
	}

	// Prefer a longer, unmistakably-fake value when the shape still accepts one:
	// a prefix-only pattern (the common case — "^(gh[a-z]_|github_pat_)") does,
	// while a fully anchored one does not, and the provider's own validator is
	// the only thing that can say which this is.
	if extended := sample + "-" + fixtureMarker; provider.ValidateToken(*p, extended) == nil {
		sample = extended
	}
	if verr := provider.ValidateToken(*p, sample); verr != nil {
		return "", fmt.Errorf("steelthread: RewriteFixture: credential %q resolves to provider %q, and the "+
			"placeholder built from its declared token shape %q is still rejected by the provider's own "+
			"validator: %v", credName, id, p.TokenShape.Pattern, verr)
	}
	return sample, nil
}

// sampleMatching builds the shortest string matching an anchored regexp, biased
// to spell fixtureMarker wherever a character class leaves a choice.
//
// Hand-rolled over regexp/syntax rather than taken from a library because none
// is in go.mod and this is the only caller: the alternative is a new
// supply-chain edge for a function whose entire correctness condition is
// checked by the caller against provider.ValidateToken — the real validator, so
// a wrong sample is refused rather than shipped.
//
// Repetition operators contribute their MINIMUM: `x*` and `x?` contribute
// nothing, `x+` contributes one. Producing the shortest match keeps the output
// predictable and keeps RewriteFixture byte-identical across runs, which its own
// determinism promise requires.
func sampleMatching(pattern string) (string, error) {
	re, err := syntax.Parse(pattern, syntax.Perl)
	if err != nil {
		return "", fmt.Errorf("parsing the declared pattern: %w", err)
	}
	var b strings.Builder
	if err := writeSample(&b, re.Simplify()); err != nil {
		return "", err
	}
	if b.Len() == 0 {
		return "", fmt.Errorf("the pattern matches the empty string, which is not a usable credential value")
	}
	return b.String(), nil
}

// writeSample appends one matching string for re to b.
func writeSample(b *strings.Builder, re *syntax.Regexp) error {
	switch re.Op {
	case syntax.OpEmptyMatch, syntax.OpNoMatch,
		syntax.OpBeginLine, syntax.OpEndLine, syntax.OpBeginText, syntax.OpEndText,
		syntax.OpWordBoundary, syntax.OpNoWordBoundary:
		// Anchors and assertions match no characters.
		return nil

	case syntax.OpLiteral:
		b.WriteString(string(re.Rune))
		return nil

	case syntax.OpAnyChar, syntax.OpAnyCharNotNL:
		b.WriteString(pickRune(b, [][2]rune{{'a', 'z'}}))
		return nil

	case syntax.OpCharClass:
		if len(re.Rune) < 2 {
			return fmt.Errorf("a character class matching nothing cannot be sampled")
		}
		pairs := make([][2]rune, 0, len(re.Rune)/2)
		for i := 0; i+1 < len(re.Rune); i += 2 {
			pairs = append(pairs, [2]rune{re.Rune[i], re.Rune[i+1]})
		}
		b.WriteString(pickRune(b, pairs))
		return nil

	case syntax.OpCapture:
		return writeSample(b, re.Sub[0])

	case syntax.OpConcat:
		for _, sub := range re.Sub {
			if err := writeSample(b, sub); err != nil {
				return err
			}
		}
		return nil

	case syntax.OpAlternate:
		// First branch: Simplify has already normalized the tree, and any branch
		// is as valid as another. Taking the first keeps the output stable.
		if len(re.Sub) == 0 {
			return fmt.Errorf("an alternation with no branches cannot be sampled")
		}
		return writeSample(b, re.Sub[0])

	case syntax.OpStar, syntax.OpQuest:
		return nil // minimum repetition is zero

	case syntax.OpPlus:
		return writeSample(b, re.Sub[0])

	case syntax.OpRepeat:
		for i := 0; i < re.Min; i++ {
			if err := writeSample(b, re.Sub[0]); err != nil {
				return err
			}
		}
		return nil

	default:
		return fmt.Errorf("unsupported regexp construct %v", re.Op)
	}
}

// pickRune chooses a character from the allowed ranges, preferring the next
// letter of fixtureMarker so a generated fill spells something recognizable
// instead of a run of 'A's that reads like a real token.
//
// Falls back to the first rune of the first range when the marker's letter is
// not allowed — correctness comes first, legibility second.
func pickRune(b *strings.Builder, ranges [][2]rune) string {
	if want := rune(fixtureMarker[b.Len()%len(fixtureMarker)]); inRanges(want, ranges) {
		return string(want)
	}
	for _, r := range ranges {
		if r[0] <= r[1] {
			return string(r[0])
		}
	}
	return ""
}

func inRanges(r rune, ranges [][2]rune) bool {
	for _, rr := range ranges {
		if r >= rr[0] && r <= rr[1] {
			return true
		}
	}
	return false
}
