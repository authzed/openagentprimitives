package permsurface

import (
	"strings"
	"unicode"
)

// Describe renders the descriptor as a phrase a human can act on.
//
// A handle is a WIRE FORMAT — `perm:list:crm_company` encodes an action and a
// resource type for code to match on, and it is the wrong thing to put in front
// of somebody deciding whether to grant it. An approval card that lists raw
// handles asks its reader to already know the grammar, which makes the decision
// slower and worse exactly where care matters most.
//
// This stays COMPUTED, never agent-authored: every part comes from the
// descriptor the runtime resolved against the live envelope, so a card built
// from it remains safe to present as the authoritative description of the
// request (see plangate.Card's What/Why trust split).
//
// Deliberately not "smart": no pluralisation, no prettifying of the resource
// type. A description that guesses at English produces confident nonsense on
// resource types it has never seen, and a card is the last place to be
// approximately right. The action is capitalised, the resource is quoted
// verbatim, and the tools that exercise it are named.
func (d Descriptor) Describe() string {
	if d.Permission != "" && d.ResourceType != "" {
		out := capitalizeFirst(d.Permission) + " " + d.ResourceType
		if tools := d.toolNames(); len(tools) > 0 {
			out += " (" + strings.Join(tools, ", ") + ")"
		}
		return out
	}
	if d.ToolName != "" {
		return d.ToolName
	}
	// Nothing resolved. The raw handle is a poor line on a card; a blank one is
	// a worse one, because it reads as "this grants nothing".
	return d.Handle.String()
}

// DescribeWithTitle prefers a declared human phrase, falling back to a
// DETOKENIZED handle rather than to Describe()'s technical form.
//
// The fallback splits on underscores and capitalizes the verb — `Push git
// repo` from `push` + `git_repo`. That is not the prettifying Describe()
// refuses to do — no plural guessed, no resource type reworded. It only stops
// showing an underscore to a human.
//
// No indefinite article. An earlier version inserted "a" ("push a git repo"),
// which reads fine on a consonant-initial resource type and wrong on a
// vowel-initial one ("push a email"). Picking a/an correctly needs a
// dictionary, not a vowel check — "an hour" and "a SQL" both defeat the naive
// rule — so this drops the article entirely rather than trading one guess-wrong
// case for a smaller one. Describe() itself is unchanged and still serves
// callers that want the tool-anchored form.
func (d Descriptor) DescribeWithTitle(title string) string {
	if title != "" {
		return title
	}
	if d.Permission == "" || d.ResourceType == "" {
		return d.Describe()
	}
	return capitalizeFirst(d.Permission) + " " + strings.ReplaceAll(d.ResourceType, "_", " ")
}

// toolNames lists the tools that produce this handle, deduplicated and in
// first-seen order — Via is already deterministically ordered by Enumerate, so
// re-sorting here would only decouple the card from the surface.
func (d Descriptor) toolNames() []string {
	if len(d.Via) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(d.Via))
	out := make([]string, 0, len(d.Via))
	for _, v := range d.Via {
		if v.Tool == "" {
			continue
		}
		if _, dup := seen[v.Tool]; dup {
			continue
		}
		seen[v.Tool] = struct{}{}
		out = append(out, v.Tool)
	}
	return out
}

func capitalizeFirst(s string) string {
	if s == "" {
		return s
	}
	r := []rune(s)
	r[0] = unicode.ToUpper(r[0])
	return string(r)
}
