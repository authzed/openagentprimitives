package meta

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/sandbox"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/guardian/leakage"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	_ "github.com/authzed/openagentprimitives/pkg/memory/kinds/all"
	"github.com/authzed/openagentprimitives/pkg/platform/artifacts"
)

func TestRespondSchemaCapsTextMarkdown(t *testing.T) {
	tt := newRespondTool(RespondConfig{
		Capabilities: []string{"text", "markdown"},
		ChannelKind:  "fake",
	})
	assert.Equal(t, "respond_to_user", tt.Name(), "name must be respond_to_user")

	var schema map[string]any
	require.NoError(t, json.Unmarshal(tt.InputSchema(), &schema), "unmarshal schema")

	props, _ := schema["properties"].(map[string]any)
	_, hasText := props["text"]
	assert.True(t, hasText, "text property must be present")
	_, hasBlocks := props["blocks"]
	assert.False(t, hasBlocks, "blocks must NOT be present without 'components' capability")
	assert.Contains(t, tt.Description(), "Markdown", "Description should mention Markdown when caps include markdown")
}

func TestRespondSchemaCapsTextOnly(t *testing.T) {
	tt := newRespondTool(RespondConfig{
		Capabilities: []string{"text"},
		ChannelKind:  "fake",
	})
	assert.Contains(t, strings.ToLower(tt.Description()), "plain text",
		"Description should mention plain text when markdown is unavailable")
}

func TestRespondExecutePublishesEnvelope(t *testing.T) {
	var got struct {
		Subject string
		Bytes   []byte
		Calls   int
	}
	pub := func(_ context.Context, subject string, payload []byte) error {
		got.Subject = subject
		got.Bytes = append([]byte(nil), payload...)
		got.Calls++
		return nil
	}
	tt := newRespondTool(RespondConfig{
		Capabilities:      []string{"text", "markdown"},
		ChannelKind:       "fake",
		NATSPublish:       pub,
		NATSSubjectPrefix: "ap.session.default.foo",
	})

	res, err := tt.Execute(memory.WithSystemApproval(context.Background(), "test"),
		json.RawMessage(`{"text":"hello"}`),
		&tool.SessionContext{Namespace: "default", Name: "foo"},
	)
	require.NoError(t, err, "Execute must not return a Go error")
	assert.False(t, res.IsError, "no error expected on happy publish")
	assert.False(t, res.Terminal, "respond_to_user must not terminate the session")
	assert.True(t, res.Trusted, "respond_to_user is a framework meta tool and must opt out of content-guard inspection")
	assert.Equal(t, 1, got.Calls, "expected exactly one publish call")
	assert.Equal(t, "ap.session.default.foo.out.user_message", got.Subject, "subject must be the user_message NATS subject")

	var env channelevents.Envelope
	require.NoError(t, json.Unmarshal(got.Bytes, &env), "unmarshal envelope")
	assert.Equal(t, channelevents.KindUserMessage, env.Kind, "envelope kind must be user_message")
	assert.Equal(t, 1, env.Version, "envelope version must be 1")

	var pl channelevents.OutboundUserMessagePayload
	require.NoError(t, json.Unmarshal(env.Payload, &pl), "unmarshal payload")
	assert.Equal(t, "hello", pl.Text, "payload text must match input")
}

// TestRespondExecuteLeakageGateError_PublishesFallbackToChannel codifies
// the "no silent errors" rule for the info-leakage gate: when LeakageGate
// returns an error, respond_to_user MUST publish a generic fallback
// user_message to the channel so the requester learns the system saw
// their request and decided not to answer — instead of returning silence
// when the LLM eventually calls agent_work_complete. The full error still
// goes back to the LLM as IsError so it can adapt, but the channel sees a
// generic notice (the raw error may contain internal config details).
func TestRespondExecuteLeakageGateError_PublishesFallbackToChannel(t *testing.T) {
	var publishes []struct {
		Subject string
		Payload channelevents.OutboundUserMessagePayload
	}
	pub := func(_ context.Context, subject string, payload []byte) error {
		var env channelevents.Envelope
		if err := json.Unmarshal(payload, &env); err != nil {
			t.Fatalf("unmarshal envelope: %v", err)
		}
		var pl channelevents.OutboundUserMessagePayload
		if err := json.Unmarshal(env.Payload, &pl); err != nil {
			t.Fatalf("unmarshal payload: %v", err)
		}
		publishes = append(publishes, struct {
			Subject string
			Payload channelevents.OutboundUserMessagePayload
		}{Subject: subject, Payload: pl})
		return nil
	}
	gateErr := errors.New("ApproverSubject not set; cannot route approval")
	tt := newRespondTool(RespondConfig{
		Capabilities:      []string{"text"},
		ChannelKind:       "fake",
		NATSPublish:       pub,
		NATSSubjectPrefix: "ap.session.default.foo",
		LeakageGate: func(context.Context, *tool.SessionContext, string, []channelevents.AttachmentRef) error {
			return gateErr
		},
	})
	res, err := tt.Execute(memory.WithSystemApproval(context.Background(), "test"),
		json.RawMessage(`{"text":"the original tainted content"}`),
		&tool.SessionContext{Namespace: "default", Name: "foo"},
	)
	require.NoError(t, err, "Execute must not return a Go error")
	assert.True(t, res.IsError, "gate failure must surface IsError to the LLM")
	assert.Contains(t, res.Content, "information-leakage gate", "LLM-facing error must mention the gate")

	require.Len(t, publishes, 1, "expected exactly one fallback publish to the channel")
	assert.Equal(t, "ap.session.default.foo.out.user_message", publishes[0].Subject,
		"fallback must go to the channel via the user_message subject")
	assert.NotContains(t, publishes[0].Payload.Text, "the original tainted content",
		"fallback MUST NOT contain the original tainted message text")
	assert.NotContains(t, publishes[0].Payload.Text, "ApproverSubject",
		"fallback MUST NOT leak the raw gate error (internal config details)")
	assert.NotEmpty(t, publishes[0].Payload.Text, "fallback must say SOMETHING — silence violates the no-silent-errors rule")
}

// TestRespondExecuteLeakageDenied_YieldsToUserNotRetry verifies the denial
// path: when LeakageGate returns an error wrapping leakage.ErrShareDenied,
// respond_to_user must (a) publish exactly ONE notice asking the user how to
// proceed (not the generic "operator notified" blocked notice), and (b) return
// a Terminal+IdleExit result so the channel-attached session yields to the user
// instead of letting the LLM retry and re-publish the notice on every attempt.
func TestRespondExecuteLeakageDenied_YieldsToUserNotRetry(t *testing.T) {
	var publishes []channelevents.OutboundUserMessagePayload
	pub := func(_ context.Context, _ string, payload []byte) error {
		var env channelevents.Envelope
		require.NoError(t, json.Unmarshal(payload, &env), "unmarshal envelope")
		var pl channelevents.OutboundUserMessagePayload
		require.NoError(t, json.Unmarshal(env.Payload, &pl), "unmarshal payload")
		publishes = append(publishes, pl)
		return nil
	}
	gateErr := fmt.Errorf("info-leakage: approval denied: nope: %w", leakage.ErrShareDenied)
	tt := newRespondTool(RespondConfig{
		Capabilities:      []string{"text"},
		ChannelKind:       "fake",
		NATSPublish:       pub,
		NATSSubjectPrefix: "ap.session.default.foo",
		LeakageGate: func(context.Context, *tool.SessionContext, string, []channelevents.AttachmentRef) error {
			return gateErr
		},
	})
	res, err := tt.Execute(memory.WithSystemApproval(context.Background(), "test"),
		json.RawMessage(`{"text":"the original tainted content"}`),
		&tool.SessionContext{Namespace: "default", Name: "foo"},
	)
	require.NoError(t, err, "Execute must not return a Go error")
	assert.True(t, res.Terminal, "a denied share must yield the turn (Terminal)")
	assert.True(t, res.IdleExit, "a denied share must go Idle (await user), not Succeed")
	assert.True(t, res.ShareDenied, "a denied share must flag ShareDenied so the runner records a distinct ShareDeniedYield audit event, not a plain IdleYield")
	assert.True(t, res.IsError, "the LLM-facing result still marks the gate fired")
	assert.Contains(t, res.Content, "Do not retry", "LLM result must instruct against retrying")

	require.Len(t, publishes, 1, "exactly one notice — no per-retry spam")
	assert.Contains(t, publishes[0].Text, "How would you like me to proceed",
		"notice must ask the user how to proceed")
	assert.NotContains(t, publishes[0].Text, "operator has been notified",
		"denial must use the 'how to proceed' notice, not the generic blocked notice")
	assert.NotContains(t, publishes[0].Text, "the original tainted content",
		"notice MUST NOT echo the tainted message text")
}

func TestRespondExecuteEmptyText(t *testing.T) {
	tt := newRespondTool(RespondConfig{
		Capabilities:      []string{"text"},
		ChannelKind:       "fake",
		NATSPublish:       func(_ context.Context, _ string, _ []byte) error { return nil },
		NATSSubjectPrefix: "ap.session.default.foo",
	})
	res, err := tt.Execute(memory.WithSystemApproval(context.Background(), "test"),
		json.RawMessage(`{"text":""}`),
		&tool.SessionContext{Namespace: "default", Name: "foo"},
	)
	require.NoError(t, err, "Execute must not return a Go error")
	assert.True(t, res.IsError, "empty text must produce IsError")
}

func TestRespondExecuteAppendsDeliveredSystemNote(t *testing.T) {
	var got []map[string]any
	pub := func(_ context.Context, _ string, _ []byte) error { return nil }
	apd := func(_ context.Context, content map[string]any) error {
		got = append(got, content)
		return nil
	}
	tt := newRespondTool(RespondConfig{
		Capabilities:      []string{"text"},
		ChannelKind:       "fake",
		NATSPublish:       pub,
		NATSSubjectPrefix: "ap.session.default.foo",
		AppendSystemNote:  apd,
	})
	ctx := sandbox.WithIDs(memory.WithSystemApproval(context.Background(), "test"), sandbox.IDs{TurnIndex: 1, ToolUseID: "tu_42"})
	res, err := tt.Execute(ctx,
		json.RawMessage(`{"text":"hello"}`),
		&tool.SessionContext{Namespace: "default", Name: "foo"},
	)
	require.NoError(t, err, "Execute must not return a Go error")
	require.False(t, res.IsError, "no error expected; content=%s", res.Content)
	require.Len(t, got, 1, "expected exactly one system_note appended")
	delivered, _ := got[0]["delivered"].([]string)
	require.Len(t, delivered, 1, "delivered must contain the tool_use_id")
	assert.Equal(t, "tu_42", delivered[0], "delivered must reference the right tool_use_id")
}

func TestRespondExecutePublishRetries(t *testing.T) {
	calls := 0
	pub := func(_ context.Context, _ string, _ []byte) error {
		calls++
		if calls < 3 {
			return errors.New("transient")
		}
		return nil
	}
	tt := newRespondTool(RespondConfig{
		Capabilities:      []string{"text"},
		ChannelKind:       "fake",
		NATSPublish:       pub,
		NATSSubjectPrefix: "ap.session.default.foo",
	})
	res, err := tt.Execute(memory.WithSystemApproval(context.Background(), "test"),
		json.RawMessage(`{"text":"x"}`),
		&tool.SessionContext{Namespace: "default", Name: "foo"},
	)
	require.NoError(t, err, "Execute must not return a Go error")
	assert.False(t, res.IsError, "must succeed after retries")
	assert.GreaterOrEqual(t, calls, 3, "must retry at least to the third attempt")
}

func TestRespond_ResolvesLogicalAttachmentToRender(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, spiceboxv1alpha1.AddToScheme(scheme))

	uid := types.UID("ai-uid")
	sessCR := &spiceboxv1alpha1.AgentSession{ObjectMeta: metav1.ObjectMeta{Name: "sess1", Namespace: "default", UID: uid}}
	cr := &spiceboxv1alpha1.ArtifactRender{
		ObjectMeta: metav1.ObjectMeta{
			Name: "ar-1", Namespace: "default", UID: "cr-uid",
			Labels:          map[string]string{artifacts.LabelArtifactID: "artifact-x"},
			OwnerReferences: []metav1.OwnerReference{{Kind: "AgentSession", Name: "sess1", UID: uid}},
		},
		Status: spiceboxv1alpha1.ArtifactRenderStatus{Phase: spiceboxv1alpha1.ArtifactRenderPhaseReady, OutputMIME: "text/html", OutputFilename: "o.html"},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(sessCR, cr).WithStatusSubresource(cr).Build()

	svc := artifacts.NewService(memory.NewLocal(inmem.NewBackend()), nil)
	scope := memory.Scope{Kind: "session", ID: "default/sess1"}
	finalCR := cr.DeepCopy()
	finalCR.Annotations = map[string]string{}
	_, err := svc.FinalizeRevision(memory.WithSystemApproval(context.Background(), "test"), scope, finalCR)
	require.NoError(t, err)

	var published [][]byte
	tl := New(RespondConfig{
		Capabilities: []string{"text", "asset:text/html"}, ChannelKind: "fake",
		NATSPublish: func(ctx context.Context, subject string, payload []byte) error {
			published = append(published, payload)
			return nil
		},
		NATSSubjectPrefix: "ap.session.default.sess1", Client: c, Artifacts: svc,
	})
	args, _ := json.Marshal(map[string]any{"text": "here", "attached": []string{"artifact-x"}})
	res, _ := tl.Execute(memory.WithSystemApproval(context.Background(), "test"), args, &tool.SessionContext{Namespace: "default", Name: "sess1", AgentSessionUID: uid})
	require.False(t, res.IsError, "got: %s", res.Content)
	require.Len(t, published, 1, "envelope must be published")
	assert.Contains(t, string(published[0]), "ar-1", "attachment must reference the resolved render")
}

// TestRespond_RejectsForeignOwnedLogicalAttachment is the defense-in-depth
// security regression for the new logical-handle path: even when a logical
// handle resolves (in this session's memory scope) to a render whose CR is
// owned by a DIFFERENT session, the ownership check on the fetched CR must
// still reject it and publish nothing.
func TestRespond_RejectsForeignOwnedLogicalAttachment(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, spiceboxv1alpha1.AddToScheme(scheme))

	callerUID := types.UID("caller-uid")
	foreign := &spiceboxv1alpha1.ArtifactRender{
		ObjectMeta: metav1.ObjectMeta{
			Name: "ar-foreign", Namespace: "default", UID: "foreign-cr-uid",
			Labels:          map[string]string{artifacts.LabelArtifactID: "artifact-y"},
			OwnerReferences: []metav1.OwnerReference{{Kind: "AgentSession", Name: "other", UID: types.UID("other-uid")}},
		},
		Status: spiceboxv1alpha1.ArtifactRenderStatus{Phase: spiceboxv1alpha1.ArtifactRenderPhaseReady, OutputMIME: "text/html", OutputFilename: "o.html"},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(foreign).WithStatusSubresource(foreign).Build()

	svc := artifacts.NewService(memory.NewLocal(inmem.NewBackend()), nil)
	scope := memory.Scope{Kind: "session", ID: "default/sess1"}
	seed := foreign.DeepCopy()
	seed.Annotations = map[string]string{}
	_, err := svc.FinalizeRevision(memory.WithSystemApproval(context.Background(), "test"), scope, seed)
	require.NoError(t, err)

	var published [][]byte
	tl := New(RespondConfig{
		Capabilities: []string{"text", "asset:text/html"}, ChannelKind: "fake",
		NATSPublish: func(ctx context.Context, subject string, payload []byte) error {
			published = append(published, payload)
			return nil
		},
		NATSSubjectPrefix: "ap.session.default.sess1", Client: c, Artifacts: svc,
	})
	args, _ := json.Marshal(map[string]any{"text": "here", "attached": []string{"artifact-y"}})
	res, _ := tl.Execute(memory.WithSystemApproval(context.Background(), "test"), args, &tool.SessionContext{Namespace: "default", Name: "sess1", AgentSessionUID: callerUID})
	assert.True(t, res.IsError, "foreign-owned render must be rejected even via a logical handle")
	assert.Contains(t, res.Content, "not owned by this session")
	assert.Empty(t, published, "no envelope may be published when the ownership check fails")
}

// TestRespond_ResolvesRevisionIDAttachment covers the artrev- handle form:
// a revision ID resolves to its render and delivers like any other handle.
func TestRespond_ResolvesRevisionIDAttachment(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, spiceboxv1alpha1.AddToScheme(scheme))

	uid := types.UID("ai-uid")
	cr := &spiceboxv1alpha1.ArtifactRender{
		ObjectMeta: metav1.ObjectMeta{
			Name: "ar-1", Namespace: "default", UID: "cr-uid",
			Labels:          map[string]string{artifacts.LabelArtifactID: "artifact-x"},
			OwnerReferences: []metav1.OwnerReference{{Kind: "AgentSession", Name: "sess1", UID: uid}},
		},
		Status: spiceboxv1alpha1.ArtifactRenderStatus{Phase: spiceboxv1alpha1.ArtifactRenderPhaseReady, OutputMIME: "text/html", OutputFilename: "o.html"},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cr).WithStatusSubresource(cr).Build()

	svc := artifacts.NewService(memory.NewLocal(inmem.NewBackend()), nil)
	scope := memory.Scope{Kind: "session", ID: "default/sess1"}
	seed := cr.DeepCopy()
	seed.Annotations = map[string]string{}
	rev, err := svc.FinalizeRevision(memory.WithSystemApproval(context.Background(), "test"), scope, seed)
	require.NoError(t, err)

	var published [][]byte
	tl := New(RespondConfig{
		Capabilities: []string{"text", "asset:text/html"}, ChannelKind: "fake",
		NATSPublish: func(ctx context.Context, subject string, payload []byte) error {
			published = append(published, payload)
			return nil
		},
		NATSSubjectPrefix: "ap.session.default.sess1", Client: c, Artifacts: svc,
	})
	args, _ := json.Marshal(map[string]any{"text": "here", "attached": []string{rev.RevisionID}})
	res, _ := tl.Execute(memory.WithSystemApproval(context.Background(), "test"), args, &tool.SessionContext{Namespace: "default", Name: "sess1", AgentSessionUID: uid})
	require.False(t, res.IsError, "got: %s", res.Content)
	require.Len(t, published, 1)
	assert.Contains(t, string(published[0]), "ar-1", "artrev- handle must resolve to its render")
}

// TestRespond_ResolvesTaggedAttachment covers the artifact_id#tag form
// end-to-end: a tagged handle resolves through the tag ref to its render.
func TestRespond_ResolvesTaggedAttachment(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, spiceboxv1alpha1.AddToScheme(scheme))

	uid := types.UID("ai-uid")
	cr := &spiceboxv1alpha1.ArtifactRender{
		ObjectMeta: metav1.ObjectMeta{
			Name: "ar-1", Namespace: "default", UID: "cr-uid",
			Labels:          map[string]string{artifacts.LabelArtifactID: "artifact-x"},
			Annotations:     map[string]string{artifacts.AnnoAppliedTags: "published"},
			OwnerReferences: []metav1.OwnerReference{{Kind: "AgentSession", Name: "sess1", UID: uid}},
		},
		Status: spiceboxv1alpha1.ArtifactRenderStatus{Phase: spiceboxv1alpha1.ArtifactRenderPhaseReady, OutputMIME: "text/html", OutputFilename: "o.html"},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cr).WithStatusSubresource(cr).Build()

	svc := artifacts.NewService(memory.NewLocal(inmem.NewBackend()), nil)
	scope := memory.Scope{Kind: "session", ID: "default/sess1"}
	_, err := svc.FinalizeRevision(memory.WithSystemApproval(context.Background(), "test"), scope, cr.DeepCopy())
	require.NoError(t, err)

	var published [][]byte
	tl := New(RespondConfig{
		Capabilities: []string{"text", "asset:text/html"}, ChannelKind: "fake",
		NATSPublish: func(ctx context.Context, subject string, payload []byte) error {
			published = append(published, payload)
			return nil
		},
		NATSSubjectPrefix: "ap.session.default.sess1", Client: c, Artifacts: svc,
	})
	args, _ := json.Marshal(map[string]any{"text": "here", "attached": []string{"artifact-x#published"}})
	res, _ := tl.Execute(memory.WithSystemApproval(context.Background(), "test"), args, &tool.SessionContext{Namespace: "default", Name: "sess1", AgentSessionUID: uid})
	require.False(t, res.IsError, "got: %s", res.Content)
	require.Len(t, published, 1)
	assert.Contains(t, string(published[0]), "ar-1", "artifact_id#tag handle must resolve to its tagged render")
}
