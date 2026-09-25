package artifacts_test

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"

	bt "github.com/authzed/openagentprimitives/pkg/bronzethread"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/platform/artifacts"
)

// The two id seams a whole-session replay drives, and the property the second
// one must not cost: prepare->await idempotence.

func newSeamedSvc(t *testing.T, opts ...artifacts.Option) (*artifacts.Service, memory.Scope) {
	t.Helper()
	mem := memory.NewLocal(inmem.NewBackend())
	return artifacts.NewService(mem, nil, opts...), memory.Scope{Kind: "session", ID: "default/sess1"}
}

// TestSeams_NilIsProduction is the compatibility claim every seam on this
// service makes: a Service built the way all six production call sites build it
// behaves exactly as it did before the seams existed.
//
// Both halves matter and they fail differently. A render name that stopped
// being random would collide on the second render of a session; a revision id
// that stopped being the UID hash would break the prepare->await handoff for
// every real run, not just a replayed one.
func TestSeams_NilIsProduction(t *testing.T) {
	t.Run("no options at all: render names are minted, random and well-shaped", func(t *testing.T) {
		svc, _ := newSeamedSvc(t)
		a, b := svc.NewRenderName("demo-agent-1234"), svc.NewRenderName("demo-agent-1234")
		assert.NotEqual(t, a, b, "two renders of one session must not collide")
		for _, got := range []string{a, b} {
			assert.True(t, strings.HasPrefix(got, "ar-demo-agent-1234-"),
				"the CR name carries its session, which is what keeps it unique in the namespace")
			assert.True(t, artifacts.IsRenderName(got),
				"the format's own inverse must accept what the format produced: %q", got)
		}
	})

	t.Run("options passed nil: revision ids match a Service built with no options", func(t *testing.T) {
		// The wiring an e2e scenario that pinned nothing produces — the funcs
		// are nil FIELDS handed straight to the options, so the options are
		// always applied and nil has to fall through to production inside the
		// Service. If it did not, every bundle would silently take the replay
		// path, and every production binary would too the day one passed a
		// nil-valued option.
		ctx := memory.WithSystemApproval(context.Background(), "test")

		plain, scope := newSeamedSvc(t)
		nilOpts, _ := newSeamedSvc(t,
			artifacts.WithRenderNameMinter(nil),
			artifacts.WithRevisionIDMinter(nil))
		assert.True(t, artifacts.IsRenderName(nilOpts.NewRenderName("demo-agent-1234")),
			"a nil render-name minter still mints the production format")

		// Same UID into both services must derive the same revision id: the
		// production derivation is a pure function of it and holds no state.
		const uid = types.UID("uid-shared")
		a, err := plain.FinalizeRevision(ctx, scope, renderedCR("ar-a-aaaaaa", plain.NewArtifactID(), "", "x", "", uid))
		require.NoError(t, err)
		b, err := nilOpts.FinalizeRevision(ctx, scope, renderedCR("ar-b-bbbbbb", nilOpts.NewArtifactID(), "", "x", "", uid))
		require.NoError(t, err)
		assert.Equal(t, a.RevisionID, b.RevisionID,
			"a nil revision-id minter must be the same derivation, not merely some derivation")
	})
}

// TestSeams_RenderNameMinterIsUsed pins that the seam actually reaches the
// value, and that the recorded name is handed back ENTIRE rather than
// re-composed around a suffix.
func TestSeams_RenderNameMinterIsUsed(t *testing.T) {
	var askedFor []string
	svc, _ := newSeamedSvc(t, artifacts.WithRenderNameMinter(func(session string) string {
		askedFor = append(askedFor, session)
		return "ar-recorded-session-cd752b"
	}))

	assert.Equal(t, "ar-recorded-session-cd752b", svc.NewRenderName("live-session-9999"),
		"a pinned handle is returned whole: the capture recorded the CR name, session segment included")
	assert.Equal(t, []string{"live-session-9999"}, askedFor,
		"the seam is still told which session asked, so an implementation that needs it has it")
}

// TestSeams_RevisionIDDerivationSurvivesTheSeam is the load-bearing one.
//
// artifact_prepare finalizes a revision and artifact_await re-finalizes the
// SAME CR; the two agreeing on one id is what makes the second call a repair
// rather than a second revision. The production derivation gets that for free
// by being a pure function of the UID. A replay seam has to be given the same
// property deliberately, which is why the seam takes the uid as a KEY.
//
// Wired to the REAL replay implementation — bt.MintedIDSequence.KeyedMinter —
// rather than to a hand-rolled memo, because the defect this guards against
// lives in the JOIN between the two packages, and a test that stubbed one side
// would pass with the driver wired to the wrong minter.
func TestSeams_RevisionIDDerivationSurvivesTheSeam(t *testing.T) {
	seq := bt.NewMintedIDSequence(
		map[string][]string{bt.FamilyArtifactRevision: {"artrev-171fcb3ee98f70b3"}},
		func(msg string) { t.Errorf("%s", msg) })

	svc, scope := newSeamedSvc(t, artifacts.WithRevisionIDMinter(seq.KeyedMinter(bt.FamilyArtifactRevision)))
	ctx := memory.WithSystemApproval(context.Background(), "test")
	headID := svc.NewArtifactID()
	cr := renderedCR("ar-sess1-cd752b", headID, "", "first", "", types.UID("uid-replayed"))

	first, err := svc.FinalizeRevision(ctx, scope, cr)
	require.NoError(t, err, "artifact_prepare's finalize")
	second, err := svc.FinalizeRevision(ctx, scope, cr)
	require.NoError(t, err, "artifact_await's re-finalize of the same CR")

	assert.Equal(t, "artrev-171fcb3ee98f70b3", first.RevisionID,
		"the replay hands back the id the capture recorded, which is what the step's expectation names")
	assert.Equal(t, first.RevisionID, second.RevisionID,
		"same CR, same revision id — the property the production derivation has by construction")

	head, _, err := svc.GetHead(ctx, scope, headID)
	require.NoError(t, err)
	assert.Equal(t, 1, head.RevisionCount,
		"a second id would have finalized a SECOND revision of one render and double-counted the head")
	assert.Equal(t, first.RevisionID, head.Tags[artifacts.TagLatest],
		"and would have left latest pointing at a duplicate of the bytes before it")
	assert.Empty(t, seq.Unused(),
		"one render finalized twice draws ONE recorded id: the sequence counts revisions, not calls")
}

// TestSeams_RevisionIDMinterIsKeyedByUID pins the argument, not just the
// result: two DIFFERENT renders must not collapse onto one id, which is the
// mirror failure of the one above.
func TestSeams_RevisionIDMinterIsKeyedByUID(t *testing.T) {
	seq := bt.NewMintedIDSequence(
		map[string][]string{bt.FamilyArtifactRevision: {"artrev-1111111111111111", "artrev-2222222222222222"}},
		func(msg string) { t.Errorf("%s", msg) })

	svc, scope := newSeamedSvc(t, artifacts.WithRevisionIDMinter(seq.KeyedMinter(bt.FamilyArtifactRevision)))
	ctx := memory.WithSystemApproval(context.Background(), "test")
	headID := svc.NewArtifactID()

	one, err := svc.FinalizeRevision(ctx, scope, renderedCR("ar-sess1-aaaaaa", headID, "", "first", "", types.UID("uid-a")))
	require.NoError(t, err)
	two, err := svc.FinalizeRevision(ctx, scope, renderedCR("ar-sess1-bbbbbb", headID, one.RevisionID, "second", "", types.UID("uid-b")))
	require.NoError(t, err)

	assert.Equal(t, "artrev-1111111111111111", one.RevisionID)
	assert.Equal(t, "artrev-2222222222222222", two.RevisionID,
		"a second CR draws the NEXT recorded id: the key is the UID, so distinct renders stay distinct")

	head, _, err := svc.GetHead(ctx, scope, headID)
	require.NoError(t, err)
	assert.Equal(t, 2, head.RevisionCount, "two renders are two revisions")
}
