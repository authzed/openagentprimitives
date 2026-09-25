// pkg/controllers/relationshipsource/scopeerrors_test.go
package relationshipsource

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/conditions"
	"github.com/authzed/openagentprimitives/pkg/platform/relsync"
)

// scopeErr is a one-line relsync.ScopeError fixture.
func scopeErr(scope, msg string) relsync.ScopeError {
	return relsync.ScopeError{Scope: relsync.ScopeID(scope), Err: errors.New(msg)}
}

// forbiddenPass reproduces the failure this feature exists for: enumeration
// succeeded, every enumerated scope was visited, and every per-scope fetch
// answered 403. Ready is True/Synced throughout — that is by design — so the
// scope errors are the only thing that says anything is wrong.
func forbiddenPass(scopes int) relsync.PassResult {
	res := relsync.PassResult{Processed: scopes, EnumComplete: true, CycleComplete: true}
	for i := range scopes {
		res.ScopeErrors = append(res.ScopeErrors, scopeErr(
			fmt.Sprintf("repo-%03d", i),
			fmt.Sprintf("github: GET https://api.github.com/repos/demo-org/repo-%03d/teams: unexpected status 403", i),
		))
	}
	return res
}

func newStatusFixture(t *testing.T) (*spiceboxv1alpha1.RelationshipSource, *spiceboxv1alpha1.RelationshipSourceStatus) {
	t.Helper()
	obj := &spiceboxv1alpha1.RelationshipSource{}
	obj.Generation = 1
	return obj, &spiceboxv1alpha1.RelationshipSourceStatus{}
}

// partialCondition returns the PartialFailure condition, failing the test when
// it is absent — an absent condition is the pre-feature behaviour, so a helper
// that returned nil would let every assertion below pass vacuously.
func partialCondition(t *testing.T, st *spiceboxv1alpha1.RelationshipSourceStatus) *metav1.Condition {
	t.Helper()
	c := conditions.Find(st.Conditions, spiceboxv1alpha1.RelationshipSourceConditionPartialFailure)
	require.NotNil(t, c, "the PartialFailure condition must be set on every completed pass")
	return c
}

// The motivating failure, asserted end to end at the status layer: an entire
// arm of the sync failing must be VISIBLE, while Ready keeps its meaning.
func TestApplySyncResult_PartialFailureIsVisibleWhileReadyStaysTrue(t *testing.T) {
	obj, st := newStatusFixture(t)

	require.True(t, applySyncResult(obj, st, true, forbiddenPass(156), false, false))

	ready := conditions.Find(st.Conditions, spiceboxv1alpha1.RelationshipSourceConditionReady)
	require.NotNil(t, ready)
	assert.Equal(t, metav1.ConditionTrue, ready.Status,
		"non-fatal stays non-fatal: Ready's meaning is unchanged by this feature")
	assert.Equal(t, spiceboxv1alpha1.ReasonRelationshipSourceSynced, ready.Reason)

	partial := partialCondition(t, st)
	assert.Equal(t, metav1.ConditionTrue, partial.Status)
	assert.Equal(t, spiceboxv1alpha1.ReasonRelationshipSourceScopeErrors, partial.Reason)
	assert.Contains(t, partial.Message, "156 scope(s) failed")

	require.NotNil(t, st.Sync.LastPass)
	assert.Equal(t, int32(156), st.Sync.LastPass.ScopeErrors,
		"the COUNT is the whole failure, not the sample length")
	assert.Equal(t, int32(156), st.Sync.LastPass.ScopesProcessed,
		"precondition: this is the shape that used to read as healthy")
	assert.Len(t, st.Sync.LastPass.ScopeErrorSamples, maxScopeErrorSamples,
		"156 errors must not become 156 status entries")
}

// A clean pass must CLEAR the condition, not merely leave it unset — a source
// that fails once and is then repaired has to stop reporting degraded.
func TestApplySyncResult_CleanPassClearsPartialFailure(t *testing.T) {
	obj, st := newStatusFixture(t)

	require.True(t, applySyncResult(obj, st, true, forbiddenPass(3), false, false))
	require.Equal(t, metav1.ConditionTrue, partialCondition(t, st).Status, "precondition: degraded")

	clean := relsync.PassResult{Processed: 3, EnumComplete: true, CycleComplete: true}
	assert.True(t, applySyncResult(obj, st, false, clean, false, false),
		"clearing a partial failure is news and must be persisted")

	partial := partialCondition(t, st)
	assert.Equal(t, metav1.ConditionFalse, partial.Status)
	assert.Equal(t, spiceboxv1alpha1.ReasonRelationshipSourceAllScopesSynced, partial.Reason)
	assert.Empty(t, partial.Message)
	assert.Zero(t, st.Sync.LastPass.ScopeErrors)
	assert.Empty(t, st.Sync.LastPass.ScopeErrorSamples,
		"a repaired source must not keep rendering the errors it no longer has")
}

// THE loop guard, for the scope-error half. The controller's self-watch has no
// predicate, so any field that differs on two identical passes makes every
// reconcile a status write that re-enqueues itself and re-runs the upstream
// Pass — forever, against an upstream that is already failing.
//
// Three shapes of instability, each of which has moved status in some earlier
// draft of this code, are asserted separately so a regression names itself.
func TestApplySyncResult_IdenticalFailingPassesDoNotChurn(t *testing.T) {
	t.Run("byte-identical failing passes: second must not move status", func(t *testing.T) {
		obj, st := newStatusFixture(t)
		res := forbiddenPass(12)

		require.True(t, applySyncResult(obj, st, true, res, false, false))
		require.NotNil(t, st.Sync.LastPass.FinishedAt)
		firstStamp := *st.Sync.LastPass.FinishedAt
		firstSamples := append([]spiceboxv1alpha1.RelationshipSourceScopeError(nil), st.Sync.LastPass.ScopeErrorSamples...)

		assert.False(t, applySyncResult(obj, st, false, res, false, false),
			"an identical failing pass must not move status")
		assert.Equal(t, firstStamp, *st.Sync.LastPass.FinishedAt, "FinishedAt must be carried, not restamped")
		assert.Equal(t, firstSamples, st.Sync.LastPass.ScopeErrorSamples)
	})

	t.Run("same failures in a different order: sorting must absorb it", func(t *testing.T) {
		obj, st := newStatusFixture(t)
		res := forbiddenPass(12)
		require.True(t, applySyncResult(obj, st, true, res, false, false))

		// relsync.Pass appends in enumeration order; a concurrent fetch, a
		// map-ordered upstream, or a future reordering inside Pass would hand
		// the same failures back in a different sequence. Reversal stands in
		// for all of them.
		shuffled := res
		shuffled.ScopeErrors = make([]relsync.ScopeError, len(res.ScopeErrors))
		for i, se := range res.ScopeErrors {
			shuffled.ScopeErrors[len(res.ScopeErrors)-1-i] = se
		}

		assert.False(t, applySyncResult(obj, st, false, shuffled, false, false),
			"the SAME failures in a different order are not new information")
	})

	t.Run("error text that re-renders a live duration: must not move status", func(t *testing.T) {
		obj, st := newStatusFixture(t)

		// The real shape: pkg/channels/channelkinds/github's rate-limit error
		// renders its REMAINING backoff into Error(), from a time.Until
		// evaluated as the string is built. Two consecutive throttled passes
		// therefore produce different bytes for the identical fact.
		throttled := func(remaining time.Duration) relsync.PassResult {
			return relsync.PassResult{
				Processed: 2, EnumComplete: true, CycleComplete: true,
				ScopeErrors: []relsync.ScopeError{scopeErr("repo-000", fmt.Sprintf(
					"github: GET https://api.github.com/repos/demo-org/repo-000/teams: rate limited (status 403); retry after %s", remaining))},
			}
		}

		require.True(t, applySyncResult(obj, st, true, throttled(59*time.Minute+12*time.Second), false, false))
		first := st.Sync.LastPass.ScopeErrorSamples[0].Message

		assert.False(t, applySyncResult(obj, st, false, throttled(58*time.Minute+41*time.Second), false, false),
			"a re-rendered backoff is not a new observation; persisting it re-enqueues a pass against an upstream that just said to wait")
		assert.Equal(t, first, st.Sync.LastPass.ScopeErrorSamples[0].Message,
			"the stored sample is preserved, not refreshed, while the key is unchanged")
	})
}

// The complement of the loop guard: real news must still land, or the guard
// has bought stability by going blind. Each case changes exactly one input.
func TestApplySyncResult_RealChangesInScopeErrorsStillMoveStatus(t *testing.T) {
	cases := []struct {
		name string
		next relsync.PassResult
	}{
		{"a different NUMBER of scopes failing", forbiddenPass(7)},
		{"the same number of DIFFERENT scopes failing", relsync.PassResult{
			Processed: 12, EnumComplete: true, CycleComplete: true,
			ScopeErrors: []relsync.ScopeError{
				scopeErr("other-a", "github: GET https://api.github.com/repos/demo-org/other-a/teams: unexpected status 403"),
				scopeErr("other-b", "github: GET https://api.github.com/repos/demo-org/other-b/teams: unexpected status 403"),
				scopeErr("other-c", "github: GET https://api.github.com/repos/demo-org/other-c/teams: unexpected status 403"),
			},
		}},
		{"the failures clearing entirely", relsync.PassResult{Processed: 12, EnumComplete: true, CycleComplete: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name+" moves status", func(t *testing.T) {
			obj, st := newStatusFixture(t)
			require.True(t, applySyncResult(obj, st, true, forbiddenPass(3), false, false))
			assert.True(t, applySyncResult(obj, st, false, tc.next, false, false),
				"this is a change an operator would call news")
		})
	}
}

// A pass that never COMPLETED must leave the condition describing the last one
// that did. requeue (the Ready=False paths) does not run applySyncResult at
// all, so the assertion here is on the contract that keeps them separable: the
// condition is only ever written from a PassResult.
func TestApplySyncResult_PartialFailureIsOnlyWrittenFromACompletedPass(t *testing.T) {
	obj, st := newStatusFixture(t)
	require.True(t, applySyncResult(obj, st, true, forbiddenPass(4), false, false))
	before := *partialCondition(t, st)

	// requeue's path: conditions.SetFalse on Ready, nothing else touched.
	conditions.SetFalse(obj, &st.Conditions, spiceboxv1alpha1.RelationshipSourceConditionReady,
		spiceboxv1alpha1.ReasonRelationshipSourceAuthResolveFailed, "token revoked")

	after := *partialCondition(t, st)
	assert.Equal(t, before, after,
		"a pass that never ran must not silently re-verdict the last one that did")
}

func TestBuildScopeErrorSamples_SortsDeterministicallyAndCaps(t *testing.T) {
	errs := []relsync.ScopeError{
		scopeErr("zeta", "boom"),
		scopeErr("alpha", "second"),
		scopeErr("alpha", "first"),
		scopeErr("", "enumeration failed"),
		scopeErr("mid", "m"),
		scopeErr("nnn", "n"),
		scopeErr("ooo", "o"),
	}
	got := buildScopeErrorSamples(errs)

	require.Len(t, got, maxScopeErrorSamples)
	// Sorted by (scope, message); the scope-less entry (enumeration/reap-scan
	// failure — relsync.ScopeError's own doc) sorts first on the empty id.
	assert.Equal(t, "", got[0].Scope)
	assert.Equal(t, "alpha", got[1].Scope)
	assert.Equal(t, "first", got[1].Message, "ties on scope break on message, not on input order")
	assert.Equal(t, "alpha", got[2].Scope)
	assert.Equal(t, "second", got[2].Message)
	assert.Equal(t, "mid", got[3].Scope)
	assert.Equal(t, "nnn", got[4].Scope)

	assert.Nil(t, buildScopeErrorSamples(nil), "no errors means no samples, not an empty non-nil slice")
}

// Scope-error text is quoted from an upstream API, and a URL is where a
// credential rides. Status is readable by anything with `get
// relationshipsources`, and the admin console renders it — so no query
// parameter and no userinfo may survive into the sample.
func TestScrubScopeErrorMessage(t *testing.T) {
	cases := []struct {
		name        string
		in          string
		wantAbsent  []string
		wantPresent []string
	}{
		{
			name:        "query parameters are stripped, path and sentence punctuation kept",
			in:          "github: GET https://api.github.com/repos/demo-org/widget/teams?access_token=ghp_notarealtoken&page=2: unexpected status 403",
			wantAbsent:  []string{"ghp_notarealtoken", "access_token", "page=2"},
			wantPresent: []string{"https://api.github.com/repos/demo-org/widget/teams?<redacted>", ": unexpected status 403"},
		},
		{
			name:        "userinfo credentials are stripped",
			in:          "scim: GET https://svc:s3cr3t-value@directory.example.internal/Groups failed",
			wantAbsent:  []string{"s3cr3t-value", "svc:s3cr3t"},
			wantPresent: []string{"https://<redacted>@directory.example.internal/Groups"},
		},
		{
			name:        "a signed URL's signature does not survive",
			in:          "onepassword: GET https://vault.example.com/v1/groups?sig=abc123def456&expires=99 -> 401",
			wantAbsent:  []string{"abc123def456", "sig=", "expires=99"},
			wantPresent: []string{"?<redacted>"},
		},
		{
			// The shortest route into this function, and the one an
			// http(s)-anchored pattern missed entirely: spec.baseURL is
			// tenant-writable and need not be a well-formed absolute URL, and
			// credhost.Check's "names no host" branch quotes whatever was
			// written, in full.
			name:        "a scheme-less destination's query is stripped too",
			in:          `relationshipsource ns/src: credential "cred": destination "/Groups?access_token=notarealtoken" names no host`,
			wantAbsent:  []string{"notarealtoken", "access_token"},
			wantPresent: []string{`destination "/Groups?<redacted>" names no host`},
		},
		{
			// The "=" requirement is what keeps the unanchored pattern off
			// ordinary prose. A question mark in a sentence is not a query.
			name:        "prose punctuation is not mistaken for a query",
			in:          "upstream returned 500; is the directory reachable? retrying next pass",
			wantAbsent:  []string{"<redacted>"},
			wantPresent: []string{"is the directory reachable? retrying next pass"},
		},
		{
			// The OAuth implicit flow returns its token in the fragment.
			name:        "a fragment-borne token does not survive",
			in:          "oauth: callback https://directory.example.internal/cb#access_token=notarealtoken&token_type=bearer rejected",
			wantAbsent:  []string{"notarealtoken", "access_token", "token_type"},
			wantPresent: []string{"https://directory.example.internal/cb#<redacted>", "rejected"},
		},
		{
			name:        "multi-line upstream bodies are collapsed to one line",
			in:          "slack: response\n  <html>\n\t<body>error</body>\n</html>",
			wantAbsent:  []string{"\n", "\t"},
			wantPresent: []string{"slack: response <html> <body>error</body> </html>"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := scrubScopeErrorMessage(tc.in)
			for _, s := range tc.wantAbsent {
				assert.NotContains(t, got, s, "must not survive scrubbing: %q", s)
			}
			for _, s := range tc.wantPresent {
				assert.Contains(t, got, s)
			}
		})
	}

	t.Run("an oversized message is truncated on a rune boundary", func(t *testing.T) {
		got := scrubScopeErrorMessage(strings.Repeat("é", 4000))
		assert.LessOrEqual(t, len([]rune(got)), maxScopeErrorMessageLen, "got %d runes", len([]rune(got)))
		assert.True(t, utf8.ValidString(got), "a byte-wise cut lands mid-codepoint and the API server rejects the whole write")
	})
}

// Kubernetes measures maxLength in RUNES and rejects an over-long status write
// WHOLE. That rejection is not a dropped field: Status().Update errors,
// Reconcile returns it, and controller-runtime retries with backoff, each retry
// re-running relsync.Pass against the upstream — the runaway this file exists
// to prevent, reached from the other end. An earlier cut clipped to exactly the
// bound and THEN appended the ellipsis, emitting one rune over.
func TestScopeErrorSamplesFitTheCRDBounds(t *testing.T) {
	// Every emitted value must be at or under the mirrored bound, and each
	// field is probed with an input far past it so truncation is what is being
	// measured rather than the input's own length.
	samples := buildScopeErrorSamples([]relsync.ScopeError{
		scopeErr(strings.Repeat("s", 4000), strings.Repeat("m", 4000)),
		scopeErr(strings.Repeat("é", 4000), strings.Repeat("é", 4000)),
	})
	require.Len(t, samples, 2)
	for _, s := range samples {
		assert.LessOrEqual(t, len([]rune(s.Scope)), crdScopeMaxLength,
			"scope exceeds the +kubebuilder:validation:MaxLength on RelationshipSourceScopeError.Scope")
		assert.LessOrEqual(t, len([]rune(s.Message)), crdMessageMaxLength,
			"message exceeds the +kubebuilder:validation:MaxLength on RelationshipSourceScopeError.Message")
		assert.True(t, utf8.ValidString(s.Scope) && utf8.ValidString(s.Message))
	}

	// The marker counts toward the budget — that IS the contract, and the one
	// an off-by-one breaks.
	assert.Equal(t, 4, len([]rune(truncateRunes("abcdef", 4))))
	assert.Equal(t, "abc…", truncateRunes("abcdef", 4))
	assert.Equal(t, "abcd", truncateRunes("abcd", 4), "a string at the bound is returned untouched")
	assert.Empty(t, truncateRunes("abcd", 0), "a zero budget has no room even for the marker")
}

// enumerationFailedMessage is the Ready=False text for a pass that enumerated
// nothing, and it is the OTHER place a rate limit's recomputed backoff could
// churn status — requeue's DeepEqual moves on a changed condition message just
// as applySyncResult's does on a changed sample.
func TestEnumerationFailedMessage(t *testing.T) {
	t.Run("no errors: the bare statement", func(t *testing.T) {
		assert.Equal(t, "enumeration produced no scopes and nothing was synced this pass",
			enumerationFailedMessage(nil))
	})

	t.Run("a deterministic failure is quoted, scrubbed", func(t *testing.T) {
		got := enumerationFailedMessage([]relsync.ScopeError{scopeErr("",
			"relsync: enumerate scopes: github: GET https://api.github.com/orgs/demo-org/repos?access_token=ghp_notarealtoken: unexpected status 401")})
		assert.Contains(t, got, "unexpected status 401", "the diagnosis has to survive")
		assert.NotContains(t, got, "ghp_notarealtoken",
			"this message reaches status, the console, and a monitoring channel")
	})

	t.Run("a rate limit is reported structurally, with no rendered backoff", func(t *testing.T) {
		// Two passes against one upstream reset timestamp build two different
		// durations; quoting either moves requeue's DeepEqual, the
		// predicate-less self-watch re-enqueues, and the source re-polls an
		// upstream that just asked it to wait.
		first := enumerationFailedMessage([]relsync.ScopeError{{
			Err: fmt.Errorf("relsync: enumerate scopes: %w", &fakeRateLimitErr{after: 59*time.Minute + 12*time.Second}),
		}})
		second := enumerationFailedMessage([]relsync.ScopeError{{
			Err: fmt.Errorf("relsync: enumerate scopes: %w", &fakeRateLimitErr{after: 58*time.Minute + 41*time.Second}),
		}})

		assert.Equal(t, first, second, "two throttled passes must produce identical condition text")
		assert.Contains(t, first, "rate limited")
		assert.NotContains(t, first, "59m12s")
		assert.NotContains(t, second, "58m41s")
	})
}

// fakeRateLimitErr carries a backoff the way each kind's own rate-limit error
// does — through the RetryAfter interface the controller already matches with
// errors.As, never through message text.
type fakeRateLimitErr struct{ after time.Duration }

func (e *fakeRateLimitErr) Error() string {
	return fmt.Sprintf("upstream: rate limited (status 403); retry after %s", e.after)
}
func (e *fakeRateLimitErr) RetryAfter() time.Duration { return e.after }

// A leaked credential must not reach status through the SAMPLE path either —
// scrubbing is asserted above at the function, and here at the field that
// actually gets persisted, because that is the boundary that matters.
func TestApplySyncResult_SampledMessagesCarryNoCredential(t *testing.T) {
	obj, st := newStatusFixture(t)
	res := relsync.PassResult{
		Processed: 1, EnumComplete: true, CycleComplete: true,
		ScopeErrors: []relsync.ScopeError{scopeErr("widget",
			"github: GET https://api.github.com/repos/demo-org/widget/teams?access_token=ghp_notarealtoken: unexpected status 403")},
	}
	require.True(t, applySyncResult(obj, st, true, res, false, false))

	require.Len(t, st.Sync.LastPass.ScopeErrorSamples, 1)
	assert.NotContains(t, st.Sync.LastPass.ScopeErrorSamples[0].Message, "ghp_notarealtoken")
	assert.NotContains(t, partialCondition(t, st).Message, "ghp_notarealtoken",
		"the condition message quotes a sample, so it inherits the sample's scrubbing")
}
