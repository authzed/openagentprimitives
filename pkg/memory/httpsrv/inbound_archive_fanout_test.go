package httpsrv

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/authzed/openagentprimitives/pkg/platform/artifactstore"
	blobstore "github.com/authzed/openagentprimitives/pkg/platform/artifactstore/blob"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/extract"
)

// fakeExploder serves a scripted member list, so the fan-out's own behavior is
// under test rather than the zip backend's (which has its own suite).
type fakeExploder struct {
	// gotLimits records what the fan-out resolved and passed down, which is
	// the only place a per-class bound can be observed on this side.
	gotLimits extract.Limits
	members   []ExplodedMember
	summary   ExplodeSummary
	err       error
}

func (f *fakeExploder) Explode(_ context.Context, _ string, _ io.Reader, lim extract.Limits, yield func(ExplodedMember) error) (ExplodeSummary, error) {
	f.gotLimits = lim
	for _, m := range f.members {
		if err := yield(m); err != nil {
			return f.summary, err
		}
	}
	return f.summary, f.err
}

// fakeExtractor mirrors the REAL registry's dispatch: it claims text-shaped
// MIMEs and returns ErrAttachmentMIMEUnsupported for everything else.
//
// A fake that answered every MIME would hide whether members are dispatched
// by type at all — an image would come back with extracted text and the test
// would still be green, which is the whole class of bug a permissive fake
// creates.
type fakeExtractor struct {
	text string
	err  error
}

func (f fakeExtractor) Extract(_ context.Context, mime string, _ io.Reader) (string, int, error) {
	if f.err != nil {
		return "", 0, f.err
	}
	if !strings.HasPrefix(mime, "text/") && mime != "application/pdf" {
		return "", 0, ErrAttachmentMIMEUnsupported
	}
	return f.text, 0, nil
}

func member(name, mime, body string, isArchive bool) ExplodedMember {
	return ExplodedMember{
		Name: name, MIME: mime, Size: int64(len(body)),
		Body: strings.NewReader(body), IsArchive: isArchive,
	}
}

// The core claim: one upload becomes N stored members plus an index that IS
// the archive's extracted text, so the archive takes the ordinary read path.
func TestExplodeStoresEveryMemberAndIndexesThem(t *testing.T) {
	h := newFanoutHandler(t,
		&fakeExploder{members: []ExplodedMember{
			member("logs/a.log", "text/plain", "alpha", false),
			member("shot.png", "image/png", "\x89PNG", false),
		}, summary: ExplodeSummary{Members: 2}},
		fakeExtractor{text: "extracted"})

	var resp inboundAssetResponse
	took := h.explodeInboundArchive(context.Background(), "ns", "sess", "aid",
		"application/zip", "bundle.zip", storeSomething(t, h), &resp)

	require.True(t, took, "an archive must be handled here, not by the single-file path")
	assert.True(t, resp.Extracted)
	assert.NotEmpty(t, resp.TextRef, "the index is the archive's extracted text")
	require.Len(t, resp.Members, 2)

	byName := map[string]memberResult{}
	for _, m := range resp.Members {
		byName[m.Name] = m
		assert.NotEmpty(t, m.Ref, "every member's bytes must be addressable")
	}
	assert.Equal(t, readText, byName["logs/a.log"].Readability)
	assert.Equal(t, readImage, byName["shot.png"].Readability)
	assert.Empty(t, byName["shot.png"].TextRef, "an image has no extracted text")
}

// Depth 0 across the seam: a nested archive is stored, classified, and never
// handed back to the exploder.
func TestNestedArchiveMemberIsClassifiedNotOpened(t *testing.T) {
	h := newFanoutHandler(t,
		&fakeExploder{members: []ExplodedMember{
			member("inner.zip", "application/zip", "PK\x03\x04", true),
		}, summary: ExplodeSummary{Members: 1}},
		fakeExtractor{text: "should never be used"})

	var resp inboundAssetResponse
	require.True(t, h.explodeInboundArchive(context.Background(), "ns", "sess", "aid",
		"application/zip", "bundle.zip", storeSomething(t, h), &resp))

	require.Len(t, resp.Members, 1)
	assert.Equal(t, readArchive, resp.Members[0].Readability)
	assert.Empty(t, resp.Members[0].TextRef,
		"a nested archive must not be extracted; it was never opened")
}

// A permanent refusal and a transient failure lead to opposite notice wording,
// so the fan-out must keep them apart.
func TestExplodeFailureModes(t *testing.T) {
	cases := []struct {
		name            string
		err             error
		wantTook        bool
		wantUnsupported bool
	}{
		{"no exploder claims the type: fall through to the single-file path", ErrArchiveMIMEUnsupported, false, false},
		{"refused (bomb or malformed): permanent, bytes still stored", ErrArchiveRefused, true, true},
		{"anything else: transient, never marked unsupported", errors.New("extractord unreachable"), true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newFanoutHandler(t, &fakeExploder{err: tc.err}, fakeExtractor{})

			var resp inboundAssetResponse
			took := h.explodeInboundArchive(context.Background(), "ns", "sess", "aid",
				"application/zip", "bundle.zip", storeSomething(t, h), &resp)

			assert.Equal(t, tc.wantTook, took)
			assert.Equal(t, tc.wantUnsupported, resp.Unsupported)
			assert.False(t, resp.Extracted, "nothing was extracted in any failure mode")
		})
	}
}

// 186 good files are still worth having.
func TestOneMemberFailingToStoreDoesNotFailTheArchive(t *testing.T) {
	h := newFanoutHandler(t,
		&fakeExploder{members: []ExplodedMember{
			member("good.log", "text/plain", "fine", false),
			{Name: "bad.log", MIME: "text/plain", Body: errReader{}},
			member("also-good.log", "text/plain", "fine too", false),
		}, summary: ExplodeSummary{Members: 3}},
		fakeExtractor{text: "x"})

	var resp inboundAssetResponse
	require.True(t, h.explodeInboundArchive(context.Background(), "ns", "sess", "aid",
		"application/zip", "bundle.zip", storeSomething(t, h), &resp))

	assert.True(t, resp.Extracted)
	assert.Len(t, resp.Members, 2, "the failing member is absent; the rest survive")
}

// Truncation must reach the caller, or a partial bundle is presented as whole.
func TestTruncationIsReportedToTheCaller(t *testing.T) {
	h := newFanoutHandler(t,
		&fakeExploder{
			members: []ExplodedMember{member("a.log", "text/plain", "x", false)},
			summary: ExplodeSummary{Members: 1, Truncated: true, TruncatedReason: "member count"},
		},
		fakeExtractor{text: "x"})

	var resp inboundAssetResponse
	require.True(t, h.explodeInboundArchive(context.Background(), "ns", "sess", "aid",
		"application/zip", "bundle.zip", storeSomething(t, h), &resp))

	assert.True(t, resp.ArchiveTruncated)
	assert.Equal(t, "member count", resp.ArchiveTruncatedReason)
}

// With no exploder wired the route must behave exactly as before.
func TestNoExploderConfiguredFallsThrough(t *testing.T) {
	h := newFanoutHandler(t, nil, fakeExtractor{text: "x"})
	var resp inboundAssetResponse
	assert.False(t, h.explodeInboundArchive(context.Background(), "ns", "sess", "aid",
		"application/zip", "bundle.zip", storeSomething(t, h), &resp))
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("member body unreadable") }

// newFanoutHandler builds a handler with just the two collaborators the
// fan-out uses: a real in-memory artifactstore (so member refs and index bytes
// are genuinely round-tripped) and scripted exploder/extractor.
//
// A nil exploder is passed as a true nil INTERFACE, never a typed-nil pointer:
// the whole reason WithAttachmentExploder carries a reflection guard is that
// the latter would defeat the handler's own nil check.
func newFanoutHandler(t *testing.T, exploder AttachmentExploder, extractor AttachmentExtractor) *handler {
	t.Helper()
	h := &handler{
		artifactStore:       blobstore.NewMem(),
		attachmentExtractor: extractor,
	}
	if exploder != nil {
		h.attachmentExploder = exploder
	}
	return h
}

// storeSomething puts the archive's own bytes in the store, so the fan-out's
// re-read (a fresh Get, never a second copy of the request body) has something
// to find.
func storeSomething(t *testing.T, h *handler) artifactstore.Ref {
	t.Helper()
	ref, err := h.artifactStore.Put(context.Background(), "ns/sess/inbound-asset/aid/bundle.zip",
		strings.NewReader("PK\x03\x04 pretend archive"))
	require.NoError(t, err)
	return ref
}

// classFixture builds a session and the class it names, so archiveLimitsFor
// has a real object graph to walk.
func classFixture(t *testing.T, archivesJSON string) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, spiceboxv1alpha1.AddToScheme(scheme))

	class := &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-agent", Namespace: "ns"},
	}
	if archivesJSON != "" {
		class.Spec.Capabilities = map[string]apiextensionsv1.JSON{
			"attachments": {Raw: []byte(archivesJSON)},
		}
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		&spiceboxv1alpha1.AgentSession{
			ObjectMeta: metav1.ObjectMeta{Name: "sess", Namespace: "ns"},
			Spec:       spiceboxv1alpha1.AgentSessionSpec{Class: "demo-agent"},
		},
		class,
	).Build()
}

// The end the whole task exists for: a bound written on an AgentClass reaches
// the component that opens the archive.
func TestClassTightenedBoundReachesTheExploder(t *testing.T) {
	cases := []struct {
		name         string
		archivesJSON string
		client       bool
		wantMembers  int
	}{
		{
			name:         "a tightened bound is applied",
			archivesJSON: `{"archives":{"maxMembers":8}}`,
			client:       true,
			wantMembers:  8,
		},
		{
			name:         "a bound ABOVE the ceiling clamps to it, never raising it",
			archivesJSON: `{"archives":{"maxMembers":100000}}`,
			client:       true,
			wantMembers:  extract.DefaultLimits.MaxMembers,
		},
		{
			name:         "no archives stanza: the deployment ceiling",
			archivesJSON: `{}`,
			client:       true,
			wantMembers:  extract.DefaultLimits.MaxMembers,
		},
		{
			name:         "a config that should not have been applied fails OPEN TO THE CEILING",
			archivesJSON: `{"archives":{"maxMembers":0}}`,
			client:       true,
			wantMembers:  extract.DefaultLimits.MaxMembers,
		},
		{
			name:        "no Kubernetes client: the ceiling, never something wider",
			client:      false,
			wantMembers: extract.DefaultLimits.MaxMembers,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ex := &fakeExploder{
				members: []ExplodedMember{member("a.log", "text/plain", "x", false)},
				summary: ExplodeSummary{Members: 1},
			}
			h := newFanoutHandler(t, ex, fakeExtractor{text: "x"})
			if tc.client {
				h.k8sClient = classFixture(t, tc.archivesJSON)
			}

			var resp inboundAssetResponse
			require.True(t, h.explodeInboundArchive(context.Background(), "ns", "sess", "aid",
				"application/zip", "bundle.zip", storeSomething(t, h), &resp))

			assert.Equal(t, tc.wantMembers, ex.gotLimits.MaxMembers)
			assert.LessOrEqual(t, ex.gotLimits.MaxMembers, extract.DefaultLimits.MaxMembers,
				"a class must never widen the deployment ceiling")
			assert.Positive(t, ex.gotLimits.MaxUncompressedTotal,
				"no path may produce a zero bound, which every check reads as unlimited")
		})
	}
}
