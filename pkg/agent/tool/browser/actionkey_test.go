package browser

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// baseFacts is the "base" row of ~/Projects/laya/spike_gate_keys.py's VARIANTS
// table (lines 24-29): a click on a "Confirm seat 14C" button inside a form
// whose action is "/trips/seats/update", on https://www.aa.com/trips/AA1234/seats.
// Host carries the "www." prefix because that is what Session.CurrentHost
// actually returns (browserpolicy.CanonicalHost lowercases but does not strip
// "www." — ActionKey itself does that, per ruling E-R1), matching the real
// pipeline's ActionFacts rather than a pre-stripped fixture.
func baseFacts() ActionFacts {
	return ActionFacts{
		Op:         "click",
		Role:       "button",
		Text:       "confirm seat 14c",
		FormAction: "/trips/seats/update",
		Host:       "www.aa.com",
		PageURL:    "https://www.aa.com/trips/AA1234/seats",
	}
}

// TestActionKeySameActionVariantsCollapseToOneKey transcribes the spike's
// VARIANTS table (lines 24-56): five ways the SAME logical action — the
// "Confirm seat 14C" click — might appear in the DOM across reloads: hashed
// utility/react classes, csrf+session tokens in the form action plus tracking
// query params in the page URL, whitespace/case drift in the visible label,
// and a fully re-rendered wrapper chain where the name comes from aria-label
// rather than text content.
//
// E3's ActionFacts has already dropped every volatile part that distinguishes
// these in the raw DOM — hashed classes never become a Role/Text/FormAction
// field at all, FormAction is URL-path-only (so the csrf/session query string
// is gone before it reaches ActionFacts), and Text is already
// trimmed/collapsed/lowercased — so each variant here is constructed with the
// STABLE facts it reduces to. The one exception is PageURL: that field is NOT
// path-only (only FormAction is normalized at the ActionFacts layer), so the
// csrf-variant's PageURL is given WITH its tracking query string, to prove
// ActionKey's own page-path extraction (not just E3's) drops it too.
func TestActionKeySameActionVariantsCollapseToOneKey(t *testing.T) {
	cases := []struct {
		name  string
		facts ActionFacts
	}{
		{name: "base", facts: baseFacts()},
		{name: "hashed utility classes + react noise", facts: baseFacts()},
		{
			name: "csrf/session tokens in action url + query params in page url",
			facts: func() ActionFacts {
				f := baseFacts()
				f.PageURL = "https://www.aa.com/trips/AA1234/seats?ref=email&cid=1123"
				return f
			}(),
		},
		{name: "whitespace/case drift in text (already normalized by E3)", facts: baseFacts()},
		{name: "re-rendered wrapper chain, name from aria-label", facts: baseFacts()},
	}

	keys := make(map[string]struct{}, len(cases))
	var baseKey string
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			k := ActionKey(tc.facts)
			require.NotEmpty(t, k)
			keys[k] = struct{}{}
			if tc.name == "base" {
				baseKey = k
			}
		})
	}

	assert.Len(t, keys, 1, "all five same-action variants must collapse to exactly one ActionKey; got %v", keys)
	assert.True(t, strings.HasPrefix(baseKey, "aa.com/click/confirm-seat-14c"),
		"readable prefix must lead with <domain>/<op>/<kebab-name>: got %q", baseKey)
}

// TestActionKeyNearMissesAreDistinct transcribes the spike's NEAR_MISSES table
// (lines 59-83): four actions that are each one stable fact away from the base
// "confirm seat 14C" click and MUST NOT collide with it, or with each other.
func TestActionKeyNearMissesAreDistinct(t *testing.T) {
	base := ActionKey(baseFacts())

	cases := []struct {
		name  string
		facts ActionFacts
	}{
		{
			// Different seat number: the normalized name differs.
			name: "different seat",
			facts: func() ActionFacts {
				f := baseFacts()
				f.Text = "confirm seat 15a"
				return f
			}(),
		},
		{
			// Different trip: same domain/op/role/name/action-path, but the page
			// path differs — this is the case that proves page_path must be in the
			// HASH even though it is not part of the readable prefix (the prefix
			// alone would be identical to the base's).
			name: "different trip",
			facts: func() ActionFacts {
				f := baseFacts()
				f.PageURL = "https://www.aa.com/trips/BA9981/seats"
				return f
			}(),
		},
		{
			// Different action on the same page: both name and action path differ.
			name: "different action same page",
			facts: func() ActionFacts {
				f := baseFacts()
				f.Text = "cancel trip"
				f.FormAction = "/trips/cancel"
				return f
			}(),
		},
		{
			// Same visible label, different site: domain, action path, and page
			// path all differ even though the name matches the base's exactly.
			name: "same label different site",
			facts: func() ActionFacts {
				f := baseFacts()
				f.Host = "www.united.com"
				f.FormAction = "/booking/seats/confirm"
				f.PageURL = "https://www.united.com/trips/UA77/seats"
				return f
			}(),
		},
	}

	seen := map[string]string{"base": base}
	for _, tc := range cases {
		t.Run(tc.name+": distinct from base and every other near-miss", func(t *testing.T) {
			k := ActionKey(tc.facts)
			require.NotEmpty(t, k)
			assert.NotEqual(t, base, k, "near-miss %q must not collide with the base key %q", tc.name, base)
			for otherName, otherKey := range seen {
				if otherName == tc.name {
					continue
				}
				assert.NotEqual(t, otherKey, k, "near-miss %q must not collide with %q", tc.name, otherName)
			}
			seen[tc.name] = k
		})
	}

	assert.Len(t, seen, len(cases)+1, "base plus every near-miss must all be pairwise distinct")
}

// TestActionKeyIsDeterministic proves ActionKey is a pure function: calling it
// twice on byte-identical ActionFacts yields byte-identical keys, with no
// wall-clock or randomness involved.
func TestActionKeyIsDeterministic(t *testing.T) {
	f := baseFacts()
	assert.Equal(t, ActionKey(f), ActionKey(f))
}
