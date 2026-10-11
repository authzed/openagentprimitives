package haiku_test

// corpus_test.go loads testdata/corpus.json -- the 51 labeled cases ported
// from ~/Projects/laya/spike_corpus.py -- and converts each raw case into the
// judge.Action/judge.PageContext shape a real ConsequenceJudge consumes, plus
// its gold tier. Shared by haiku_test.go (fake-provider plumbing + threshold
// policy tests) and haiku_corpus_eval_test.go (the env-gated live-Haiku
// accuracy eval), so both exercise the exact same 51 cases.
import (
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/authzed/openagentprimitives/pkg/agent/tool/browser/judge"
)

// corpusCase is one labeled example, already translated into judge types.
type corpusCase struct {
	Note   string
	Action judge.Action
	Page   judge.PageContext
	Gold   judge.Tier
}

// rawCorpusCase is testdata/corpus.json's wire shape: the DOM-ish facts the
// python spike corpus captured for each case, before translating into a
// judge.Action. Carries every field the python source has (including ones
// this port does not use, e.g. "classes", "tag", "aria_expanded") so the JSON
// stays a traceable 1:1 port of spike_corpus.py's CASES, independent of which
// fields this translation happens to read.
type rawCorpusCase struct {
	Gold         int    `json:"gold"`
	Note         string `json:"note"`
	Op           string `json:"op"`
	Tag          string `json:"tag,omitempty"`
	Role         string `json:"role,omitempty"`
	Text         string `json:"text,omitempty"`
	Classes      string `json:"classes,omitempty"`
	AriaExpanded string `json:"aria_expanded,omitempty"`
	AriaLabel    string `json:"aria_label,omitempty"`
	InputType    string `json:"input_type,omitempty"`
	FieldLabel   string `json:"field_label,omitempty"`
	Href         string `json:"href,omitempty"`
	FormAction   string `json:"form_action,omitempty"`
	TargetURL    string `json:"target_url,omitempty"`
	PageURL      string `json:"page_url"`
}

// loadCorpus reads and converts testdata/corpus.json. Fails the test (not
// the eval it's used from) on any read/parse/convert error -- a broken
// fixture is a test-infra bug, not a judge finding.
func loadCorpus(t *testing.T) []corpusCase {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "corpus.json"))
	if err != nil {
		t.Fatalf("read testdata/corpus.json: %v", err)
	}
	var cases []rawCorpusCase
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatalf("parse testdata/corpus.json: %v", err)
	}
	out := make([]corpusCase, 0, len(cases))
	for _, rc := range cases {
		out = append(out, rc.toCorpusCase(t))
	}
	return out
}

// toCorpusCase translates one raw case into a judge.Action/PageContext,
// mirroring the real derivation pkg/agent/tool/browser's actionfacts.go and
// pagestate.go perform, field by field:
//
//   - Role: the raw corpus already carries the final resolved AX role for
//     every click/tab/option/etc. target (matching pagestate.go's roleOf()
//     output), EXCEPT a "type" op's generic text/password input, which the
//     spike always leaves unset. Production's roleOf() resolves that case to
//     "textbox" (tag=input, type != checkbox/radio/submit/button/reset/image),
//     so that's the one case this loader derives rather than copies.
//
//   - Text: production's accessible-text extraction (actionfacts.go's text(),
//     pagestate.go's text()) prefers aria-label over innerText/textContent
//     over the element's current .value, in that order. The raw corpus's
//     "text" field holds the click target's visible label for click/tab/etc
//     cases (correct as-is), but for a "type" case it is deliberately the
//     literal VALUE BEING TYPED -- the spike's own probe into whether typed
//     content helps classification. Action.Text in production is never the
//     in-flight keystrokes (actionFactsScript reads the DOM node's OWN
//     accessible name, not a tool argument); the input's aria-label is the
//     realistic stand-in, so a "type" case maps Text from field_label, not
//     text. A non-empty aria_label always wins over both, matching the real
//     precedence order.
//
//   - FormAction: resolve whichever of href / form_action / target_url the
//     case carries against page_url the same way a browser resolves a
//     relative href/action to an absolute URL (Element.href / Element.action)
//     before actionfacts.go's urlPath() takes just the .Path -- so a
//     fragment-only href ("#baggage-details") or a query-only href
//     ("?page=2") resolves to the CURRENT page's path, exactly as the real
//     DOM would report it.
//
//   - Container is left "": the python corpus has no ancestor-container
//     concept (its "classes" field is the TARGET's own classes, not an
//     ancestor's), and Container is not read by anything this corpus tests
//     (not the deterministic judge, not this taxonomy prompt).
//
//   - PageContext.VisibleTextSample is left empty: the python spike's own
//     router.predict call never fed page-wide visible text either (build_state
//     excludes only "gold"/"note" and keeps everything else, and the spike
//     never set such a field) -- so leaving it empty keeps parity with what
//     the spike itself actually tested.
func (rc rawCorpusCase) toCorpusCase(t *testing.T) corpusCase {
	t.Helper()

	role := rc.Role
	if role == "" && rc.Op == "type" {
		role = "textbox"
	}

	text := rc.Text
	if rc.AriaLabel != "" {
		text = rc.AriaLabel
	} else if rc.Op == "type" && rc.FieldLabel != "" {
		text = rc.FieldLabel
	}

	formAction := ""
	switch {
	case rc.FormAction != "":
		formAction = resolvedPathFor(t, rc.FormAction, rc.PageURL)
	case rc.Href != "":
		formAction = resolvedPathFor(t, rc.Href, rc.PageURL)
	case rc.TargetURL != "":
		formAction = resolvedPathFor(t, rc.TargetURL, rc.PageURL)
	}

	host, path := "", ""
	if u, err := url.Parse(rc.PageURL); err == nil {
		host, path = u.Host, u.Path
	}

	gold, ok := goldTierFor(rc.Gold)
	if !ok {
		t.Fatalf("corpus case %q: unrecognized gold tier %d", rc.Note, rc.Gold)
	}

	return corpusCase{
		Note: rc.Note,
		Action: judge.Action{
			Op:         rc.Op,
			Role:       role,
			Text:       text,
			FormAction: formAction,
			Host:       host,
			PageURL:    rc.PageURL,
		},
		Page: judge.PageContext{URLPath: path},
		Gold: gold,
	}
}

// resolvedPathFor resolves rawRef (a relative href, a fragment-only
// reference, or an already-absolute URL) against pageURL, mirroring how a
// browser resolves Element.href/.action before
// pkg/agent/tool/browser/actionfacts.go's urlPath() takes just the path.
func resolvedPathFor(t *testing.T, rawRef, pageURL string) string {
	t.Helper()
	base, err := url.Parse(pageURL)
	if err != nil {
		t.Fatalf("parse page_url %q: %v", pageURL, err)
	}
	ref, err := url.Parse(rawRef)
	if err != nil {
		t.Fatalf("parse ref %q: %v", rawRef, err)
	}
	return base.ResolveReference(ref).Path
}

func goldTierFor(gold int) (judge.Tier, bool) {
	switch gold {
	case 0:
		return judge.TierObserve, true
	case 1:
		return judge.TierNavigate, true
	case 2:
		return judge.TierSession, true
	case 3:
		return judge.TierMutate, true
	default:
		return "", false
	}
}
