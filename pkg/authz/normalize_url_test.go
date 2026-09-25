package authz_test

import (
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz"
)

func norm(t *testing.T, s string) string {
	t.Helper()
	got, err := authz.ApplyTransforms(s, []string{"normalize_url"})
	require.NoError(t, err)
	return got
}

// TestNormalizeURL_DistinctTargetsNeverCollide is the SECURITY half, and the
// one that decides whether the transform is safe at all.
//
// A resource id is what an approval is granted against, so two distinct targets
// mapping to one id means approving the first silently authorises the second.
// Every pair here is a spelling an attacker would reach for; each must survive
// normalisation as two different ids.
func TestNormalizeURL_DistinctTargetsNeverCollide(t *testing.T) {
	pairs := []struct {
		name string
		a, b string
	}{
		{
			// The classic. A naive implementation reads the host as evil.com,
			// or drops userinfo entirely and lands on good.com.
			name: "userinfo host confusion",
			a:    "http://evil.com@good.com/", b: "http://good.com/",
		},
		{
			name: "userinfo differing only in case (userinfo IS case-sensitive)",
			a:    "http://Alice@h.example/", b: "http://alice@h.example/",
		},
		{
			name: "embedded credentials vs none",
			a:    "https://user:pw@h.example/x", b: "https://h.example/x",
		},
		{
			name: "encoded slash must not fold into a real separator",
			a:    "https://h.example/a%2F..%2Fb", b: "https://h.example/b",
		},
		{
			name: "trailing slash is a different resource",
			a:    "https://h.example/a/", b: "https://h.example/a",
		},
		{
			name: "empty interior segment is preserved",
			a:    "https://h.example//a", b: "https://h.example/a",
		},
		{
			name: "query is not stripped",
			a:    "https://h.example/x?token=1", b: "https://h.example/x",
		},
		{
			name: "differing query values stay distinct",
			a:    "https://h.example/x?a=1", b: "https://h.example/x?a=2",
		},
		{
			name: "IDN homoglyph host must not fold onto ASCII",
			a:    "https://exаmple.com/", b: "https://example.com/", // first has Cyrillic а
		},
		{
			name: "punycode and its unicode spelling are left as written",
			a:    "https://xn--80ak6aa92e.com/", b: "https://аррӏе.com/",
		},
		{
			name: "different scheme, same authority",
			a:    "https://h.example/x", b: "http://h.example/x",
		},
		{
			name: "non-default port is preserved",
			a:    "https://h.example:8443/x", b: "https://h.example/x",
		},
		{
			name: "port that is default for a DIFFERENT scheme is kept",
			a:    "https://h.example:80/x", b: "https://h.example/x",
		},
		{
			name: "percent-encoded dot segments are not resolved",
			a:    "https://h.example/a/%2e%2e/b", b: "https://h.example/b",
		},
		{
			name: "subdomain is not simplified away",
			a:    "https://a.h.example/x", b: "https://h.example/x",
		},
	}
	for _, tc := range pairs {
		t.Run(tc.name, func(t *testing.T) {
			na, nb := norm(t, tc.a), norm(t, tc.b)
			assert.NotEqual(t, na, nb,
				"distinct targets collapsed onto one id: %q and %q both became %q", tc.a, tc.b, na)
		})
	}
}

// TestNormalizeURL_EquivalentSpellingsConverge is the USABILITY half, asserted
// separately so a failure names which property broke. Each pair denotes the
// same target, so an approval for one must cover the other — otherwise the
// agent earns a spurious denial and an extra approval prompt.
func TestNormalizeURL_EquivalentSpellingsConverge(t *testing.T) {
	pairs := []struct {
		name string
		a, b string
	}{
		{name: "scheme case", a: "HTTP://h.example/x", b: "http://h.example/x"},
		{name: "host case", a: "https://H.Example/x", b: "https://h.example/x"},
		{name: "default http port", a: "http://h.example:80/x", b: "http://h.example/x"},
		{name: "default https port", a: "https://h.example:443/x", b: "https://h.example/x"},
		{name: "fragment is client-side only", a: "https://h.example/x#frag", b: "https://h.example/x"},
		{name: "dot segments resolve", a: "https://h.example/a/../b", b: "https://h.example/b"},
		{name: "single-dot segment resolves", a: "https://h.example/./b", b: "https://h.example/b"},
		{name: "traversal past root cannot escape", a: "https://h.example/../b", b: "https://h.example/b"},
		{name: "path case is NOT folded", a: "https://h.example/X", b: "https://h.example/X"},
	}
	for _, tc := range pairs {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, norm(t, tc.a), norm(t, tc.b),
				"the same target produced two ids, so one approval will not cover the other")
		})
	}
}

func TestNormalizeURL_UnparseableInputIsLeftAlone(t *testing.T) {
	// Returning the input unchanged is the fail-closed answer: it simply will
	// not match any grant. Rewriting a guess could match the WRONG one.
	for _, s := range []string{"", "not a url", "://missing-scheme", "mailto:a@b.example"} {
		assert.Equal(t, s, norm(t, s), "unparseable input must be returned unchanged")
	}
}

func TestNormalizeURL_IsIdempotent(t *testing.T) {
	for _, s := range []string{
		"HTTP://H.Example:80/a/../b?q=1#f",
		"https://user:pw@h.example//a/",
		"https://h.example/a%2Fb",
	} {
		once := norm(t, s)
		assert.Equal(t, once, norm(t, once), "normalising twice must equal normalising once")
	}
}

// FuzzNormalizeURL_HostChangesSurvive asserts the property that matters most and
// is hardest to enumerate by hand: whatever else normalisation does, it must not
// erase the host. If it did, any two URLs would become interchangeable.
func FuzzNormalizeURL_HostChangesSurvive(f *testing.F) {
	for _, s := range []string{
		"https://h.example/x", "http://a@b.example/p?q=1#f",
		"https://h.example:8443//a/../b", "https://[::1]/x",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		u, err := url.Parse(raw)
		if err != nil || u.Scheme == "" || u.Hostname() == "" {
			return // not an absolute URL; normalizeURL returns it unchanged
		}
		if strings.ContainsAny(u.Hostname(), "%[]") {
			return // exotic host encodings are out of this property's scope
		}
		// Prefixing the host yields a genuinely different authority.
		other := *u
		other.Host = "zz" + u.Host
		a, b := norm(t, u.String()), norm(t, other.String())
		if a == b {
			t.Fatalf("two different hosts normalised to the same id: %q and %q both became %q",
				u.String(), other.String(), a)
		}
	})
}

// FuzzNormalizeURL_Idempotent: a transform that is not idempotent makes the id
// depend on how many times it ran, which no caller reasons about.
func FuzzNormalizeURL_Idempotent(f *testing.F) {
	for _, s := range []string{"https://h.example/a/../b", "HTTP://X.Example:80/", "///"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		once := norm(t, raw)
		if twice := norm(t, once); once != twice {
			t.Fatalf("not idempotent: %q → %q → %q", raw, once, twice)
		}
	})
}

func TestTransformInjectivity_IsDeclaredPerTransform(t *testing.T) {
	cases := []struct {
		name      string
		injective bool
		why       string
	}{
		{name: "sha256", injective: true, why: "collision resistance is the standard this design already leans on"},
		{name: "normalize_url", injective: true, why: "asserted by the pair tables and fuzz targets above"},
		{name: "basename", injective: false, why: "/etc/passwd and /home/u/passwd become one object"},
		{name: "spicedb_object_id", injective: false, why: "illegal runes all become - and runs are deduped"},
		{name: "lowercase", injective: false, why: "folds case-distinct values together"},
		{name: "remove_spaces", injective: false, why: "'a b' and 'ab' become one object"},
		{name: "no_such_transform", injective: false, why: "unknown must mean unsafe, not assume-the-best"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.injective, authz.IsInjectiveTransform(tc.name), tc.why)
		})
	}
}

func TestNonInjectiveTransforms_NamesThemInOrder(t *testing.T) {
	assert.Equal(t, []string{"basename"},
		authz.NonInjectiveTransforms([]string{"normalize_url", "basename", "sha256"}))
	assert.Empty(t, authz.NonInjectiveTransforms([]string{"normalize_url", "sha256"}))
}
