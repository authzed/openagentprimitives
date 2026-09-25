package meta_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/meta"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	_ "github.com/authzed/openagentprimitives/pkg/memory/kinds/all"
	"github.com/authzed/openagentprimitives/pkg/platform/artifacts"
)

// fakeArtifactClient is a no-network stand-in for sandbox.ArtifactClient,
// letting artifact_prepare's source=tool_output path be unit-tested without
// a real operator /debug/artifact endpoint.
type fakeArtifactClient struct {
	data    map[string][]byte
	err     error
	gotRefs []string
}

func (f *fakeArtifactClient) Get(_ context.Context, ref string) (io.ReadCloser, error) {
	f.gotRefs = append(f.gotRefs, ref)
	if f.err != nil {
		return nil, f.err
	}
	b, ok := f.data[ref]
	if !ok {
		return nil, fmt.Errorf("fakeArtifactClient: no data for ref %q", ref)
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

// fakeFileDownloader is a no-network stand-in for meta.FileDownloader,
// letting artifact_prepare's source=container_file path be unit-tested
// without a real provider.
type fakeFileDownloader struct {
	data      []byte
	err       error
	gotFileID string
}

func (f *fakeFileDownloader) Download(_ context.Context, fileID string) (io.ReadCloser, error) {
	f.gotFileID = fileID
	if f.err != nil {
		return nil, f.err
	}
	return io.NopCloser(bytes.NewReader(f.data)), nil
}

type downloadErr string

func (e downloadErr) Error() string { return string(e) }

func newPrepareSvc() *artifacts.Service {
	return artifacts.NewService(memory.NewLocal(inmem.NewBackend()), nil)
}

func newPrepareArtifactScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(s), "corev1.AddToScheme")
	require.NoError(t, spiceboxv1alpha1.AddToScheme(s), "spiceboxv1alpha1.AddToScheme")
	return s
}

// readyOnCreate flips a created ArtifactRender straight to Ready (no
// status subresource here, so Create persists the status the poll loop reads).
func readyOnCreate(uid types.UID) interceptor.Funcs {
	return interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if ar, ok := obj.(*spiceboxv1alpha1.ArtifactRender); ok {
				ar.UID = uid
				ar.Status.Phase = spiceboxv1alpha1.ArtifactRenderPhaseReady
				ar.Status.OutputRef = "mem://o"
				ar.Status.OutputMIME = "text/html"
				ar.Status.OutputSize = 10
				ar.Status.OutputFilename = "out.html"
			}
			return c.Create(ctx, obj, opts...)
		},
	}
}

func TestArtifactPrepare_NewArtifact_RecordsRevision(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(newPrepareArtifactScheme(t)).
		WithInterceptorFuncs(readyOnCreate("uid-1")).Build()
	svc := newPrepareSvc()
	tl := meta.NewArtifactPrepare(meta.ArtifactPrepareConfig{
		Client: c, Artifacts: svc, AvailableKinds: []string{"html"},
		MaxInputBytes: 1 << 20, PollInterval: time.Millisecond,
	})
	args, _ := json.Marshal(map[string]any{"kind": "html", "payload": "<h1>hi</h1>", "name": "report"})
	res, _ := tl.Execute(memory.WithSystemApproval(context.Background(), "test"), args, &tool.SessionContext{Namespace: "default", Name: "sess1"})
	require.False(t, res.IsError, "want success, got: %s", res.Content)
	assert.True(t, res.Trusted, "artifact_prepare is a framework meta tool and must opt out of content-guard inspection")

	var got map[string]any
	require.NoError(t, json.Unmarshal([]byte(res.Content), &got))
	assert.Equal(t, "ready", got["status"])
	assert.NotEmpty(t, got["artifact_id"])
	assert.NotEmpty(t, got["revision_id"])
	assert.Equal(t, float64(1), got["seq"])

	arts, err := svc.ListArtifacts(memory.WithSystemApproval(context.Background(), "test"), memory.Scope{Kind: "session", ID: "default/sess1"})
	require.NoError(t, err)
	require.Len(t, arts, 1)
	assert.Equal(t, "report", arts[0].Name)
}

func TestArtifactPrepare_RejectsReservedTag(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(newPrepareArtifactScheme(t)).Build()
	tl := meta.NewArtifactPrepare(meta.ArtifactPrepareConfig{Client: c, Artifacts: newPrepareSvc(), AvailableKinds: []string{"html"}, MaxInputBytes: 1 << 20})
	args, _ := json.Marshal(map[string]any{"kind": "html", "payload": "x", "tags": []string{"latest"}})
	res, _ := tl.Execute(memory.WithSystemApproval(context.Background(), "test"), args, &tool.SessionContext{Namespace: "default", Name: "sess1"})
	assert.True(t, res.IsError)
	assert.Contains(t, res.Content, "reserved")
}

func TestArtifactPrepare_RejectsUnknownKind(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(newPrepareArtifactScheme(t)).Build()
	tl := meta.NewArtifactPrepare(meta.ArtifactPrepareConfig{
		Client:         c,
		Artifacts:      newPrepareSvc(),
		AvailableKinds: []string{"html"},
		PollInterval:   5 * time.Millisecond,
	})
	args, err := json.Marshal(map[string]any{"kind": "no-such-renderer", "payload": "<h1>hi</h1>"})
	require.NoError(t, err, "marshal args")
	res, _ := tl.Execute(memory.WithSystemApproval(context.Background(), "test"), args, &tool.SessionContext{Namespace: "default", Name: "sess1"})
	assert.True(t, res.IsError, "unknown kind must produce IsError")
	assert.Contains(t, res.Content, "no-such-renderer", "error must name the unknown kind")
}

func TestArtifactPrepare_RejectsTooLargePayload(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(newPrepareArtifactScheme(t)).Build()
	tl := meta.NewArtifactPrepare(meta.ArtifactPrepareConfig{
		Client:         c,
		Artifacts:      newPrepareSvc(),
		AvailableKinds: []string{"html"},
		MaxInputBytes:  10,
		PollInterval:   5 * time.Millisecond,
	})
	args, err := json.Marshal(map[string]any{"kind": "html", "payload": strings.Repeat("x", 100)})
	require.NoError(t, err, "marshal args")
	res, _ := tl.Execute(memory.WithSystemApproval(context.Background(), "test"), args, &tool.SessionContext{Namespace: "default", Name: "sess1"})
	assert.True(t, res.IsError, "oversized payload must produce IsError")
	assert.Contains(t, res.Content, "PayloadTooLarge", "error must name PayloadTooLarge")
}

func TestArtifactPrepare_PollsUntilReady(t *testing.T) {
	scheme := newPrepareArtifactScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&spiceboxv1alpha1.ArtifactRender{}).Build()
	tl := meta.NewArtifactPrepare(meta.ArtifactPrepareConfig{
		Client:         c,
		Artifacts:      newPrepareSvc(),
		AvailableKinds: []string{"html"},
		MaxInputBytes:  256 << 10,
		PollInterval:   5 * time.Millisecond,
	})
	go func() {
		var cr spiceboxv1alpha1.ArtifactRender
		for {
			var list spiceboxv1alpha1.ArtifactRenderList
			_ = c.List(memory.WithSystemApproval(context.Background(), "test"), &list)
			if len(list.Items) > 0 {
				cr = list.Items[0]
				break
			}
			time.Sleep(2 * time.Millisecond)
		}
		cr.Status.Phase = spiceboxv1alpha1.ArtifactRenderPhaseReady
		cr.Status.OutputMIME = "text/html"
		cr.Status.OutputSize = 100
		cr.Status.OutputFilename = "report.html"
		cr.Status.Warnings = []spiceboxv1alpha1.SanitizerWarning{{Kind: "tag", Name: "script", Action: "stripped", Count: 1}}
		_ = c.Status().Update(memory.WithSystemApproval(context.Background(), "test"), &cr)
	}()
	args, err := json.Marshal(map[string]any{
		"kind":             "html",
		"payload":          "<h1>hi</h1>",
		"filename":         "report.html",
		"max_wait_seconds": 5,
	})
	require.NoError(t, err, "marshal args")
	res, err := tl.Execute(memory.WithSystemApproval(context.Background(), "test"), args, &tool.SessionContext{Namespace: "default", Name: "sess1"})
	require.NoError(t, err, "Execute must not return a Go error")
	require.False(t, res.IsError, "ready CR must not produce IsError: %s", res.Content)

	var body struct {
		Handle     string                              `json:"handle"`
		ArtifactID string                              `json:"artifact_id"`
		RevisionID string                              `json:"revision_id"`
		Seq        int                                 `json:"seq"`
		Status     string                              `json:"status"`
		MIME       string                              `json:"mime"`
		Size       int64                               `json:"size"`
		Warnings   []spiceboxv1alpha1.SanitizerWarning `json:"warnings"`
	}
	require.NoError(t, json.Unmarshal([]byte(res.Content), &body), "decode response: %s", res.Content)
	assert.Equal(t, "ready", body.Status, "status must be ready")
	assert.Equal(t, "text/html", body.MIME, "mime must be text/html")
	assert.NotEmpty(t, body.Warnings, "warnings from the render must propagate")
	assert.NotEmpty(t, body.ArtifactID, "artifact_id must propagate from the finalized revision")
	assert.NotEmpty(t, body.RevisionID, "revision_id must propagate from the finalized revision")
	assert.Equal(t, 1, body.Seq, "first revision seq must be 1")
}

func TestArtifactPrepare_PerKindPayloadCap(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(newPrepareArtifactScheme(t)).
		WithInterceptorFuncs(readyOnCreate("uid-cap")).Build()
	svc := newPrepareSvc()
	tl := meta.NewArtifactPrepare(meta.ArtifactPrepareConfig{
		Client: c, Artifacts: svc, AvailableKinds: []string{"html", "image"},
		MaxInputBytes:       256 << 10,
		MaxInputBytesByKind: map[string]int64{"html": 256 << 10, "image": 8 << 20},
		PollInterval:        time.Millisecond,
	})

	// image payload over the html cap but under the image cap: NOT PayloadTooLarge.
	bigImg := strings.Repeat("x", (256<<10)+10)
	args, _ := json.Marshal(map[string]any{"kind": "image", "payload": bigImg})
	res, _ := tl.Execute(memory.WithSystemApproval(context.Background(), "test"), args, &tool.SessionContext{Namespace: "default", Name: "s"})
	assert.NotContains(t, res.Content, "PayloadTooLarge", "image payload under the image cap must pass size validation")

	// html payload over the html cap: still PayloadTooLarge.
	args, _ = json.Marshal(map[string]any{"kind": "html", "payload": bigImg})
	res, _ = tl.Execute(memory.WithSystemApproval(context.Background(), "test"), args, &tool.SessionContext{Namespace: "default", Name: "s"})
	assert.True(t, res.IsError, "html payload over html cap must error")
	assert.Contains(t, res.Content, "PayloadTooLarge")

	// image payload over the image cap: PayloadTooLarge.
	tooBig := strings.Repeat("x", (8<<20)+10)
	args, _ = json.Marshal(map[string]any{"kind": "image", "payload": tooBig})
	res, _ = tl.Execute(memory.WithSystemApproval(context.Background(), "test"), args, &tool.SessionContext{Namespace: "default", Name: "s"})
	assert.True(t, res.IsError, "image payload over image cap must error")
	assert.Contains(t, res.Content, "PayloadTooLarge")
}

// TestArtifactPrepare_InlineSourceNeverSetsPayloadRef guards against
// PayloadRef regaining a runner-writable path: artifact_prepare must always
// build the CR with inline Payload bytes, leaving PayloadRef (operator-only,
// in v1 always empty) untouched. A model-supplied store_ref source was
// removed rather than kept, since there is no session-ownership check on a
// model-supplied ref.
func TestArtifactPrepare_InlineSourceNeverSetsPayloadRef(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(newPrepareArtifactScheme(t)).
		WithInterceptorFuncs(readyOnCreate("uid-source")).Build()
	svc := newPrepareSvc()
	tl := meta.NewArtifactPrepare(meta.ArtifactPrepareConfig{
		Client: c, Artifacts: svc, AvailableKinds: []string{"html"},
		MaxInputBytes: 1 << 20, PollInterval: time.Millisecond,
	})
	args, err := json.Marshal(map[string]any{"kind": "html", "payload": "<h1>hi</h1>"})
	require.NoError(t, err, "marshal args")
	res, err := tl.Execute(memory.WithSystemApproval(context.Background(), "test"), args, &tool.SessionContext{Namespace: "default", Name: "sess1"})
	require.NoError(t, err, "Execute must not return a Go error")
	require.False(t, res.IsError, "want success, got: %s", res.Content)

	var list spiceboxv1alpha1.ArtifactRenderList
	require.NoError(t, c.List(memory.WithSystemApproval(context.Background(), "test"), &list), "list ArtifactRenders")
	require.Len(t, list.Items, 1, "exactly one ArtifactRender must have been created")
	cr := list.Items[0]
	assert.Empty(t, cr.Spec.PayloadRef, "PayloadRef must stay empty; artifact_prepare has no store_ref source")
	assert.Equal(t, "<h1>hi</h1>", string(cr.Spec.Payload), "inline Payload")
}

func TestArtifactPrepare_RejectsUnknownSource(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(newPrepareArtifactScheme(t)).Build()
	tl := meta.NewArtifactPrepare(meta.ArtifactPrepareConfig{
		Client: c, Artifacts: newPrepareSvc(), AvailableKinds: []string{"html"}, MaxInputBytes: 1 << 20,
	})
	args, err := json.Marshal(map[string]any{"kind": "html", "payload": "x", "source": "bogus"})
	require.NoError(t, err, "marshal args")
	res, _ := tl.Execute(memory.WithSystemApproval(context.Background(), "test"), args, &tool.SessionContext{Namespace: "default", Name: "sess1"})
	assert.True(t, res.IsError, "unknown source must produce IsError")
	assert.Contains(t, res.Content, "bogus", "error must name the unknown source")
}

func TestArtifactPrepare_ReturnsPendingOnTimeout(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(newPrepareArtifactScheme(t)).WithStatusSubresource(&spiceboxv1alpha1.ArtifactRender{}).Build()
	tl := meta.NewArtifactPrepare(meta.ArtifactPrepareConfig{
		Client:         c,
		Artifacts:      newPrepareSvc(),
		AvailableKinds: []string{"html"},
		MaxInputBytes:  256 << 10,
		PollInterval:   5 * time.Millisecond,
	})
	args, err := json.Marshal(map[string]any{
		"kind":             "html",
		"payload":          "<h1>hi</h1>",
		"max_wait_seconds": 1,
	})
	require.NoError(t, err, "marshal args")
	start := time.Now()
	res, _ := tl.Execute(memory.WithSystemApproval(context.Background(), "test"), args, &tool.SessionContext{Namespace: "default", Name: "sess1"})
	require.False(t, res.IsError, "timeout must return pending, not IsError: %s", res.Content)
	assert.Contains(t, res.Content, `"status":"pending"`, "timeout must surface status=pending")
	assert.GreaterOrEqual(t, time.Since(start), 800*time.Millisecond, "must honor max_wait_seconds")
}

func TestArtifactPrepare_ContainerFileSourceInlinesDownloadedBytes(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(newPrepareArtifactScheme(t)).
		WithInterceptorFuncs(readyOnCreate("uid-container")).Build()
	svc := newPrepareSvc()
	downloader := &fakeFileDownloader{data: []byte("<h1>from container</h1>")}
	tl := meta.NewArtifactPrepare(meta.ArtifactPrepareConfig{
		Client: c, Artifacts: svc, AvailableKinds: []string{"html"},
		MaxInputBytes: 1 << 20, PollInterval: time.Millisecond, FileDownloader: downloader,
	})
	args, err := json.Marshal(map[string]any{"kind": "html", "payload": "file_abc", "source": "container_file"})
	require.NoError(t, err, "marshal args")
	res, err := tl.Execute(memory.WithSystemApproval(context.Background(), "test"), args, &tool.SessionContext{Namespace: "default", Name: "sess1"})
	require.NoError(t, err, "Execute must not return a Go error")
	require.False(t, res.IsError, "want success, got: %s", res.Content)

	assert.Equal(t, "file_abc", downloader.gotFileID, "FileDownloader.Download must receive the container file id as payload")

	var list spiceboxv1alpha1.ArtifactRenderList
	require.NoError(t, c.List(memory.WithSystemApproval(context.Background(), "test"), &list), "list ArtifactRenders")
	require.Len(t, list.Items, 1, "exactly one ArtifactRender must have been created")
	cr := list.Items[0]
	assert.Equal(t, downloader.data, cr.Spec.Payload, "inline Payload must be the downloaded bytes")
	assert.Empty(t, cr.Spec.PayloadRef, "PayloadRef must stay empty for source=container_file (bytes are inlined, not store-referenced)")
}

func TestArtifactPrepare_ContainerFileNilDownloaderIsError(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(newPrepareArtifactScheme(t)).Build()
	tl := meta.NewArtifactPrepare(meta.ArtifactPrepareConfig{
		Client: c, Artifacts: newPrepareSvc(), AvailableKinds: []string{"html"}, MaxInputBytes: 1 << 20,
	})
	args, err := json.Marshal(map[string]any{"kind": "html", "payload": "file_abc", "source": "container_file"})
	require.NoError(t, err, "marshal args")
	res, err := tl.Execute(memory.WithSystemApproval(context.Background(), "test"), args, &tool.SessionContext{Namespace: "default", Name: "sess1"})
	require.NoError(t, err, "Execute must not return a Go error")
	assert.True(t, res.IsError, "nil FileDownloader with source=container_file must fail closed")
}

func TestArtifactPrepare_ContainerFileDownloadErrorIsError(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(newPrepareArtifactScheme(t)).Build()
	downloader := &fakeFileDownloader{err: downloadErr("boom")}
	tl := meta.NewArtifactPrepare(meta.ArtifactPrepareConfig{
		Client: c, Artifacts: newPrepareSvc(), AvailableKinds: []string{"html"}, MaxInputBytes: 1 << 20, FileDownloader: downloader,
	})
	args, err := json.Marshal(map[string]any{"kind": "html", "payload": "file_abc", "source": "container_file"})
	require.NoError(t, err, "marshal args")
	res, err := tl.Execute(memory.WithSystemApproval(context.Background(), "test"), args, &tool.SessionContext{Namespace: "default", Name: "sess1"})
	require.NoError(t, err, "Execute must not return a Go error")
	assert.True(t, res.IsError, "download error must produce IsError")
	assert.Contains(t, res.Content, "boom")
}

func TestArtifactPrepare_ContainerFileExceedsInlineCapIsError(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(newPrepareArtifactScheme(t)).Build()
	downloader := &fakeFileDownloader{data: []byte(strings.Repeat("x", 11))}
	tl := meta.NewArtifactPrepare(meta.ArtifactPrepareConfig{
		Client: c, Artifacts: newPrepareSvc(), AvailableKinds: []string{"html"}, MaxInputBytes: 10, FileDownloader: downloader,
	})
	args, err := json.Marshal(map[string]any{"kind": "html", "payload": "file_abc", "source": "container_file"})
	require.NoError(t, err, "marshal args")
	res, err := tl.Execute(memory.WithSystemApproval(context.Background(), "test"), args, &tool.SessionContext{Namespace: "default", Name: "sess1"})
	require.NoError(t, err, "Execute must not return a Go error")
	assert.True(t, res.IsError, "downloaded bytes over the inline cap must produce IsError")
	assert.Contains(t, res.Content, "10", "error must mention the byte limit")
	assert.Contains(t, res.Content, "not yet implemented", "error must point to the deferred large-file follow-up")
}

// newOwnedToolCall builds a ToolCall CR owned by the given AgentSession
// (Kind=AgentSession, Name+UID match), mirroring how the runner stamps
// OwnerReferences on ToolCalls it creates.
func newOwnedToolCall(name, namespace, sessionName string, sessionUID types.UID, status spiceboxv1alpha1.ToolCallStatus) *spiceboxv1alpha1.ToolCall {
	tval := true
	return &spiceboxv1alpha1.ToolCall{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: spiceboxv1alpha1.SchemeBuilder.GroupVersion.String(), Kind: "AgentSession",
				Name: sessionName, UID: sessionUID, Controller: &tval, BlockOwnerDeletion: &tval,
			}},
		},
		Spec:   spiceboxv1alpha1.ToolCallSpec{Session: sessionName, Tool: "grep"},
		Status: status,
	}
}

func TestArtifactPrepare_ToolOutputSourceInlinesOwnedRef(t *testing.T) {
	sessUID := types.UID("uid-sess-1")
	ref := "mem://default/sess1/tc-1/stdout"
	tc := newOwnedToolCall("tc-1", "default", "sess1", sessUID, spiceboxv1alpha1.ToolCallStatus{StdoutArtifactRef: ref})
	c := fake.NewClientBuilder().WithScheme(newPrepareArtifactScheme(t)).
		WithObjects(tc).
		WithInterceptorFuncs(readyOnCreate("uid-tool-output")).Build()
	svc := newPrepareSvc()
	artClient := &fakeArtifactClient{data: map[string][]byte{ref: []byte("captured stdout bytes")}}
	tl := meta.NewArtifactPrepare(meta.ArtifactPrepareConfig{
		Client: c, Artifacts: svc, AvailableKinds: []string{"html"},
		MaxInputBytes: 1 << 20, PollInterval: time.Millisecond,
	})
	args, err := json.Marshal(map[string]any{"kind": "html", "payload": ref, "source": "tool_output"})
	require.NoError(t, err, "marshal args")
	sess := &tool.SessionContext{Namespace: "default", Name: "sess1", AgentSessionUID: sessUID, ArtifactClient: artClient}
	res, err := tl.Execute(memory.WithSystemApproval(context.Background(), "test"), args, sess)
	require.NoError(t, err, "Execute must not return a Go error")
	require.False(t, res.IsError, "want success, got: %s", res.Content)

	assert.Equal(t, []string{ref}, artClient.gotRefs, "ArtifactClient.Get must be called with the ref")

	var list spiceboxv1alpha1.ArtifactRenderList
	require.NoError(t, c.List(memory.WithSystemApproval(context.Background(), "test"), &list), "list ArtifactRenders")
	require.Len(t, list.Items, 1, "exactly one ArtifactRender must have been created")
	cr := list.Items[0]
	assert.Equal(t, "captured stdout bytes", string(cr.Spec.Payload), "inline Payload must be the fetched tool-output bytes")
	assert.Empty(t, cr.Spec.PayloadRef, "PayloadRef must stay empty for source=tool_output (bytes are inlined, not store-referenced)")
}

func TestArtifactPrepare_ToolOutputSourceRejectsCrossSessionRef(t *testing.T) {
	otherRef := "mem://default/other-session/tc-other/stdout"
	tc := newOwnedToolCall("tc-other", "default", "other-session", types.UID("uid-other-sess"), spiceboxv1alpha1.ToolCallStatus{StdoutArtifactRef: otherRef})
	c := fake.NewClientBuilder().WithScheme(newPrepareArtifactScheme(t)).WithObjects(tc).Build()
	artClient := &fakeArtifactClient{data: map[string][]byte{otherRef: []byte("someone else's stdout")}}
	tl := meta.NewArtifactPrepare(meta.ArtifactPrepareConfig{
		Client: c, Artifacts: newPrepareSvc(), AvailableKinds: []string{"html"}, MaxInputBytes: 1 << 20,
	})
	args, err := json.Marshal(map[string]any{"kind": "html", "payload": otherRef, "source": "tool_output"})
	require.NoError(t, err, "marshal args")
	sess := &tool.SessionContext{Namespace: "default", Name: "sess1", AgentSessionUID: types.UID("uid-sess-1"), ArtifactClient: artClient}
	res, err := tl.Execute(memory.WithSystemApproval(context.Background(), "test"), args, sess)
	require.NoError(t, err, "Execute must not return a Go error")
	assert.True(t, res.IsError, "cross-session ref must produce IsError")
	assert.Contains(t, res.Content, "cross-session", "error must name cross-session rejection")
	assert.Empty(t, artClient.gotRefs, "ArtifactClient.Get must NOT be called for a rejected ref")
}

// TestArtifactPrepare_ToolOutputSourceRejectsEmptyRef guards against an
// empty payload spuriously "matching" an owned ToolCall whose
// Status.StdoutArtifactRef/StderrArtifactRef are unset (Go zero value is
// also ""). Without an explicit empty check, validateToolOutputRef would
// treat that as a same-session match.
func TestArtifactPrepare_ToolOutputSourceRejectsEmptyRef(t *testing.T) {
	sessUID := types.UID("uid-sess-1")
	// A ToolCall owned by this session that hasn't captured any output yet
	// (all ArtifactRef fields at their zero value, "").
	tc := newOwnedToolCall("tc-running", "default", "sess1", sessUID, spiceboxv1alpha1.ToolCallStatus{})
	c := fake.NewClientBuilder().WithScheme(newPrepareArtifactScheme(t)).WithObjects(tc).Build()
	artClient := &fakeArtifactClient{data: map[string][]byte{}}
	tl := meta.NewArtifactPrepare(meta.ArtifactPrepareConfig{
		Client: c, Artifacts: newPrepareSvc(), AvailableKinds: []string{"html"}, MaxInputBytes: 1 << 20,
	})
	args, err := json.Marshal(map[string]any{"kind": "html", "payload": "", "source": "tool_output"})
	require.NoError(t, err, "marshal args")
	sess := &tool.SessionContext{Namespace: "default", Name: "sess1", AgentSessionUID: sessUID, ArtifactClient: artClient}
	res, err := tl.Execute(memory.WithSystemApproval(context.Background(), "test"), args, sess)
	require.NoError(t, err, "Execute must not return a Go error")
	assert.True(t, res.IsError, "empty ref must not spuriously match an owned ToolCall's unset ArtifactRef fields")
	assert.Contains(t, res.Content, "cross-session", "error must name cross-session rejection")
	assert.Empty(t, artClient.gotRefs, "ArtifactClient.Get must NOT be called for a rejected ref")
}

func TestArtifactPrepare_ToolOutputSourceNilArtifactClientIsError(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(newPrepareArtifactScheme(t)).Build()
	tl := meta.NewArtifactPrepare(meta.ArtifactPrepareConfig{
		Client: c, Artifacts: newPrepareSvc(), AvailableKinds: []string{"html"}, MaxInputBytes: 1 << 20,
	})
	args, err := json.Marshal(map[string]any{"kind": "html", "payload": "mem://default/sess1/tc-1/stdout", "source": "tool_output"})
	require.NoError(t, err, "marshal args")
	sess := &tool.SessionContext{Namespace: "default", Name: "sess1", AgentSessionUID: types.UID("uid-sess-1")}
	res, err := tl.Execute(memory.WithSystemApproval(context.Background(), "test"), args, sess)
	require.NoError(t, err, "Execute must not return a Go error")
	assert.True(t, res.IsError, "nil ArtifactClient with source=tool_output must fail closed")
	assert.Contains(t, res.Content, "artifact client", "error must mention the missing artifact client")
}

// TestArtifactPrepare_StampsCascadingOwnerRef pins the invariant that
// respond_to_user's DELEGATED ownership check depends on and cannot verify for
// itself.
//
// ownedBySessionNamed matches a delegated render's owner by NAME alone — it has
// no UID to compare, because the per-session runner Role pins `get` on
// agentsessions to the runner's own session (see rbac.go), so a parent cannot
// read its child's UID. Its safety argument is therefore not "we checked the
// UID" but "a render outlives its session by nothing": the ownerRef cascade
// reaps it, so no recreated same-named session can ever inherit a stranger's
// render.
//
// That argument is only as good as this ownerRef. Drop it, or clear either
// flag, and renders start surviving their session — reopening a window a
// name-only match cannot close, in a code path whose own comment says the
// window does not exist. Nothing else in the suite asserts it.
func TestArtifactPrepare_StampsCascadingOwnerRef(t *testing.T) {
	const sessUID = "sess-uid-1"
	c := fake.NewClientBuilder().WithScheme(newPrepareArtifactScheme(t)).
		WithInterceptorFuncs(readyOnCreate("uid-1")).Build()
	tl := meta.NewArtifactPrepare(meta.ArtifactPrepareConfig{
		Client: c, Artifacts: newPrepareSvc(), AvailableKinds: []string{"html"},
		MaxInputBytes: 1 << 20, PollInterval: time.Millisecond,
	})
	args, _ := json.Marshal(map[string]any{"kind": "html", "payload": "<h1>hi</h1>", "name": "report"})
	res, _ := tl.Execute(
		memory.WithSystemApproval(context.Background(), "test"), args,
		&tool.SessionContext{Namespace: "default", Name: "sess1", AgentSessionUID: sessUID},
	)
	require.False(t, res.IsError, "want success, got: %s", res.Content)

	var list spiceboxv1alpha1.ArtifactRenderList
	require.NoError(t, c.List(context.Background(), &list, client.InNamespace("default")))
	require.Len(t, list.Items, 1, "artifact_prepare must create exactly one ArtifactRender")

	refs := list.Items[0].OwnerReferences
	require.Len(t, refs, 1, "the render must carry an ownerReference, or nothing reaps it when the session goes")
	ref := refs[0]
	assert.Equal(t, "AgentSession", ref.Kind)
	assert.Equal(t, "sess1", ref.Name)
	assert.Equal(t, types.UID(sessUID), ref.UID,
		"the UID must be the session's own: a name-only ownerRef would survive into a recreated same-named session")
	require.NotNil(t, ref.Controller)
	assert.True(t, *ref.Controller, "Controller must be set for the GC cascade to own this render")
	require.NotNil(t, ref.BlockOwnerDeletion)
	assert.True(t, *ref.BlockOwnerDeletion,
		"BlockOwnerDeletion must be set so the session cannot vanish while a render still points at it")
}
