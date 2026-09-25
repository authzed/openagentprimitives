package userprofilegate_test

import (
	"context"
	"testing"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/authzed/openagentprimitives/pkg/agent/runner/userprofilegate"
	"github.com/authzed/openagentprimitives/pkg/agent/userprofile"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

// profileKind implements channelkinds.UserProfileProvider; plainKind does not.
// fields overrides what ProfileFields() reports; nil defaults to
// userprofile.AllFields() so existing callers that don't care about the
// kind's supported set keep working unchanged.
type profileKind struct {
	channelkinds.Kind
	called int
	fields []userprofile.Field
}

func (p *profileKind) Name() string { return "profile-kind" }

func (p *profileKind) ProfileFields() []userprofile.Field {
	if p.fields != nil {
		return p.fields
	}
	return userprofile.AllFields()
}
func (p *profileKind) FetchProfileByEmail(context.Context, channelkinds.LookupDeps, string) (userprofile.Profile, error) {
	p.called++
	return userprofile.Profile{Title: "Director of Support"}, nil
}

type plainKind struct{ channelkinds.Kind }

func classWith(t *testing.T, raw string) *spiceboxv1alpha1.AgentClass {
	t.Helper()
	ac := &spiceboxv1alpha1.AgentClass{ObjectMeta: metav1.ObjectMeta{Name: "demo-agent", Namespace: "ns"}}
	if raw != "" {
		ac.Spec.Capabilities = map[string]apiextensionsv1.JSON{"user_profile": {Raw: []byte(raw)}}
	}
	return ac
}

func TestOffer(t *testing.T) {
	cases := []struct {
		name       string
		raw        string
		kind       channelkinds.Kind
		wantOK     bool
		wantFields []userprofile.Field
	}{
		{name: "capability absent: declines", raw: "", kind: &profileKind{}, wantOK: false},
		{name: "capability disabled: declines", raw: `{"enabled":false}`, kind: &profileKind{}, wantOK: false},
		{name: "malformed config: declines", raw: `{`, kind: &profileKind{}, wantOK: false},
		{name: "unknown field name: declines", raw: `{"fields":["salary"]}`, kind: &profileKind{}, wantOK: false},
		{name: "kind implements no provider: declines", raw: `{}`, kind: &plainKind{}, wantOK: false},
		{name: "nil kind: declines", raw: `{}`, kind: nil, wantOK: false},
		{
			name: "granted + supported: offers with the default field set",
			raw:  `{}`, kind: &profileKind{}, wantOK: true, wantFields: userprofile.DefaultFields(),
		},
		{
			name: "granted with explicit fields: offers those fields",
			raw:  `{"fields":["title"]}`, kind: &profileKind{}, wantOK: true,
			wantFields: []userprofile.Field{userprofile.FieldTitle},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fetch, fields, ok := userprofilegate.Offer(logr.Discard(), classWith(t, tc.raw), tc.kind, nil)
			assert.Equal(t, tc.wantOK, ok)
			if !tc.wantOK {
				assert.Nil(t, fetch, "a declined offer must hand back NO fetcher, not an inert one")
				assert.Empty(t, fields)
				return
			}
			require.NotNil(t, fetch)
			assert.Equal(t, tc.wantFields, fields)

			got, err := fetch(context.Background(), "dana@example.com")
			require.NoError(t, err)
			assert.Equal(t, "Director of Support", got.Title)
		})
	}
}

// TestDeclinedOfferNeverTouchesTheKind pins the promise: no grant means no API
// call, not a call whose result is discarded.
func TestDeclinedOfferNeverTouchesTheKind(t *testing.T) {
	k := &profileKind{}
	_, _, ok := userprofilegate.Offer(logr.Discard(), classWith(t, ""), k, nil)
	require.False(t, ok)
	assert.Zero(t, k.called)
}

// TestOfferLogsMalformedConfig pins the AGENTS.md "never silently drop an
// error" rule: ActiveWithConfig's parse error must actually reach the log,
// not just cause a decline. Mirrors
// capability.TestAssembleMalformedEnabledLogsAndInactivates's funcr pattern.
func TestOfferLogsMalformedConfig(t *testing.T) {
	var logged []string
	lg := funcr.New(func(prefix, args string) { logged = append(logged, args) }, funcr.Options{})

	fetch, fields, ok := userprofilegate.Offer(lg, classWith(t, `{`), &profileKind{}, nil)
	require.False(t, ok)
	assert.Nil(t, fetch)
	assert.Empty(t, fields)
	require.NotEmpty(t, logged, "a malformed user_profile config must emit a log line, not be silently dropped")
	assert.Contains(t, logged[0], "class", "log line should identify the AgentClass")
	assert.Contains(t, logged[0], "demo-agent", "log line should name the class")
}

// TestOfferDoesNotLogUngrantedCapability pins the split the review called
// for: an ungranted capability is the ordinary, expected case and must not
// produce log noise on every session — only a config that failed to parse
// does.
func TestOfferDoesNotLogUngrantedCapability(t *testing.T) {
	var logged []string
	lg := funcr.New(func(prefix, args string) { logged = append(logged, args) }, funcr.Options{})

	_, _, ok := userprofilegate.Offer(lg, classWith(t, ""), &profileKind{}, nil)
	require.False(t, ok)
	assert.Empty(t, logged, "an ungranted capability is normal and must not log")
}

// TestOfferIntersectsFieldsWithKindSupport pins Step 9: the operator's
// configured allowlist is narrowed to what the bound kind can actually
// populate, in the OPERATOR's order — never the kind's.
func TestOfferIntersectsFieldsWithKindSupport(t *testing.T) {
	cases := []struct {
		name       string
		kindFields []userprofile.Field
		wantFields []userprofile.Field
	}{
		{
			name:       "kind supports a superset: every configured field survives, in operator order",
			kindFields: userprofile.AllFields(),
			wantFields: []userprofile.Field{userprofile.FieldPronouns, userprofile.FieldTitle},
		},
		{
			name:       "kind supports a strict subset: only the overlap survives",
			kindFields: []userprofile.Field{userprofile.FieldPronouns, userprofile.FieldTimezone},
			wantFields: []userprofile.Field{userprofile.FieldPronouns},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			k := &profileKind{fields: tc.kindFields}
			fetch, fields, ok := userprofilegate.Offer(
				logr.Discard(), classWith(t, `{"fields":["pronouns","title"]}`), k, nil)
			require.True(t, ok)
			require.NotNil(t, fetch)
			assert.Equal(t, tc.wantFields, fields)
		})
	}
}

// TestOfferDeclinesWhenKindSupportsNoConfiguredField pins the disjoint half:
// a kind that can populate NONE of the operator's configured fields must
// decline outright (nil fetcher, no offer) rather than being offered for a
// fetch that would only ever render an empty block — and, per AGENTS.md, the
// decline must be logged since it is a real (if quiet) config mismatch an
// operator can act on.
func TestOfferDeclinesWhenKindSupportsNoConfiguredField(t *testing.T) {
	var logged []string
	lg := funcr.New(func(prefix, args string) { logged = append(logged, args) }, funcr.Options{})

	k := &profileKind{fields: []userprofile.Field{userprofile.FieldTimezone, userprofile.FieldLocale}}
	fetch, fields, ok := userprofilegate.Offer(
		lg, classWith(t, `{"fields":["title","pronouns"]}`), k, nil)

	require.False(t, ok)
	assert.Nil(t, fetch, "a disjoint field set must hand back NO fetcher, not one that renders empty blocks")
	assert.Empty(t, fields)
	assert.Zero(t, k.called, "the kind must never be called on a declined offer")
	require.NotEmpty(t, logged, "a disjoint field set is a config mismatch and must be logged")
	assert.Contains(t, logged[0], "demo-agent")
	assert.Contains(t, logged[0], "profile-kind")
}
