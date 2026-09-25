package mcpserver_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	mcppin "github.com/authzed/openagentprimitives/pkg/authz/pinning/kinds/mcp"
	"github.com/authzed/openagentprimitives/pkg/controllers/mcpserver"
	"github.com/authzed/openagentprimitives/pkg/controllers/testfixtures"
	mcptest "github.com/authzed/openagentprimitives/pkg/tools/mcp/testing"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// defaultTools is a stable tool list used across pin tests.
var defaultTools = []mcptest.Tool{
	{Name: "search_issues", InputSchema: json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}}}`)},
}

// alteredTools is a different set used for drift tests.
var alteredTools = []mcptest.Tool{
	{Name: "search_issues", InputSchema: json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}}}`)},
	{Name: "create_issue", InputSchema: json.RawMessage(`{"type":"object","properties":{"title":{"type":"string"}}}`)},
}

func TestReconcile_PinCheck(t *testing.T) {
	// Compute the expected hash for defaultTools so we can assert against it.
	expectedHash := func() string {
		t.Helper()
		srv := mcptest.New(mcptest.Behavior{Tools: defaultTools})
		defer srv.Close()
		cr := newMCPServer(srv.URL)
		c := buildFakeClientFor(t, cr)
		got := reconcileLinear(t, c, &mcpserver.Reconciler{Client: c, HTTP: http.DefaultClient})
		require.NotNil(t, got.Status.Pin, "expected pin to be set on first reconcile")
		return got.Status.Pin.Digest
	}()

	alteredHash := func() string {
		t.Helper()
		srv := mcptest.New(mcptest.Behavior{Tools: alteredTools})
		defer srv.Close()
		cr := newMCPServer(srv.URL)
		c := buildFakeClientFor(t, cr)
		got := reconcileLinear(t, c, &mcpserver.Reconciler{Client: c, HTTP: http.DefaultClient})
		require.NotNil(t, got.Status.Pin)
		return got.Status.Pin.Digest
	}()

	require.NotEqual(t, expectedHash, alteredHash, "test setup: hashes must differ")
	require.True(t, strings.HasPrefix(expectedHash, "sha256:"), "digest must be sha256-prefixed")

	cases := []struct {
		name string
		// setup builds the fake server, returns its URL and a closer.
		setup func(t *testing.T) (url string, setAltTools func(), close func())
		// mutate the CR before the first reconcile (optional).
		mutateCR func(cr *spiceboxv1alpha1.MCPServer)
		// mutateBetween is called between the first and second reconcile (optional).
		mutateBetween func(cr *spiceboxv1alpha1.MCPServer, setAlt func())
		// two booleans: do a second reconcile?
		secondReconcile bool
		// check assertions on the final state.
		check func(t *testing.T, first, second spiceboxv1alpha1.MCPServer)
	}{
		{
			name: "TOFU first-observe: pin recorded + PinDrift=True/PinMatch",
			setup: func(t *testing.T) (string, func(), func()) {
				s := mcptest.New(mcptest.Behavior{Tools: defaultTools, ServerInfo: &mcptest.ServerInfo{Name: "linear", Version: "1.2.3"}})
				return s.URL, func() {}, s.Close
			},
			check: func(t *testing.T, first, _ spiceboxv1alpha1.MCPServer) {
				require.NotNil(t, first.Status.Pin, "Pin must be set after TOFU")
				assert.Equal(t, mcppin.KindName, first.Status.Pin.Kind)
				assert.Equal(t, "unpinned", first.Status.Pin.Strength)
				assert.Equal(t, expectedHash, first.Status.Pin.Digest)
				assert.Equal(t, "1.2.3", first.Status.Pin.Version)
				assert.NotNil(t, first.Status.Pin.ObservedAt, "ObservedAt must be stamped")
				assert.Equal(t, "1", first.Status.Pin.Details["toolCount"])

				c := meta.FindStatusCondition(first.Status.Conditions, spiceboxv1alpha1.PinDriftCondition)
				require.NotNil(t, c, "PinDrift condition must be present")
				assert.Equal(t, metav1.ConditionTrue, c.Status, "PinDrift must be True (healthy)")
				assert.Equal(t, spiceboxv1alpha1.ReasonPinMatch, c.Reason)
			},
		},
		{
			name: "unchanged re-reconcile preserves ObservedAt",
			setup: func(t *testing.T) (string, func(), func()) {
				s := mcptest.New(mcptest.Behavior{Tools: defaultTools})
				return s.URL, func() {}, s.Close
			},
			secondReconcile: true,
			check: func(t *testing.T, first, second spiceboxv1alpha1.MCPServer) {
				require.NotNil(t, first.Status.Pin, "first Pin must be set")
				require.NotNil(t, second.Status.Pin, "second Pin must be set")
				assert.Equal(t, first.Status.Pin.Digest, second.Status.Pin.Digest)
				// ObservedAt must not change on an idle re-reconcile.
				require.NotNil(t, first.Status.Pin.ObservedAt)
				require.NotNil(t, second.Status.Pin.ObservedAt)
				assert.Equal(t, first.Status.Pin.ObservedAt.Time.UTC(), second.Status.Pin.ObservedAt.Time.UTC(),
					"ObservedAt must not churn on idle re-reconcile")

				c := meta.FindStatusCondition(second.Status.Conditions, spiceboxv1alpha1.PinDriftCondition)
				require.NotNil(t, c)
				assert.Equal(t, metav1.ConditionTrue, c.Status)
				assert.Equal(t, spiceboxv1alpha1.ReasonPinMatch, c.Reason)
			},
		},
		{
			name: "live mutation → PinDrift=False/PinDrifted with accept-hint",
			setup: func(t *testing.T) (string, func(), func()) {
				s := mcptest.New(mcptest.Behavior{Tools: defaultTools})
				return s.URL, func() { s.SetBehavior(mcptest.Behavior{Tools: alteredTools}) }, s.Close
			},
			secondReconcile: true,
			mutateBetween: func(_ *spiceboxv1alpha1.MCPServer, setAlt func()) {
				setAlt()
			},
			check: func(t *testing.T, first, second spiceboxv1alpha1.MCPServer) {
				require.NotNil(t, first.Status.Pin)
				assert.Equal(t, expectedHash, first.Status.Pin.Digest, "first reconcile: original hash")

				c := meta.FindStatusCondition(second.Status.Conditions, spiceboxv1alpha1.PinDriftCondition)
				require.NotNil(t, c)
				assert.Equal(t, metav1.ConditionFalse, c.Status, "PinDrift must be False on drift")
				assert.Equal(t, spiceboxv1alpha1.ReasonPinDrifted, c.Reason)
				assert.Contains(t, c.Message, "set spec.pinnedManifestHash to the new hash to accept",
					"accept-hint must appear in the message")
				assert.Contains(t, c.Message, expectedHash, "old hash must appear")
				assert.Contains(t, c.Message, alteredHash, "new hash must appear")
			},
		},
		{
			name: "asserted hash match → PinDrift=True/PinMatch with frozen strength",
			setup: func(t *testing.T) (string, func(), func()) {
				s := mcptest.New(mcptest.Behavior{Tools: defaultTools})
				return s.URL, func() {}, s.Close
			},
			mutateCR: func(cr *spiceboxv1alpha1.MCPServer) {
				cr.Spec.PinnedManifestHash = expectedHash
			},
			check: func(t *testing.T, first, _ spiceboxv1alpha1.MCPServer) {
				require.NotNil(t, first.Status.Pin)
				assert.Equal(t, "frozen", first.Status.Pin.Strength, "assertion → frozen strength")
				assert.Equal(t, expectedHash, first.Status.Pin.Digest)

				c := meta.FindStatusCondition(first.Status.Conditions, spiceboxv1alpha1.PinDriftCondition)
				require.NotNil(t, c)
				assert.Equal(t, metav1.ConditionTrue, c.Status)
				assert.Equal(t, spiceboxv1alpha1.ReasonPinMatch, c.Reason)
			},
		},
		{
			name: "asserted hash mismatch → PinDrift=False/PinDrifted",
			setup: func(t *testing.T) (string, func(), func()) {
				s := mcptest.New(mcptest.Behavior{Tools: defaultTools})
				return s.URL, func() {}, s.Close
			},
			mutateCR: func(cr *spiceboxv1alpha1.MCPServer) {
				cr.Spec.PinnedManifestHash = alteredHash // asserts altered but server serves original
			},
			check: func(t *testing.T, first, _ spiceboxv1alpha1.MCPServer) {
				c := meta.FindStatusCondition(first.Status.Conditions, spiceboxv1alpha1.PinDriftCondition)
				require.NotNil(t, c)
				assert.Equal(t, metav1.ConditionFalse, c.Status)
				assert.Equal(t, spiceboxv1alpha1.ReasonPinDrifted, c.Reason)
				assert.Contains(t, c.Message, alteredHash, "baseline (asserted) hash in message")
				assert.Contains(t, c.Message, expectedHash, "live hash in message")
				assert.Contains(t, c.Message, "set spec.pinnedManifestHash to the new hash to accept")
			},
		},
		{
			name: "unreachable server with existing baseline → PinDrift=False/PinVerifyFailed",
			setup: func(t *testing.T) (string, func(), func()) {
				// Use a closed server so the probe fails.
				s := mcptest.New(mcptest.Behavior{Tools: defaultTools})
				url := s.URL
				s.Close()
				return url, func() {}, func() {}
			},
			mutateCR: func(cr *spiceboxv1alpha1.MCPServer) {
				// Pre-seed a baseline so there's something to compare against.
				now := metav1.NewTime(time.Now())
				cr.Status.Pin = &spiceboxv1alpha1.PinRecord{
					Kind:       mcppin.KindName,
					Strength:   "unpinned",
					Digest:     expectedHash,
					ObservedAt: &now,
				}
			},
			check: func(t *testing.T, first, _ spiceboxv1alpha1.MCPServer) {
				c := meta.FindStatusCondition(first.Status.Conditions, spiceboxv1alpha1.PinDriftCondition)
				require.NotNil(t, c, "PinDrift condition must be set when baseline exists and server unreachable")
				assert.Equal(t, metav1.ConditionFalse, c.Status)
				assert.Equal(t, spiceboxv1alpha1.ReasonPinVerifyFailed, c.Reason)
				assert.Contains(t, c.Message, "server unreachable")
			},
		},
		{
			name: "unreachable server with no baseline → no PinDrift condition",
			setup: func(t *testing.T) (string, func(), func()) {
				s := mcptest.New(mcptest.Behavior{Tools: defaultTools})
				url := s.URL
				s.Close()
				return url, func() {}, func() {}
			},
			check: func(t *testing.T, first, _ spiceboxv1alpha1.MCPServer) {
				c := meta.FindStatusCondition(first.Status.Conditions, spiceboxv1alpha1.PinDriftCondition)
				assert.Nil(t, c, "PinDrift must NOT be set when server is unreachable and no baseline exists")
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			url, setAlt, close := tc.setup(t)
			defer close()

			cr := newMCPServer(url)
			if tc.mutateCR != nil {
				tc.mutateCR(cr)
			}
			c := buildFakeClientFor(t, cr)
			r := &mcpserver.Reconciler{Client: c, HTTP: http.DefaultClient, RevalidateInterval: time.Minute}

			first := reconcileLinear(t, c, r)

			if !tc.secondReconcile {
				tc.check(t, first, spiceboxv1alpha1.MCPServer{})
				return
			}

			if tc.mutateBetween != nil {
				tc.mutateBetween(&first, setAlt)
			}
			second := reconcileLinear(t, c, r)
			tc.check(t, first, second)
		})
	}
}

// TestReconcile_PinRefreeze covers the four refreeze-annotation cases from
// the Plan 4 Task 1 spec. Each case uses two reconciles: the first establishes
// state (a baseline or drift), the second has the annotation present.
func TestReconcile_PinRefreeze(t *testing.T) {
	// Compute hashes once for the two stable tool lists.
	hashFor := func(t *testing.T, tools []mcptest.Tool) string {
		t.Helper()
		srv := mcptest.New(mcptest.Behavior{Tools: tools})
		defer srv.Close()
		cr := newMCPServer(srv.URL)
		c := buildFakeClientFor(t, cr)
		got := reconcileLinear(t, c, &mcpserver.Reconciler{Client: c, HTTP: http.DefaultClient})
		require.NotNil(t, got.Status.Pin)
		return got.Status.Pin.Digest
	}
	defaultHash := hashFor(t, defaultTools)
	altHash := hashFor(t, alteredTools)
	require.NotEqual(t, defaultHash, altHash)

	cases := []struct {
		name string
		// toolsFirst is served on the first reconcile (establishes baseline).
		toolsFirst []mcptest.Tool
		// toolsSecond is served on the second reconcile.
		toolsSecond []mcptest.Tool
		// mutateBetween may patch the in-memory CR between reconciles.
		mutateBetween func(t *testing.T, cr *spiceboxv1alpha1.MCPServer)
		// refreezeAnnotation is set on the CR before the second reconcile.
		refreezeAnnotation string
		check              func(t *testing.T, got spiceboxv1alpha1.MCPServer)
	}{
		{
			name:               "honored: annotation == liveHash, drift present → baseline rewritten + annotation gone + PinMatch",
			toolsFirst:         defaultTools,
			toolsSecond:        alteredTools, // drift on second reconcile
			refreezeAnnotation: altHash,      // matches live on second reconcile
			check: func(t *testing.T, got spiceboxv1alpha1.MCPServer) {
				// Annotation must have been cleared.
				_, hasAnnotation := got.Annotations[spiceboxv1alpha1.AnnotationPinRefreeze]
				assert.False(t, hasAnnotation, "pin-refreeze annotation must be cleared after honoring")

				// Baseline must be updated to the new (alt) hash.
				require.NotNil(t, got.Status.Pin)
				assert.Equal(t, altHash, got.Status.Pin.Digest, "baseline must be rewritten to live hash")

				// ObservedAt must be fresh (not the stale first-reconcile value).
				require.NotNil(t, got.Status.Pin.ObservedAt)

				// PinDrift must be True/PinMatch.
				c := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.PinDriftCondition)
				require.NotNil(t, c)
				assert.Equal(t, metav1.ConditionTrue, c.Status)
				assert.Equal(t, spiceboxv1alpha1.ReasonPinMatch, c.Reason)
			},
		},
		{
			name:               "stale: annotation != liveHash → drift stays, message notes failed refreeze",
			toolsFirst:         defaultTools,
			toolsSecond:        alteredTools, // live is altHash on second reconcile
			refreezeAnnotation: defaultHash,  // stale: points at old hash, not live
			check: func(t *testing.T, got spiceboxv1alpha1.MCPServer) {
				// Annotation must NOT have been cleared (stale refreeze).
				annotationVal, hasAnnotation := got.Annotations[spiceboxv1alpha1.AnnotationPinRefreeze]
				assert.True(t, hasAnnotation, "stale annotation must remain")
				assert.Equal(t, defaultHash, annotationVal)

				// Baseline must remain at the original hash (not auto-updated).
				require.NotNil(t, got.Status.Pin)
				assert.Equal(t, defaultHash, got.Status.Pin.Digest, "baseline must not change on stale refreeze")

				// PinDrift must be False/PinDrifted with the refreeze note appended.
				c := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.PinDriftCondition)
				require.NotNil(t, c)
				assert.Equal(t, metav1.ConditionFalse, c.Status)
				assert.Equal(t, spiceboxv1alpha1.ReasonPinDrifted, c.Reason)
				assert.Contains(t, c.Message, "refreeze to", "message must mention the failed refreeze")
				assert.Contains(t, c.Message, defaultHash, "message must include the stale refreeze value")
				assert.Contains(t, c.Message, altHash, "message must include the live hash")
			},
		},
		{
			name:        "refreeze vs spec assertion: rejected, annotation stays, drift message explains",
			toolsFirst:  defaultTools,
			toolsSecond: defaultTools, // server still serves defaultHash
			mutateBetween: func(t *testing.T, cr *spiceboxv1alpha1.MCPServer) {
				// Set a spec assertion to a different value — assertion wins.
				cr.Spec.PinnedManifestHash = altHash
			},
			refreezeAnnotation: defaultHash, // annotation matches live, but spec says altHash
			check: func(t *testing.T, got spiceboxv1alpha1.MCPServer) {
				// Annotation must NOT be cleared (rejected — user still sees it pending).
				_, hasAnnotation := got.Annotations[spiceboxv1alpha1.AnnotationPinRefreeze]
				assert.True(t, hasAnnotation, "rejected refreeze annotation must remain")

				// PinDrift must be False (spec assertion != live).
				c := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.PinDriftCondition)
				require.NotNil(t, c)
				assert.Equal(t, metav1.ConditionFalse, c.Status)
				// Message must explain the assertion wins.
				assert.Contains(t, c.Message, "assertion", "message must explain assertion wins")
			},
		},
		{
			name:               "non-drifted: annotation == liveHash, liveHash == baseline → annotation cleared + PinMatch",
			toolsFirst:         defaultTools,
			toolsSecond:        defaultTools, // same tools, no drift
			refreezeAnnotation: defaultHash,  // annotation == liveHash == baseline
			check: func(t *testing.T, got spiceboxv1alpha1.MCPServer) {
				// Annotation must be cleared (honored even when no drift was present).
				_, hasAnnotation := got.Annotations[spiceboxv1alpha1.AnnotationPinRefreeze]
				assert.False(t, hasAnnotation, "annotation must be cleared even on no-op refreeze")

				// Baseline must still reflect defaultHash.
				require.NotNil(t, got.Status.Pin)
				assert.Equal(t, defaultHash, got.Status.Pin.Digest)

				// PinDrift must be True/PinMatch.
				c := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.PinDriftCondition)
				require.NotNil(t, c)
				assert.Equal(t, metav1.ConditionTrue, c.Status)
				assert.Equal(t, spiceboxv1alpha1.ReasonPinMatch, c.Reason)
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// First reconcile: establish state.
			srv := mcptest.New(mcptest.Behavior{Tools: tc.toolsFirst})
			defer srv.Close()
			cr := newMCPServer(srv.URL)
			c := fake.NewClientBuilder().WithScheme(testfixtures.NewScheme(t)).
				WithObjects(cr).
				WithStatusSubresource(&spiceboxv1alpha1.MCPServer{}).
				Build()
			r := &mcpserver.Reconciler{Client: c, HTTP: http.DefaultClient, RevalidateInterval: time.Minute}
			reconcileLinear(t, c, r)

			// Apply between-reconcile mutations (spec changes etc).
			var fresh spiceboxv1alpha1.MCPServer
			require.NoError(t, c.Get(context.Background(),
				types.NamespacedName{Namespace: "ns", Name: "linear"}, &fresh))
			if tc.mutateBetween != nil {
				tc.mutateBetween(t, &fresh)
			}

			// Set the refreeze annotation.
			if fresh.Annotations == nil {
				fresh.Annotations = map[string]string{}
			}
			fresh.Annotations[spiceboxv1alpha1.AnnotationPinRefreeze] = tc.refreezeAnnotation
			require.NoError(t, c.Update(context.Background(), &fresh),
				"set pin-refreeze annotation before second reconcile")

			// Switch to the second tool set.
			srv.SetBehavior(mcptest.Behavior{Tools: tc.toolsSecond})

			// Second reconcile: the refreeze annotation is active.
			_, err := r.Reconcile(context.Background(), ctrl.Request{
				NamespacedName: types.NamespacedName{Namespace: "ns", Name: "linear"},
			})
			require.NoError(t, err)

			var got spiceboxv1alpha1.MCPServer
			require.NoError(t, c.Get(context.Background(),
				types.NamespacedName{Namespace: "ns", Name: "linear"}, &got))
			tc.check(t, got)
		})
	}
}

// TestReconcile_ObservedDriftDigest verifies that the reconciler records the
// live manifest hash in status.Pin.Details["observedDriftDigest"] when drift is
// detected, that the baseline Digest is NOT changed while the key is present,
// and that recovery (matching probe) clears the key.
func TestReconcile_ObservedDriftDigest(t *testing.T) {
	hashFor := func(t *testing.T, tools []mcptest.Tool) string {
		t.Helper()
		srv := mcptest.New(mcptest.Behavior{Tools: tools})
		defer srv.Close()
		cr := newMCPServer(srv.URL)
		c := buildFakeClientFor(t, cr)
		got := reconcileLinear(t, c, &mcpserver.Reconciler{Client: c, HTTP: http.DefaultClient})
		require.NotNil(t, got.Status.Pin)
		return got.Status.Pin.Digest
	}
	defaultHash := hashFor(t, defaultTools)
	altHash := hashFor(t, alteredTools)
	require.NotEqual(t, defaultHash, altHash)

	t.Run("drift: observedDriftDigest set to live hash, baseline unchanged", func(t *testing.T) {
		// Establish baseline with defaultTools, then switch to alteredTools on
		// second reconcile. The key must be set to altHash; baseline stays at defaultHash.
		srv := mcptest.New(mcptest.Behavior{Tools: defaultTools})
		defer srv.Close()
		cr := newMCPServer(srv.URL)
		c := fake.NewClientBuilder().WithScheme(testfixtures.NewScheme(t)).
			WithObjects(cr).WithStatusSubresource(&spiceboxv1alpha1.MCPServer{}).Build()
		r := &mcpserver.Reconciler{Client: c, HTTP: http.DefaultClient, RevalidateInterval: time.Minute}

		// First reconcile: TOFU baseline with defaultHash.
		first := reconcileLinear(t, c, r)
		require.NotNil(t, first.Status.Pin)
		assert.Equal(t, defaultHash, first.Status.Pin.Digest)
		_, hasKey := first.Status.Pin.Details["observedDriftDigest"]
		assert.False(t, hasKey, "no drift key on first TOFU reconcile")

		// Switch to alteredTools → drift on second reconcile.
		srv.SetBehavior(mcptest.Behavior{Tools: alteredTools})
		second := reconcileLinear(t, c, r)

		require.NotNil(t, second.Status.Pin)
		assert.Equal(t, defaultHash, second.Status.Pin.Digest,
			"baseline must NOT change on drift")
		assert.Equal(t, altHash, second.Status.Pin.Details["observedDriftDigest"],
			"observedDriftDigest must be set to the live hash")
	})

	t.Run("recovery: observedDriftDigest cleared after drift resolves", func(t *testing.T) {
		// Establish drift, then accept (refreeze), verify key is gone.
		srv := mcptest.New(mcptest.Behavior{Tools: defaultTools})
		defer srv.Close()
		cr := newMCPServer(srv.URL)
		c := fake.NewClientBuilder().WithScheme(testfixtures.NewScheme(t)).
			WithObjects(cr).WithStatusSubresource(&spiceboxv1alpha1.MCPServer{}).Build()
		r := &mcpserver.Reconciler{Client: c, HTTP: http.DefaultClient, RevalidateInterval: time.Minute}

		// Establish TOFU baseline.
		reconcileLinear(t, c, r)

		// Introduce drift.
		srv.SetBehavior(mcptest.Behavior{Tools: alteredTools})
		drifted := reconcileLinear(t, c, r)
		require.Equal(t, altHash, drifted.Status.Pin.Details["observedDriftDigest"],
			"drift key must be set")

		// Accept the drift via refreeze annotation (altHash == liveHash → honored).
		var fresh spiceboxv1alpha1.MCPServer
		require.NoError(t, c.Get(context.Background(),
			types.NamespacedName{Namespace: "ns", Name: "linear"}, &fresh))
		if fresh.Annotations == nil {
			fresh.Annotations = map[string]string{}
		}
		fresh.Annotations[spiceboxv1alpha1.AnnotationPinRefreeze] = altHash
		require.NoError(t, c.Update(context.Background(), &fresh))

		recovered := reconcileLinear(t, c, r)
		require.NotNil(t, recovered.Status.Pin)
		assert.Equal(t, altHash, recovered.Status.Pin.Digest, "baseline must be updated to altHash")
		_, hasKey := recovered.Status.Pin.Details["observedDriftDigest"]
		assert.False(t, hasKey, "observedDriftDigest must be cleared after recovery")
	})
}
