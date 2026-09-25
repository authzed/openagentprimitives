package agentsession

import (
	"context"
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	admissionv1 "k8s.io/api/admission/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

const (
	testNS      = "agents"
	testSession = "sess-a"
	// attacker is the SA of testSession's own runner pod — the principal a
	// compromised agent process holds.
	attacker = "system:serviceaccount:agents:sess-a-runner-sa"
	// victimSubject is the canonical subject of an unrelated, more-privileged
	// human whose credentials the attacker wants projected.
	victimSubject = "user:victim-canonical"
	ownSubject    = "user:attacker-canonical"
	// The sha256-hex channelkey.LabelValue of a slack thread. Both hashes are
	// derived from values (channel id + thread ts) visible to everyone in the
	// channel, so an attacker can compute the victim's without any secret.
	ownThreadHash    = "1111111111111111111111111111111111111111111111111111111111111111"
	victimThreadHash = "2222222222222222222222222222222222222222222222222222222222222222"
)

func handler(t *testing.T) *Webhook {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, spiceboxv1alpha1.AddToScheme(s))
	return New(admission.NewDecoder(s))
}

// session builds the baseline AgentSession: owned by the attacker, no pending
// restart. Mutators perturb one dimension per case.
func session(muts ...func(*spiceboxv1alpha1.AgentSession)) *spiceboxv1alpha1.AgentSession {
	s := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Name: testSession, Namespace: testNS,
			// Every live AgentSession carries the operator's finalizer
			// (EnsureFinalizer, agentsession/controller.go) — so a case can
			// STRIP an existing one, which is the direction that skips
			// finalize's memory-token revoke, not only add a foreign one.
			Finalizers: []string{spiceboxv1alpha1.FinalizerAgentSession},
			// The channel correlation labels channelsd's inbound lookup selects
			// on, so a case can repoint an EXISTING binding rather than only
			// adding one.
			Labels: map[string]string{
				spiceboxv1alpha1.LabelChannelName: "demo-channel",
				spiceboxv1alpha1.LabelChannelKind: "slack",
				spiceboxv1alpha1.LabelChannelKey:  ownThreadHash,
			},
			Annotations: map[string]string{
				spiceboxv1alpha1.AnnotationStartedByCanonicalID: ownSubject,
				spiceboxv1alpha1.AnnotationStartedByExternalID:  "U-ATTACKER",
				spiceboxv1alpha1.AnnotationStartedByEmail:       "attacker@example.test",
			},
		},
		Spec: spiceboxv1alpha1.AgentSessionSpec{Class: "demo-class"},
		// The status carries one entry of each operator-/channelsd-authored
		// collection, so a case can perturb an EXISTING entry (the realistic
		// forgery — an injected entry a rebuild would drop) rather than only
		// appending to an empty list.
		Status: spiceboxv1alpha1.AgentSessionStatus{
			Phase: spiceboxv1alpha1.AgentSessionPhaseIdle,
			BundleSessions: []spiceboxv1alpha1.ResolvedBundle{
				{Name: "shell", SpiceboxSessionName: testSession + "-shell", AgentIdentity: "demo-identity"},
			},
			PendingRequesters: []spiceboxv1alpha1.PendingRequester{
				{Kind: "slack", ExternalID: "U-COWORKER", Email: "coworker@example.test", RequestRef: "req-1"},
			},
			PendingInteractions: []spiceboxv1alpha1.PendingInteraction{
				{RequestID: "int-1", Category: "tool_approval"},
			},
			PassthroughCredHashes:    map[string]string{"linear-oauth": "sha256:abcd"},
			ResolvedSidecarToolboxes: []spiceboxv1alpha1.ResolvedSidecarToolbox{{Ref: "k8s-proxy", SecretTokenHash: "sha256:beef"}},
			// A non-nil allowedToolkits is the admin ceiling in force; nilling it
			// is what ToolkitAllowed reads as "unconstrained".
			EffectiveSettings: &spiceboxv1alpha1.EffectiveSettings{
				AllowedToolkits: []string{"kubectl"},
			},
		},
	}
	for _, m := range muts {
		m(s)
	}
	return s
}

// updateReq builds an UPDATE admission request from user, on subresource sub
// ("" for the main resource, "status" for the status subresource).
func updateReq(t *testing.T, user, sub string, older, newer *spiceboxv1alpha1.AgentSession) admission.Request {
	t.Helper()
	oldRaw, err := json.Marshal(older)
	require.NoError(t, err)
	newRaw, err := json.Marshal(newer)
	require.NoError(t, err)
	return admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		Operation:   admissionv1.Update,
		SubResource: sub,
		UserInfo:    authenticationv1.UserInfo{Username: user},
		Object:      runtime.RawExtension{Raw: newRaw},
		OldObject:   runtime.RawExtension{Raw: oldRaw},
	}}
}

// statusJSONFieldName returns a struct field's json name (tag minus options),
// or "" for an untagged / json:"-" field.
//
// A local copy of pkg/apis/v1alpha1's unexported jsonFieldName. Exporting that
// one to share it would widen the API surface of the types package for a
// test-only reflection helper; six lines here is the cheaper trade.
func statusJSONFieldName(f reflect.StructField) string {
	tag := f.Tag.Get("json")
	if tag == "" || tag == "-" {
		return ""
	}
	name, _, _ := strings.Cut(tag, ",")
	return name
}

// serializedField is one field of a status struct as it reaches the CRD schema:
// its json name, plus the Go path that produced it (for a legible failure when
// the field came from an inlined member several levels down).
type serializedField struct {
	goPath string
	json   string
}

// serializedStatusFields returns every json field a status struct serializes,
// FLATTENING inlined and embedded members the way controller-gen flattens them
// into the CRD schema. The second return is the Go paths of fields that are
// serialized but cannot be named — the cases a classifier must be told about
// rather than allowed to skip.
//
// Recursion is the point. The obvious walk is one non-recursive pass that
// `continue`s on a field with no json name — which silently exempts exactly the
// construct whose fields cannot be named individually. controller-gen flattens
// an inlined struct into the parent's schema, so every one of its fields is
// serialized, patchable by the runner's `agentsessions/status` verb, and would
// be classified by neither pinnedStatusFields nor runnerWritableStatusFields,
// with the suite green. Inline embedding is idiomatic in this API (a shared
// SettingsStatus across two CRDs), so it is a reachable growth path, not a
// hypothetical. A guard that skips the case it cannot name is how a hole gets
// made; this one recurses instead, and reports what it still cannot name.
func serializedStatusFields(t reflect.Type) (fields []serializedField, unnameable []string) {
	return walkSerializedFields(t, nil)
}

func walkSerializedFields(t reflect.Type, onPath []reflect.Type) (fields []serializedField, unnameable []string) {
	onPath = append(onPath, t)
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		tag := f.Tag.Get("json")
		if tag == "-" {
			continue // genuinely never serialized; the one safe skip
		}
		if name := statusJSONFieldName(f); name != "" {
			fields = append(fields, serializedField{goPath: f.Name, json: name})
			continue
		}
		// No json name. Either the member is flattened into the parent — an
		// anonymous field, or one carrying the `,inline` option — or it is a
		// named field somebody forgot to tag, which encoding/json still
		// serializes under its Go name.
		_, opts, _ := strings.Cut(tag, ",")
		if !f.Anonymous && !slices.Contains(strings.Split(opts, ","), "inline") {
			unnameable = append(unnameable, f.Name)
			continue
		}
		ft := f.Type
		for ft.Kind() == reflect.Pointer {
			ft = ft.Elem()
		}
		if ft.Kind() != reflect.Struct || slices.Contains(onPath, ft) {
			// A non-struct inline, or a type already on this path (which would
			// recurse forever). Neither can be flattened into named fields.
			unnameable = append(unnameable, f.Name)
			continue
		}
		inner, innerUnnameable := walkSerializedFields(ft, onPath)
		for _, sf := range inner {
			fields = append(fields, serializedField{goPath: f.Name + "." + sf.goPath, json: sf.json})
		}
		for _, u := range innerUnnameable {
			unnameable = append(unnameable, f.Name+"."+u)
		}
	}
	return fields, unnameable
}

// TestSerializedStatusFieldsFlattensInlinedMembers drives the walk itself,
// because AgentSessionStatus has no inlined member TODAY — so nothing in
// TestEveryStatusFieldIsPinnedOrWaived can show whether the walk would classify
// one or quietly skip it. The synthetic types below are what the API type is
// one embedded struct away from becoming.
func TestSerializedStatusFieldsFlattensInlinedMembers(t *testing.T) {
	got, unnameable := serializedStatusFields(reflect.TypeOf(inlineProbeStatus{}))

	byJSON := map[string]string{}
	for _, f := range got {
		byJSON[f.json] = f.goPath
	}

	assert.Contains(t, byJSON, "phase", "a plainly-tagged field must be returned")
	assert.Contains(t, byJSON, "sharedCeiling",
		"a field of a `json:\",inline\"` member is flattened into the CRD schema and patchable under agentsessions/status, "+
			"so the walk must return it — skipping the member exempts every field it carries from classification")
	assert.Contains(t, byJSON, "embeddedCeiling",
		"an anonymous member with no json tag is flattened by encoding/json and by controller-gen alike; it must be walked too")
	assert.Equal(t, "InlineProbeShared.SharedCeiling", byJSON["sharedCeiling"],
		"the Go path must name the member the field came from, so a failure says where to look")
	assert.NotContains(t, byJSON, "-", "a json:\"-\" field is never serialized and must not be returned")

	assert.Equal(t, []string{"Untagged"}, unnameable,
		"a named field with no json tag is still serialized (encoding/json uses its Go name), so it must be REPORTED rather "+
			"than skipped — the skip is what made the inline hole")
}

// inlineProbe* are fixtures for TestSerializedStatusFieldsFlattensInlinedMembers
// only. Exported names so the embedded field names are exported, matching the
// shape a real API type would have.
type InlineProbeShared struct {
	// Reached only by flattening an explicit `json:",inline"` embed.
	SharedCeiling []string `json:"sharedCeiling,omitempty"`
}

type InlineProbeEmbedded struct {
	// Reached only by flattening an embed carrying no json tag at all.
	EmbeddedCeiling string `json:"embeddedCeiling,omitempty"`
}

type inlineProbeStatus struct {
	// The ordinary tagged case, present so the walk has a baseline to find.
	Phase             string `json:"phase,omitempty"`
	InlineProbeShared `json:",inline"`
	InlineProbeEmbedded
	// Serialized under its Go name because encoding/json needs no tag.
	Untagged string
	// The one field genuinely absent from the wire, so the walk must omit it.
	Skipped string `json:"-"`
}

// TestEveryStatusFieldIsPinnedOrWaived is the drift guard, and the reason this
// package has a table instead of a chain of comparisons.
//
// The failure mode of pinnedStatusFields is OMISSION, and every other test in
// this file is blind to it: each restates a row that already exists, so a new
// status field added to AgentSessionStatus and left unpinned breaks nothing.
// That is not hypothetical — pinning pendingRestart while leaving
// effectiveIdentityMode, which reaches the identical Secret projection,
// unpinned is the exact shape of the mistake, and the sweep that fixed it still
// missed supersededBy.
//
// So assert the PROPERTY, not the key list: every json field of
// AgentSessionStatus must appear in exactly one of pinnedStatusFields or
// runnerWritableStatusFields, and a new field fails the unit suite until
// someone classifies it. Same discipline as
// TestEveryStatusFieldHasExactlyOneOwner and the cluster-kind invariants test,
// which is what let a sixth cluster kind be added without weakening the
// first-boot rule.
func TestEveryStatusFieldIsPinnedOrWaived(t *testing.T) {
	pinned := map[string]bool{}
	for _, f := range pinnedStatusFields {
		assert.False(t, pinned[f.name], "pinnedStatusFields has a duplicate row for %q", f.name)
		pinned[f.name] = true
		assert.NotEmpty(t, f.why,
			"pinnedStatusFields[%q].why completes the denial message the runner's author reads; it may not be empty", f.name)
	}
	for name, why := range runnerWritableStatusFields {
		assert.NotEmpty(t, why,
			"runnerWritableStatusFields[%q] must carry a reason — the written justification IS the value of the waiver; "+
				"a bare name is the name list this test exists to prevent", name)
	}

	st := reflect.TypeOf(spiceboxv1alpha1.AgentSessionStatus{})
	live, unnameable := serializedStatusFields(st)
	assert.Emptyf(t, unnameable,
		"AgentSessionStatus has serialized field(s) %v with no json name, so this gate cannot classify them. Give each a json "+
			"tag (or `json:\"-\"` if it must not serialize). Reporting rather than skipping is deliberate: the skip is what let "+
			"an inlined member's fields reach the CRD schema unclassified", unnameable)

	for _, f := range live {
		name := f.json
		isPinned := pinned[name]
		_, isWaived := runnerWritableStatusFields[name]
		assert.Truef(t, isPinned != isWaived,
			"AgentSessionStatus.%s (json %q) is not classified for the runner-forgery gate (pinned=%v waived=%v).\n"+
				"A session runner holds `patch` on agentsessions/status and K8s RBAC has no field granularity, so this "+
				"webhook is the only thing standing between it and this field. Decide which it is and add ONE row in "+
				"pkg/controllers/webhooks/agentsession/webhook.go:\n"+
				"  PINNED — if the operator, channelsd or the CLI reads the field back and turns it into an authorization "+
				"decision, a credential projection, a pod/image it creates, or an action on ANOTHER session. Add a "+
				"pinnedStatusField{name: %q, get: ..., why: \"...\"} to pinnedStatusFields, and a forgery case to "+
				"TestRunnerCannotForgeSessionIdentity naming the escalation.\n"+
				"  WAIVED — otherwise. Add runnerWritableStatusFields[%q] = \"<who writes it, and why a forged value buys "+
				"nothing>\". Say which: the runner writes it via pkg/agent/runner.StatusPatcher, or the operator recomputes "+
				"it every reconcile, or a forgery only affects the forging session.\n"+
				"If you cannot write that sentence honestly, the answer is PINNED.",
			f.goPath, name, isPinned, isWaived, name, name)
	}

	// The waiver map may not name a field that no longer exists: a stale entry
	// is a silent hole, since it would waive a future field that reuses the name.
	serialized := map[string]bool{}
	for _, f := range live {
		serialized[f.json] = true
	}
	for name := range runnerWritableStatusFields {
		assert.Truef(t, serialized[name],
			"runnerWritableStatusFields[%q] names no field of AgentSessionStatus — remove the stale waiver", name)
	}
	for _, f := range pinnedStatusFields {
		assert.Truef(t, serialized[f.name],
			"pinnedStatusFields row %q names no field of AgentSessionStatus — remove or rename it", f.name)
	}
}

// TestEveryObjectMetaFieldIsPinnedOrWaived is the metadata counterpart of
// TestEveryStatusFieldIsPinnedOrWaived, and it is what makes the metadata half
// of this gate exhaustive rather than a fourth hand-maintained list.
//
// The runner Role's `patch` verb on the main resource covers ALL of ObjectMeta,
// and until this test existed the gate read two of its fifteen fields —
// annotations and labels — plus spec. metadata.labels itself was ungated
// through two sweeps of the status half, which is the evidence that "someone
// will remember to add a row" is not a mechanism.
//
// Annotations and labels stay allowlist-gated because their key spaces are
// OPEN — there is nothing to reflect over, so refuse-by-default is the only
// closed shape. The rest of ObjectMeta is a closed struct, exactly like
// AgentSessionStatus, so it gets exactly what the status half gets: a reflect
// walk that fails until every field is classified into precisely one of the
// three maps.
func TestEveryObjectMetaFieldIsPinnedOrWaived(t *testing.T) {
	pinned := map[string]bool{}
	for _, f := range pinnedMetadataFields {
		assert.Falsef(t, pinned[f.name], "pinnedMetadataFields has a duplicate row for %q", f.name)
		pinned[f.name] = true
		assert.NotEmptyf(t, f.why,
			"pinnedMetadataFields[%q].why completes the denial message the runner's author reads; it may not be empty", f.name)
	}
	for name, why := range runnerWritableMetadataFields {
		assert.NotEmptyf(t, why,
			"runnerWritableMetadataFields[%q] must carry a reason — say whether the API server itself refuses the change, or "+
				"the server owns the value, or a forged value buys nothing. A bare name is the list this test exists to prevent", name)
	}
	for name, why := range allowlistGatedMetadataFields {
		assert.NotEmptyf(t, why, "allowlistGatedMetadataFields[%q] must name the allowlist that gates it", name)
	}

	// The same walk the status partition uses, so an inlined member would be
	// FLATTENED rather than skipped. metav1.ObjectMeta has none today; using the
	// walk anyway costs nothing and keeps one classifier for both halves.
	live, unnameable := walkSerializedFields(reflect.TypeOf(metav1.ObjectMeta{}), nil)
	assert.Emptyf(t, unnameable,
		"metav1.ObjectMeta has serialized field(s) %v with no json name, so this gate cannot classify them", unnameable)
	require.NotEmpty(t, live, "the walk returned no fields — the reflection, not ObjectMeta, is what broke")

	for _, f := range live {
		isPinned := pinned[f.json]
		_, isWaived := runnerWritableMetadataFields[f.json]
		_, isKeyGated := allowlistGatedMetadataFields[f.json]
		inExactlyOne := (isPinned && !isWaived && !isKeyGated) ||
			(!isPinned && isWaived && !isKeyGated) ||
			(!isPinned && !isWaived && isKeyGated)
		assert.Truef(t, inExactlyOne,
			"metav1.ObjectMeta.%s (json %q) is not classified for the runner-forgery gate (pinned=%v waived=%v key-gated=%v).\n"+
				"A session runner holds `patch` on its own agentsession, and K8s RBAC has no field granularity, so this webhook "+
				"is the only thing standing between it and this field. Add ONE row in pkg/controllers/webhooks/agentsession/webhook.go:\n"+
				"  PINNED — if anything reads the field back and turns it into an authorization decision, a lifecycle decision "+
				"(what deletes this object, whether it can be deleted at all), or an effect on ANOTHER object. Add a "+
				"pinnedMetadataField{name: %q, get: ..., why: \"...\"} and a forgery case to TestRunnerCannotForgeSessionIdentity.\n"+
				"  KEY-GATED — only for a field whose key space is open and partly runner-writable; it needs its own allowlist, "+
				"like annotations and labels.\n"+
				"  WAIVED — otherwise. Add runnerWritableMetadataFields[%q] = \"<why a runner writing it changes nothing>\". If "+
				"the reason is that the API server refuses the change, say which validation does it — the claim is checkable, "+
				"and an apimachinery change would make it false.\n"+
				"If you cannot write that sentence honestly, the answer is PINNED.",
			f.goPath, f.json, isPinned, isWaived, isKeyGated, f.json, f.json)
	}

	// No map may name a field that no longer exists: a stale row is a silent
	// hole, since it would waive a future field that reuses the name.
	serialized := map[string]bool{}
	for _, f := range live {
		serialized[f.json] = true
	}
	for _, f := range pinnedMetadataFields {
		assert.Truef(t, serialized[f.name], "pinnedMetadataFields row %q names no field of metav1.ObjectMeta", f.name)
	}
	for name := range runnerWritableMetadataFields {
		assert.Truef(t, serialized[name], "runnerWritableMetadataFields[%q] names no field of metav1.ObjectMeta — remove the stale waiver", name)
	}
	for name := range allowlistGatedMetadataFields {
		assert.Truef(t, serialized[name], "allowlistGatedMetadataFields[%q] names no field of metav1.ObjectMeta", name)
	}
}

// TestRunnerMetadataGateIsDefaultDeny asserts the PROPERTY that makes the
// metadata half exhaustive, rather than restating any row.
//
// AgentSessionStatus is a closed struct, so its partition can be a reflect walk
// that fails on an unclassified field (TestEveryStatusFieldIsPinnedOrWaived).
// metadata is an open key space, so there is nothing to reflect over: what
// stands in for the walk is that the gate is an ALLOWLIST. A key nobody
// anticipated — a label a future controller invents, an annotation some other
// component starts writing — must be refused without anyone adding a row, which
// is the only shape in which omission fails closed. metadata.labels was ungated
// for exactly as long as this half was a three-element denylist.
func TestRunnerMetadataGateIsDefaultDeny(t *testing.T) {
	w := handler(t)
	unanticipated := "agentprimitives.authzed.com/nobody-has-written-this-yet"

	t.Run("an annotation key in neither map: denied (the allowlist is what closes the space, not a pin list)", func(t *testing.T) {
		older := session()
		newer := older.DeepCopy()
		newer.Annotations[unanticipated] = "1"
		resp := w.Handle(context.Background(), updateReq(t, attacker, "", older, newer))
		assert.False(t, resp.Allowed, "an unwaived annotation must be refused; response=%+v", resp.Result)
	})

	t.Run("a label key in neither map: denied (labels are cross-session correlation keys; none is runner-writable)", func(t *testing.T) {
		older := session()
		newer := older.DeepCopy()
		newer.Labels[unanticipated] = "1"
		resp := w.Handle(context.Background(), updateReq(t, attacker, "", older, newer))
		assert.False(t, resp.Allowed, "an unwaived label must be refused; response=%+v", resp.Result)
	})

	t.Run("every metadata waiver carries a written reason and is waived in exactly one map", func(t *testing.T) {
		for key, why := range runnerWritableAnnotations {
			assert.NotEmptyf(t, why,
				"runnerWritableAnnotations[%q] must carry a reason — the written justification IS the value of the waiver", key)
			_, alsoPinned := pinnedAnnotationReasons[key]
			assert.Falsef(t, alsoPinned,
				"annotation %q is both waived and given a pinned denial reason; a key the runner may write cannot also be one it may not", key)
		}
		for key, why := range runnerWritableLabels {
			assert.NotEmptyf(t, why, "runnerWritableLabels[%q] must carry a reason naming why the label is not a cross-session handle", key)
		}
		for key, why := range pinnedAnnotationReasons {
			assert.NotEmptyf(t, why, "pinnedAnnotationReasons[%q] completes the denial sentence; it may not be empty", key)
		}
	})
}

// TestRunnerCannotForgeSessionIdentity pins the privilege-escalation gate: a
// compromised runner holds `patch` on both its own AgentSession and that
// session's /status subresource (rbac.go), and K8s RBAC has no field
// granularity. Each case is a forgery that must be refused at admission.
func TestRunnerCannotForgeSessionIdentity(t *testing.T) {
	cases := []struct {
		name string
		sub  string
		mut  func(*spiceboxv1alpha1.AgentSession)
	}{
		{
			name: "runner rewrites started-by-canonical-id to a victim: denied (operator would re-project the victim's OAuth master Secrets)",
			mut: func(s *spiceboxv1alpha1.AgentSession) {
				s.Annotations[spiceboxv1alpha1.AnnotationStartedByCanonicalID] = victimSubject
			},
		},
		{
			name: "runner deletes started-by-canonical-id: denied (an unset starter re-derives ownership from the remaining annotations)",
			mut: func(s *spiceboxv1alpha1.AgentSession) {
				delete(s.Annotations, spiceboxv1alpha1.AnnotationStartedByCanonicalID)
			},
		},
		{
			name: "runner rewrites started-by-external-id: denied (feeds the pipeline's external-ID fast path)",
			mut: func(s *spiceboxv1alpha1.AgentSession) {
				s.Annotations[spiceboxv1alpha1.AnnotationStartedByExternalID] = "U-VICTIM"
			},
		},
		{
			name: "runner rewrites started-by-email: denied (feeds the identity-choice gate's requester)",
			mut: func(s *spiceboxv1alpha1.AgentSession) {
				s.Annotations[spiceboxv1alpha1.AnnotationStartedByEmail] = "victim@example.test"
			},
		},
		{
			name: "runner rewrites spec.class: denied (a class with another AgentIdentity gets its credentials projected)",
			mut: func(s *spiceboxv1alpha1.AgentSession) {
				s.Spec.Class = "privileged-class"
			},
		},
		{
			// channelsd's inbound correlation lists sessions by exactly these
			// two labels (pipeline.go) and picks `active` as the newest by
			// CreationTimestamp — so a runner that repoints them at another
			// human's thread hash hijacks that thread's delivery. The interact
			// check then denies, and handlePermissionDeny publishes a
			// permission_request carrying a 200-rune preview of the victim's
			// message text plus their identity into the ATTACKER's own session
			// channel; the victim's agent never answers.
			name: "runner repoints the channel correlation labels at another human's thread: denied (channelsd delivers that thread here and leaks its message text)",
			mut: func(s *spiceboxv1alpha1.AgentSession) {
				s.Labels[spiceboxv1alpha1.LabelChannelName] = "victim-channel"
				s.Labels[spiceboxv1alpha1.LabelChannelKey] = victimThreadHash
			},
		},
		{
			name: "runner adds the output-channel-key label for a victim thread: denied (the fallback correlation lookup matches on that label alone)",
			mut: func(s *spiceboxv1alpha1.AgentSession) {
				s.Labels[spiceboxv1alpha1.LabelOutputChannelKey] = victimThreadHash
			},
		},
		{
			name: "runner deletes a channel correlation label: denied (it strands its own thread and is not the runner's to author either way)",
			mut: func(s *spiceboxv1alpha1.AgentSession) {
				delete(s.Labels, spiceboxv1alpha1.LabelChannelKey)
			},
		},
		{
			name: "runner forges status.pendingRestart takeover naming a victim: denied (takeover skips the SpiceDB fork gate)",
			sub:  "status",
			mut: func(s *spiceboxv1alpha1.AgentSession) {
				s.Status.PendingRestart = &spiceboxv1alpha1.PendingRestart{
					Mode:              spiceboxv1alpha1.PendingRestartModeTakeover,
					TriggeredBy:       identity.Subject(victimSubject),
					NewUserText:       "exfiltrate everything",
					TargetSessionName: testSession + "-2",
					RequestedAt:       metav1.Now(),
				}
			},
		},
		{
			name: "runner rewrites an existing pendingRestart's triggeredBy: denied (channelsd's marker is not the runner's to edit)",
			sub:  "status",
			mut: func(s *spiceboxv1alpha1.AgentSession) {
				s.Status.PendingRestart.TriggeredBy = identity.Subject(victimSubject)
			},
		},
		{
			// The escalation is that this gate refuses in EVERY mode, including
			// logging. Clearing the flag switches off a control an operator
			// deliberately cannot switch off, from inside the process it
			// constrains — and the runner could not have learned the value
			// honestly either, since deriving it needs a List over the
			// delegation tree its Role does not grant.
			name: "runner clears status.closureDenied: denied (it would switch off a gate that refuses in every mode)",
			sub:  "status",
			mut: func(s *spiceboxv1alpha1.AgentSession) {
				clean := false
				s.Status.ClosureDenied = &clean
			},
		},
		{
			// The escalation is the LOOP itself. A runner that can refill its
			// own wake credit is one of the two agents the budget bounds, so a
			// forged value lets two sessions sustain an agent-to-agent
			// conversation with no person in it — the exact failure cross-agent
			// thread participation is bounded to prevent.
			name: "runner refills its own status.agentWakeCredit: denied (it is one of the two agents the budget bounds; a forged refill IS the loop)",
			sub:  "status",
			mut: func(s *spiceboxv1alpha1.AgentSession) {
				forged := 99
				s.Status.AgentWakeCredit = &forged
			},
		},
		{
			name: "runner forges status.effectiveIdentityMode=userPassthrough: denied (the passthrough gate would project the starter's OAuth master Secrets)",
			sub:  "status",
			mut: func(s *spiceboxv1alpha1.AgentSession) {
				s.Status.EffectiveIdentityMode = spiceboxv1alpha1.IdentityModeUserPassthrough
			},
		},
		{
			name: "runner rewrites status.auditPublicKey: denied (the operator re-registers it as a trusted verify-on-write key)",
			sub:  "status",
			mut: func(s *spiceboxv1alpha1.AgentSession) {
				s.Status.AuditPublicKey = "b3RoZXIta2V5LW5vdC10aGUtc2Vzc2lvbi1zZWVkcw=="
			},
		},
		{
			name: "runner rewrites status.auditKeyID: denied (the key ID is half the trust anchor oap audit verify reads)",
			sub:  "status",
			mut: func(s *spiceboxv1alpha1.AgentSession) {
				s.Status.AuditKeyID = "deadbeef"
			},
		},
		{
			name: "runner pre-stamps status.auditChainHeads: denied (a non-nil map permanently blocks the real truncation anchor)",
			sub:  "status",
			mut: func(s *spiceboxv1alpha1.AgentSession) {
				s.Status.AuditChainHeads = map[string]string{"session:agents/sess-a": "1:0000"}
			},
		},
		{
			// The forged value is a subject SET the attacker is a member of, in
			// the one shape the writer accepts: spicedb.ParseSubject requires
			// "type:id#relation", and the schema's `participant: user |
			// group#member` admits group#member. On a thread takeover the
			// operator copies the PARENT's value onto the victim's newly created
			// child session (restart.go), so this buys interact — hence
			// memory_entry#read, hence the inherited transcript — on a session
			// owned by a different human.
			name: "runner forges status.appliedInteractPermission with a group it belongs to: denied (a later takeover copies it onto the victim's child as #participant)",
			sub:  "status",
			mut: func(s *spiceboxv1alpha1.AgentSession) {
				s.Status.AppliedInteractPermission = "group:attackers#member"
			},
		},
		{
			name: "runner rewrites status.appliedInteractPermissionAt: denied (the timestamp is channelsd's evidence of when the policy was snapshotted)",
			sub:  "status",
			mut: func(s *spiceboxv1alpha1.AgentSession) {
				t := metav1.NewTime(time.Unix(1, 0))
				s.Status.AppliedInteractPermissionAt = &t
			},
		},
		{
			// The operator turns spiceboxSessionName into BOTH a Role
			// resourceName and a memory-API token scope, on every reconcile,
			// without re-deriving the list in any parked phase.
			name: "runner injects a bundleSession naming another session: denied (it becomes a Role resourceName and a memory-token scope)",
			sub:  "status",
			mut: func(s *spiceboxv1alpha1.AgentSession) {
				s.Status.BundleSessions = append(s.Status.BundleSessions,
					spiceboxv1alpha1.ResolvedBundle{Name: "x", SpiceboxSessionName: "victim-session"})
			},
		},
		{
			name: "runner rewrites a pendingRequester's identity: denied (Approve grants #participant to the named subject and replays its text past the permission check)",
			sub:  "status",
			mut: func(s *spiceboxv1alpha1.AgentSession) {
				// The canonical the grant is written for is derived from these.
				s.Status.PendingRequesters[0].ExternalID = "U-ATTACKER"
				s.Status.PendingRequesters[0].Email = "attacker@example.test"
				s.Status.PendingRequesters[0].MessageText = "delete everything"
			},
		},
		{
			name: "runner injects a pendingInteraction: denied (oap session approve auto-selects a lone entry, collecting a real owner decision for a request the owner never saw)",
			sub:  "status",
			mut: func(s *spiceboxv1alpha1.AgentSession) {
				s.Status.PendingInteractions = append(s.Status.PendingInteractions,
					spiceboxv1alpha1.PendingInteraction{RequestID: "forged", Category: "tool_approval"})
			},
		},
		{
			name: "runner clears status.passthroughCredHashes: denied (the prime branch then suppresses the credential invalidation for a replaced credential)",
			sub:  "status",
			mut: func(s *spiceboxv1alpha1.AgentSession) {
				s.Status.PassthroughCredHashes = nil
			},
		},
		{
			name: "runner blanks a resolvedSidecarToolbox's secretTokenHash: denied (no prior hash means the sidecar is never replaced after a token rotation)",
			sub:  "status",
			mut: func(s *spiceboxv1alpha1.AgentSession) {
				s.Status.ResolvedSidecarToolboxes[0].SecretTokenHash = ""
			},
		},
		{
			// One patch, any non-empty string. ReconcileRestart's first test is
			// `SupersededBy != ""` → clearPendingRestart, so from here on EVERY
			// marker channelsd writes onto this session is nil'd with no
			// condition, no notice and no log. The marker that is discarded is a
			// different human's TAKEOVER — the one path that deliberately skips
			// the SpiceDB fork gate because channelsd is its choke point, and the
			// recovery mechanism against a runner behaving exactly like this one.
			name: "runner forges status.supersededBy: denied (it makes ReconcileRestart silently discard every later takeover marker)",
			sub:  "status",
			mut: func(s *spiceboxv1alpha1.AgentSession) {
				s.Status.SupersededBy = "no-such-child"
			},
		},
		{
			// The operator recomputes effectiveSettings each reconcile — but the
			// revocation sweep reads the PERSISTED value first, above every gate
			// that can park the reconcile. reconcileCredentialGrants gates each
			// credential's authorized_token grant on ToolkitAllowed, which reads
			// a nil allowedToolkits as unconstrained, so nilling it collects a
			// grant for a toolkit the ADMIN's cluster∩namespace ceiling forbade.
			// From a parked phase the authoritative diff that would delete the
			// grant again is never reached.
			name: "runner nils status.effectiveSettings.allowedToolkits: denied (the revocation sweep reads it before the recompute and grants authorized_token past the admin ceiling)",
			sub:  "status",
			mut: func(s *spiceboxv1alpha1.AgentSession) {
				s.Status.EffectiveSettings.AllowedToolkits = nil
			},
		},
		{
			// Nothing removes a finalizer the operator does not own: finalize
			// deletes only FinalizerAgentSession, and early-returns when its own
			// is absent. The object stays Terminating forever — and
			// newestActiveSession does NOT filter on DeletionTimestamp, so a
			// husk left at a non-Succeeded phase keeps winning the newestLive
			// branch and captures the thread permanently. Deleting the session's
			// namespace never completes either.
			name: "runner adds a foreign finalizer: denied (nothing can ever remove it, so the object wedges Terminating and holds the thread)",
			mut: func(s *spiceboxv1alpha1.AgentSession) {
				s.Finalizers = append(s.Finalizers, "example.test/wedge")
			},
		},
		{
			// The other direction, and the worse one. finalize is the ONLY
			// caller of Tokens.Revoke, the registry is in-process
			// (pkg/memory/tokens), and pkg/memory/httpsrv authenticates a
			// per-session bearer token against that registry with no liveness
			// check on the AgentSession. So "delete the session" is the only
			// kill switch for a token the runner may have exfiltrated, and
			// stripping the finalizer defeats it for the operator's lifetime —
			// along with the SpiceDB relationship deletion and the
			// cross-namespace passthrough RBAC reap.
			name: "runner strips the operator's finalizer: denied (finalize never runs, so the session's memory token is never revoked)",
			mut: func(s *spiceboxv1alpha1.AgentSession) {
				s.Finalizers = nil
			},
		},
		{
			// No AgentSession is created with an ownerReference anywhere in the
			// repo, so any value is a forgery. The runner Role grants no
			// `delete` on agentsessions — but the GC deletes a dependent once
			// its owner refs are all dangling, and the API server validates
			// neither the owner's existence nor its namespace. This is a
			// deletion primitive attributed to the garbage collector.
			name: "runner forges an ownerReference to a non-existent owner: denied (the GC then deletes the session, routing around the absent delete verb)",
			mut: func(s *spiceboxv1alpha1.AgentSession) {
				s.OwnerReferences = []metav1.OwnerReference{{
					APIVersion: "v1", Kind: "ConfigMap",
					Name: "no-such-owner", UID: "00000000-0000-0000-0000-000000000000",
				}}
			},
		},
		{
			name: "runner forges a blockOwnerDeletion ownerReference to another object: denied (it wedges that object's foreground deletion)",
			mut: func(s *spiceboxv1alpha1.AgentSession) {
				s.OwnerReferences = []metav1.OwnerReference{{
					APIVersion: spiceboxv1alpha1.SchemeBuilder.GroupVersion.String(), Kind: "AgentSession",
					Name: "victim-session", UID: "11111111-1111-1111-1111-111111111111",
					BlockOwnerDeletion: ptr.To(true),
				}}
			},
		},
	}

	w := handler(t)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			older := session()
			if tc.name == "runner rewrites an existing pendingRestart's triggeredBy: denied (channelsd's marker is not the runner's to edit)" {
				older.Status.PendingRestart = &spiceboxv1alpha1.PendingRestart{
					Mode:              spiceboxv1alpha1.PendingRestartModeTakeover,
					TriggeredBy:       identity.Subject(ownSubject),
					TargetSessionName: testSession + "-2",
				}
			}
			newer := older.DeepCopy()
			tc.mut(newer)

			resp := w.Handle(context.Background(), updateReq(t, attacker, tc.sub, older, newer))
			assert.False(t, resp.Allowed, "forgery must be refused at admission; response=%+v", resp.Result)
		})
	}
}

// TestLegitimateAgentSessionWritesAreAllowed pins the other half: the gate must
// not break the writes the runner Role exists to permit, and must not touch any
// principal other than a session runner.
func TestLegitimateAgentSessionWritesAreAllowed(t *testing.T) {
	cases := []struct {
		name string
		user string
		sub  string
		mut  func(*spiceboxv1alpha1.AgentSession)
	}{
		{
			name: "runner stamps the wake-requested-at annotation: allowed (the write the Role's patch verb exists for)",
			user: attacker,
			mut: func(s *spiceboxv1alpha1.AgentSession) {
				s.Annotations[spiceboxv1alpha1.AnnotationWakeRequestedAt] = "2026-08-10T00:00:00Z"
			},
		},
		{
			name: "runner stamps the sidecar-failure-reported annotation: allowed (its own dedup record of what it has told the user)",
			user: attacker,
			mut: func(s *spiceboxv1alpha1.AgentSession) {
				s.Annotations[spiceboxv1alpha1.AnnotationSidecarFailureReported] = `{"k8s-proxy":"CrashLoopBackOff|boom"}`
			},
		},
		{
			name: "runner clears the sidecar-failure-reported annotation after a heal: allowed (the whole set is re-encoded each pass)",
			user: attacker,
			mut: func(s *spiceboxv1alpha1.AgentSession) {
				s.Annotations[spiceboxv1alpha1.AnnotationSidecarFailureReported] = ""
			},
		},
		{
			name: "runner patches its own status.phase: allowed (the runner's normal lifecycle reporting)",
			user: attacker,
			sub:  "status",
			mut: func(s *spiceboxv1alpha1.AgentSession) {
				s.Status.Phase = spiceboxv1alpha1.AgentSessionPhaseRunning
			},
		},
		{
			name: "operator clears status.pendingRestart: allowed (not a runner SA; the operator owns the clear)",
			user: "system:serviceaccount:agentprimitives-system:spicebox-operator",
			sub:  "status",
			mut: func(s *spiceboxv1alpha1.AgentSession) {
				s.Status.PendingRestart = nil
			},
		},
		{
			name: "channelsd sets a takeover pendingRestart: allowed (channelsd is the authorization choke point)",
			user: "system:serviceaccount:agentprimitives-system:spicebox-channelsd",
			sub:  "status",
			mut: func(s *spiceboxv1alpha1.AgentSession) {
				s.Status.PendingRestart = &spiceboxv1alpha1.PendingRestart{
					Mode:        spiceboxv1alpha1.PendingRestartModeTakeover,
					TriggeredBy: identity.Subject(victimSubject),
				}
			},
		},
		{
			name: "channelsd snapshots status.appliedInteractPermission: allowed (channelsd owns the interact-policy snapshot at session creation)",
			user: "system:serviceaccount:agentprimitives-system:spicebox-channelsd",
			sub:  "status",
			mut: func(s *spiceboxv1alpha1.AgentSession) {
				s.Status.AppliedInteractPermission = "group:engineering#member"
				now := metav1.NewTime(time.Unix(1, 0))
				s.Status.AppliedInteractPermissionAt = &now
			},
		},
		{
			name: "a human rewrites started-by-canonical-id: allowed (kubectl admin is out of this gate's scope)",
			user: "kubernetes-admin",
			mut: func(s *spiceboxv1alpha1.AgentSession) {
				s.Annotations[spiceboxv1alpha1.AnnotationStartedByCanonicalID] = victimSubject
			},
		},
		{
			// The legitimate writer of metadata.finalizers, and the only one:
			// EnsureFinalizer on every reconcile of a live session.
			name: "operator adds its own finalizer: allowed (EnsureFinalizer runs on every reconcile; the finalizer pin is not the operator's)",
			user: "system:serviceaccount:agentprimitives-system:spicebox-operator",
			mut: func(s *spiceboxv1alpha1.AgentSession) {
				s.Finalizers = append(s.Finalizers, "example.test/some-other-controller")
			},
		},
		{
			name: "operator removes its finalizer at finalize: allowed (the removal IS teardown, and it is the operator's write)",
			user: "system:serviceaccount:agentprimitives-system:spicebox-operator",
			mut: func(s *spiceboxv1alpha1.AgentSession) {
				s.Finalizers = nil
			},
		},
		{
			// The false-denial trap in whole-of-ObjectMeta gating, asserted so a
			// future sweep cannot pin managedFields by reflex. store.Update runs
			// rest.BeforeUpdate — which is where the field manager rewrites
			// managedFields — BEFORE updateValidation, i.e. before this webhook
			// is dialed. So the runner's own annotation patch always arrives
			// with a managedFields entry the old object does not have, and a
			// gate that compared the field would refuse the one write the
			// runner Role's `patch` verb exists for.
			name: "runner's wake annotation patch carries the API server's own managedFields rewrite: allowed (managedFields differs on EVERY write)",
			user: attacker,
			mut: func(s *spiceboxv1alpha1.AgentSession) {
				s.Annotations[spiceboxv1alpha1.AnnotationWakeRequestedAt] = "2026-08-10T00:00:00Z"
				now := metav1.NewTime(time.Unix(2, 0))
				s.ManagedFields = append(s.ManagedFields, metav1.ManagedFieldsEntry{
					Manager:    "runner",
					Operation:  metav1.ManagedFieldsOperationUpdate,
					APIVersion: spiceboxv1alpha1.SchemeBuilder.GroupVersion.String(),
					Time:       &now,
				})
			},
		},
	}

	w := handler(t)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			older := session()
			if tc.sub == "status" && tc.mut != nil && tc.user != attacker {
				older.Status.PendingRestart = &spiceboxv1alpha1.PendingRestart{
					Mode: spiceboxv1alpha1.PendingRestartModeInherit,
				}
			}
			newer := older.DeepCopy()
			tc.mut(newer)

			resp := w.Handle(context.Background(), updateReq(t, tc.user, tc.sub, older, newer))
			assert.True(t, resp.Allowed, "legitimate write must not be refused; response=%+v", resp.Result)
		})
	}
}
