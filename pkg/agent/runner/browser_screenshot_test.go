package runner

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/platform/artifactstore"
)

// browserScreenshotTestScheme builds a scheme carrying the ArtifactRender CRD
// type. package runner (this file) cannot reuse another file's internal-test
// scheme helper across _test.go files of the same package that declare their
// own (see apptoolsGrantTestScheme's doc comment) — each file in this
// package's test suite mints its own, so this one does too.
func browserScreenshotTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, spiceboxv1alpha1.AddToScheme(s), "AddToScheme")
	return s
}

// fakeScreenshotPutter is a minimal screenshotArtifactPutter recorder: it
// never touches real storage, just remembers the keys/bytes it was asked to
// Put and hands back a fixed Ref.
type fakeScreenshotPutter struct {
	ref   artifactstore.Ref
	err   error
	calls []struct {
		key   string
		bytes []byte
	}
}

func (f *fakeScreenshotPutter) Put(_ context.Context, key string, r io.Reader) (artifactstore.Ref, error) {
	b, err := io.ReadAll(r)
	if err != nil {
		return "", err
	}
	f.calls = append(f.calls, struct {
		key   string
		bytes []byte
	}{key: key, bytes: b})
	if f.err != nil {
		return "", f.err
	}
	return f.ref, nil
}

func TestStoreScreenshotArtifact_InlineVsRef(t *testing.T) {
	scheme := browserScreenshotTestScheme(t)
	sess := channelevents.SessionRef{Namespace: "default", Name: "s1"}

	t.Run("PNG under the 256 KiB cap is stored inline", func(t *testing.T) {
		c := fake.NewClientBuilder().WithScheme(scheme).
			WithInterceptorFuncs(readyOnCreateInterceptor()).Build()
		putter := &fakeScreenshotPutter{ref: "mem://should-not-be-used"}
		deps := screenshotStoreDeps{
			Client:        c,
			ArtifactStore: putter,
			Artifacts:     uiResourceTestArtifactsService(),
			SessionUID:    types.UID("sess-uid-1"),
		}
		small := bytes.Repeat([]byte{0xAB}, 1024) // < 256 KiB

		ref, err := storeScreenshotArtifact(context.Background(), deps, sess, small)
		require.NoError(t, err)
		require.NotNil(t, ref)

		assert.NotEmpty(t, ref.RenderName)
		assert.NotEmpty(t, ref.ArtifactID)
		assert.Equal(t, "image/png", ref.MIME)
		assert.Equal(t, "page.png", ref.Filename)
		assert.Equal(t, "current page (credential fields redacted)", ref.AltText)

		var cr spiceboxv1alpha1.ArtifactRender
		require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: sess.Namespace, Name: ref.RenderName}, &cr),
			"the created ArtifactRender must be readable back by the returned RenderName")
		assert.Equal(t, ref.RenderName, cr.Name)
		assert.Equal(t, "image", cr.Spec.Kind)
		assert.Equal(t, small, []byte(cr.Spec.Payload), "small PNG must be inline")
		assert.Empty(t, cr.Spec.PayloadRef, "small PNG must not set PayloadRef")
		assert.Equal(t, ref.ArtifactID, cr.Labels["artifact.agentprimitives.authzed.com/id"])
		assert.Empty(t, putter.calls, "artifactstore.Put must not be called for an inline-sized PNG")
	})

	t.Run("PNG over the 256 KiB cap is stored by ref", func(t *testing.T) {
		c := fake.NewClientBuilder().WithScheme(scheme).
			WithInterceptorFuncs(readyOnCreateInterceptor()).Build()
		putter := &fakeScreenshotPutter{ref: "mem://default/s1/screenshot.png"}
		deps := screenshotStoreDeps{
			Client:        c,
			ArtifactStore: putter,
			Artifacts:     uiResourceTestArtifactsService(),
			SessionUID:    types.UID("sess-uid-1"),
		}
		big := bytes.Repeat([]byte{0xCD}, 300*1024) // > 256 KiB

		ref, err := storeScreenshotArtifact(context.Background(), deps, sess, big)
		require.NoError(t, err)
		require.NotNil(t, ref)
		assert.Equal(t, "image/png", ref.MIME)

		var cr spiceboxv1alpha1.ArtifactRender
		require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: sess.Namespace, Name: ref.RenderName}, &cr))
		assert.Nil(t, cr.Spec.Payload, "big PNG must not be inlined")
		assert.Equal(t, "mem://default/s1/screenshot.png", cr.Spec.PayloadRef, "big PNG must be stored by ref")

		require.Len(t, putter.calls, 1, "artifactstore.Put must be called exactly once for an oversized PNG")
		assert.Equal(t, big, putter.calls[0].bytes, "the exact PNG bytes must reach the artifact store")
	})

	t.Run("artifact store Put failure is returned, not swallowed", func(t *testing.T) {
		c := fake.NewClientBuilder().WithScheme(scheme).Build()
		putErr := errors.New("boom: store unavailable")
		putter := &fakeScreenshotPutter{err: putErr}
		deps := screenshotStoreDeps{
			Client:        c,
			ArtifactStore: putter,
			Artifacts:     uiResourceTestArtifactsService(),
			SessionUID:    types.UID("sess-uid-1"),
		}
		big := bytes.Repeat([]byte{0xEF}, 300*1024)

		ref, err := storeScreenshotArtifact(context.Background(), deps, sess, big)
		require.Error(t, err)
		assert.Nil(t, ref)
		assert.ErrorIs(t, err, putErr)
	})

	t.Run("CR Create failure is returned, not swallowed", func(t *testing.T) {
		createErr := errors.New("boom: create refused")
		c := fake.NewClientBuilder().WithScheme(scheme).
			WithInterceptorFuncs(interceptor.Funcs{
				Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
					return createErr
				},
			}).
			Build()
		deps := screenshotStoreDeps{
			Client:     c,
			Artifacts:  uiResourceTestArtifactsService(),
			SessionUID: types.UID("sess-uid-1"),
		}
		small := bytes.Repeat([]byte{0x01}, 16)

		ref, err := storeScreenshotArtifact(context.Background(), deps, sess, small)
		require.Error(t, err)
		assert.Nil(t, ref)
	})

	t.Run("Ready render is finalized so the artifact head resolves in memory", func(t *testing.T) {
		// Task #54: the chat UI's <img src="/artifact-download?artifactId=..."> 403'd
		// because the screenshot's artifactID had never been finalized — no
		// memory artifact head, no artifact#parent SpiceDB relationship. This
		// pins the fix at the layer this package owns: once
		// storeScreenshotArtifact returns, artifacts.Service.GetHead must find
		// the head FinalizeRevision wrote (the same Put that
		// pkg/memory/spicedbauthorizer.Authorizer hooks to write
		// artifact#parent in production).
		c := fake.NewClientBuilder().WithScheme(scheme).
			WithInterceptorFuncs(readyOnCreateInterceptor()).Build()
		svc := uiResourceTestArtifactsService()
		deps := screenshotStoreDeps{
			Client:     c,
			Artifacts:  svc,
			SessionUID: types.UID("sess-uid-1"),
		}
		small := bytes.Repeat([]byte{0x05}, 16)

		ref, err := storeScreenshotArtifact(context.Background(), deps, sess, small)
		require.NoError(t, err)
		require.NotNil(t, ref)

		scope := memory.Scope{Kind: "session", ID: sess.Namespace + "/" + sess.Name}
		head, found, err := svc.GetHead(memory.WithSystemApproval(context.Background(), "test"), scope, ref.ArtifactID)
		require.NoError(t, err)
		assert.True(t, found, "FinalizeRevision must have Put the artifact head — without it the artifact is unresolvable and unviewable")
		assert.Equal(t, 1, head.RevisionCount)
	})

	t.Run("missing Artifacts service fails closed rather than minting an unviewable ref", func(t *testing.T) {
		c := fake.NewClientBuilder().WithScheme(scheme).
			WithInterceptorFuncs(readyOnCreateInterceptor()).Build()
		deps := screenshotStoreDeps{
			Client:     c,
			SessionUID: types.UID("sess-uid-1"),
			// Artifacts intentionally left nil.
		}
		small := bytes.Repeat([]byte{0x03}, 16)

		ref, err := storeScreenshotArtifact(context.Background(), deps, sess, small)
		require.Error(t, err, "a nil Artifacts service must fail closed, never mint a ref FinalizeRevision never ran for")
		assert.Nil(t, ref)
	})

	t.Run("deterministic name/id seams are honored", func(t *testing.T) {
		c := fake.NewClientBuilder().WithScheme(scheme).
			WithInterceptorFuncs(readyOnCreateInterceptor()).Build()
		deps := screenshotStoreDeps{
			Client:        c,
			ArtifactStore: &fakeScreenshotPutter{},
			Artifacts:     uiResourceTestArtifactsService(),
			SessionUID:    types.UID("sess-uid-1"),
			NewRenderName: func(session string) string { return "ar-" + session + "-fixed" },
			NewArtifactID: func() string { return "artifact-fixed" },
		}
		small := bytes.Repeat([]byte{0x02}, 16)

		ref, err := storeScreenshotArtifact(context.Background(), deps, sess, small)
		require.NoError(t, err)
		assert.Equal(t, "ar-s1-fixed", ref.RenderName)
		assert.Equal(t, "artifact-fixed", ref.ArtifactID)
	})
}

// sleepyCapturer is a BrowserScreenshotCapturer stub that blocks for delay
// before succeeding, unless the ctx it is given finishes first (in which case
// it returns ctx.Err()). It stands in for a cold real capture that takes real
// wall-clock time, so a test can prove whether the capture's ctx is tied to
// the CALLER's short-lived ctx (the bug) or the closure's own, decoupled
// deadline (the fix).
type sleepyCapturer struct {
	delay time.Duration
	png   []byte
}

func (s *sleepyCapturer) CaptureRedacted(ctx context.Context) ([]byte, error) {
	select {
	case <-time.After(s.delay):
		return s.png, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// TestCaptureBrowserScreenshotFn_SurvivesCallerCtxShortDeadline pins the
// Task #47 fix: the closure's capture must NOT inherit the caller's ctx
// deadline. A caller ctx whose deadline has ALREADY ELAPSED by the time the
// closure runs (far tighter than any real turn/dispatch deadline would be,
// but the clearest possible proof) stands in for the live-observed bug — a
// capture inheriting it would see an already-Done ctx and fail instantly with
// context.DeadlineExceeded, exactly as the runner logs on a real cluster. The
// fix gives the capture its own fresh budget rooted at context.Background(),
// so a capturer that merely takes real time (well under its own budget)
// still succeeds.
func TestCaptureBrowserScreenshotFn_SurvivesCallerCtxShortDeadline(t *testing.T) {
	scheme := browserScreenshotTestScheme(t)
	l := &Loop{
		Client: fake.NewClientBuilder().WithScheme(scheme).
			WithInterceptorFuncs(readyOnCreateInterceptor()).Build(),
		Artifacts: uiResourceTestArtifactsService(),
	}
	sess := channelevents.SessionRef{Namespace: "ns", Name: "s1"}
	cap := &sleepyCapturer{delay: 50 * time.Millisecond, png: bytes.Repeat([]byte{0xAA}, 32)}
	fn := l.captureBrowserScreenshotFn(sess, cap)

	callerCtx, cancel := context.WithTimeout(context.Background(), 1*time.Millisecond)
	defer cancel()
	time.Sleep(5 * time.Millisecond) // guarantee callerCtx's deadline has elapsed before fn runs

	ref := fn(callerCtx)
	require.NotNil(t, ref, "the capture must survive a caller ctx whose deadline already elapsed — it must run on its own decoupled deadline, not the caller's")
}

// TestCaptureBrowserScreenshotFn_PropagatesCallerCancellation proves the OTHER
// half of the contract: decoupling the DEADLINE must not stop a genuine
// caller CANCELLATION (the turn aborted, the session ended) from tearing the
// capture down. A capturer that would otherwise run far longer than any
// caller deadline (5s) must still observe cancellation and return promptly
// once the caller's ctx is explicitly Cancel()'d.
func TestCaptureBrowserScreenshotFn_PropagatesCallerCancellation(t *testing.T) {
	scheme := browserScreenshotTestScheme(t)
	l := &Loop{Client: fake.NewClientBuilder().WithScheme(scheme).Build()}
	sess := channelevents.SessionRef{Namespace: "ns", Name: "s1"}
	cap := &sleepyCapturer{delay: 5 * time.Second} // long enough that only cancellation can end it

	fn := l.captureBrowserScreenshotFn(sess, cap)
	callerCtx, cancel := context.WithCancel(context.Background())

	resultCh := make(chan *channelevents.AttachmentRef, 1)
	go func() { resultCh <- fn(callerCtx) }()

	time.Sleep(10 * time.Millisecond) // let fn start and park on the capturer
	cancel()                          // explicit interrupt, not a deadline

	select {
	case ref := <-resultCh:
		assert.Nil(t, ref, "an explicit caller cancellation must abort the best-effort capture (nil, logged) rather than let it run to its own full budget")
	case <-time.After(2 * time.Second):
		t.Fatal("capture did not observe the caller's cancellation within 2s")
	}
}

// flakyCapturer fails with err on its first failTimes calls, then returns png
// on every call after — standing in for the Task #53 residual symptom: an
// intermittent "context deadline exceeded" that a second attempt against the
// same budget usually clears (a contended mutex that cleared, a CDP round
// trip that was merely slow). calls counts every invocation so a test can
// assert exactly how many attempts the closure made.
type flakyCapturer struct {
	failTimes int
	err       error
	png       []byte
	calls     int
}

func (f *flakyCapturer) CaptureRedacted(ctx context.Context) ([]byte, error) {
	f.calls++
	if f.calls <= f.failTimes {
		return nil, f.err
	}
	return f.png, nil
}

// TestCaptureBrowserScreenshotFn_RetriesOnceOnTimeout pins the Task #53
// robustness fix: a single timeout-shaped failure (context.DeadlineExceeded)
// must be retried exactly once, and a second attempt that succeeds must make
// the closure return a ref — not give up after the first failure the way #47
// left it.
func TestCaptureBrowserScreenshotFn_RetriesOnceOnTimeout(t *testing.T) {
	scheme := browserScreenshotTestScheme(t)
	l := &Loop{
		Client: fake.NewClientBuilder().WithScheme(scheme).
			WithInterceptorFuncs(readyOnCreateInterceptor()).Build(),
		Artifacts: uiResourceTestArtifactsService(),
	}
	sess := channelevents.SessionRef{Namespace: "ns", Name: "s1"}
	cap := &flakyCapturer{failTimes: 1, err: context.DeadlineExceeded, png: bytes.Repeat([]byte{0xAA}, 32)}
	fn := l.captureBrowserScreenshotFn(sess, cap)

	ref := fn(context.Background())
	require.NotNil(t, ref, "a single timeout-shaped failure must be retried once and succeed")
	assert.Equal(t, 2, cap.calls, "exactly one retry: two total attempts, not an unbounded loop")
}

// TestCaptureBrowserScreenshotFn_DoesNotRetryNonTimeoutError proves the retry
// is scoped to the Task #53 timeout symptom specifically: a fail-closed
// redaction error (a blanked field whose box genuinely cannot be resolved) is
// NOT a timeout and must not be masked by a retry that happens to succeed on
// a since-changed page.
func TestCaptureBrowserScreenshotFn_DoesNotRetryNonTimeoutError(t *testing.T) {
	scheme := browserScreenshotTestScheme(t)
	l := &Loop{Client: fake.NewClientBuilder().WithScheme(scheme).Build()}
	sess := channelevents.SessionRef{Namespace: "ns", Name: "s1"}
	cap := &flakyCapturer{failTimes: 1, err: errors.New("redact: empty box for blanked node 7"), png: bytes.Repeat([]byte{0xAA}, 32)}
	fn := l.captureBrowserScreenshotFn(sess, cap)

	ref := fn(context.Background())
	assert.Nil(t, ref, "a non-timeout fail-closed error must not be retried into a false success")
	assert.Equal(t, 1, cap.calls, "no retry for a non-timeout error")
}
