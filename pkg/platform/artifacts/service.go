package artifacts

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/memory"
	memartifact "github.com/authzed/openagentprimitives/pkg/memory/kinds/artifact"
	memrev "github.com/authzed/openagentprimitives/pkg/memory/kinds/artifactrevision"
)

// Service is the cross-cutting artifact-versioning accessor over a
// memory.Memory.
type Service struct {
	mem   memory.Memory
	newID func() string
	// newRenderName and newRevisionID are the other two id seams; see
	// WithRenderNameMinter and WithRevisionIDMinter. nil is production for
	// both, applied at NewRenderName / revisionIDFor rather than here so a
	// bare &Service{} behaves identically to a constructed one.
	newRenderName func(session string) string
	newRevisionID func(uid string) string

	// now is the clock seam; see WithClock. nil is production (time.Now),
	// resolved at nowUTC rather than here so a bare &Service{} behaves
	// identically to a constructed one. It exists so a deterministic fixture
	// (and a replay) can pin the CreatedAt the model reads back.
	now func() time.Time

	mu     sync.Mutex             // guards headMu
	headMu map[string]*sync.Mutex // per-(scope, head) finalize serialization
}

// Option adjusts a Service at construction.
//
// Variadic rather than more constructor parameters because every one of them is
// an id seam only a REPLAY sets — six of the seven construction sites in this
// repo pass nothing at all — and a growing positional signature would make each
// of those sites spell out two more nils it does not care about.
type Option func(*Service)

// WithRenderNameMinter overrides how the name of a new ArtifactRender CR is
// minted. nil is production (artifacts.RenderName).
//
// The seam exists for the reason NewArtifactID's does: the name is handed BACK
// to the model as artifact_prepare's `handle`, so a whole-session replay that
// re-mints it diverges from the recorded result on a value the run itself
// chose. Injected HERE rather than at each meta tool, because both tools that
// create a render CR share this one Service, and a package-level hook would be
// global mutable state that parallel tests could not hold apart.
//
// Not reachable from a tool argument, same as NewArtifactID: a name the model
// could choose is a name it could point at another session's render.
func WithRenderNameMinter(fn func(session string) string) Option {
	return func(s *Service) { s.newRenderName = fn }
}

// WithRevisionIDMinter overrides how a revision id is derived from the render
// CR's UID. nil is production (sha256 of the UID, see revisionIDFor).
//
// # The contract fn MUST honour
//
// fn is a FUNCTION OF THE UID and is called every time an id is needed, never
// once per revision: equal uid MUST yield an equal id, for the life of the
// process. That is not a style preference — it is what makes FinalizeRevision
// idempotent across the prepare->await handoff, which is a designed flow rather
// than an error path (artifact_prepare returns `pending` and TELLS the agent to
// call artifact_await, which re-finalizes the SAME CR). A fn that handed back a
// different id on the second call would mint a SECOND revision of one render:
// the head's RevisionCount would double-count, `latest` would point at a
// revision holding the same bytes as the one before it, and the handle the
// agent was given would resolve to neither.
//
// The signature is what carries the contract. It takes the uid precisely so an
// implementation has the key it must be stable across; a bare func() string
// could not be written correctly at all. bt.MintedIDSequence.KeyedMinter is the
// replay-side implementation, and it memoizes per key for exactly this reason.
func WithRevisionIDMinter(fn func(uid string) string) Option {
	return func(s *Service) { s.newRevisionID = fn }
}

// WithClock overrides the clock a revision's and head's CreatedAt are stamped
// from. nil is production (time.Now, resolved in nowUTC).
//
// The seam exists because CreatedAt is written into the entry the model reads
// back through artifact_history, so a deterministic fixture — or a whole-session
// replay — that leaves it on the wall clock produces bytes that differ run to
// run whenever two runs straddle a clock tick. That is a property of the clock,
// not of the tool. It is injected HERE, at the one component that stamps both
// the revision and the head, for the same reason the id seams are: a
// package-level hook on time.Now would be global mutable state that parallel
// tests could not hold apart.
func WithClock(fn func() time.Time) Option {
	return func(s *Service) { s.now = fn }
}

// NewService constructs the artifact service.
//
// newID defaults to minting from memory.NewID if nil, which is what every
// binary passes. The seam exists so a whole-session REPLAY can hand back the
// very artifact ids the captured run returned to the model, keeping the
// recorded arguments literal and correct. It is injected HERE, at the component
// that mints, rather than at memory.NewID: that function is called from every
// kind's accessor, so a hook on it would be global mutable state and would not
// survive parallel tests.
//
// It is deliberately not reachable from a tool argument — an id the model could
// choose is an id it could point at another session's artifact.
//
// The nil default is applied in NewArtifactID rather than here, so that the ONE
// place deciding what nil means also covers a Service built as a bare
// &Service{} — the same construction lockForHead already tolerates.
func NewService(mem memory.Memory, newID func() string, opts ...Option) *Service {
	s := &Service{mem: mem, newID: newID, headMu: map[string]*sync.Mutex{}}
	for _, o := range opts {
		o(s)
	}
	return s
}

// nowUTC is the clock the CreatedAt stamps read. nil means production
// (time.Now), resolved here so a bare &Service{} behaves like a constructed
// one — the same nil-default-at-use-site the id seams follow.
func (s *Service) nowUTC() time.Time {
	if s.now != nil {
		return s.now().UTC()
	}
	return time.Now().UTC()
}

// lockForHead returns the mutex serializing FinalizeRevision for one logical
// artifact, creating it on first use. Keyed by (scope, headID) so two revisions
// of DIFFERENT artifacts — the common parallel case — never wait on each other;
// only two revisions of the SAME head do, which is the whole point.
//
// The map only ever grows, bounded by the number of distinct artifact heads this
// process finalizes: a handful per session in the runner, which is the only
// process that finalizes at all. Reference-counting entries to delete them would
// cost more complexity than the memory it reclaims.
func (s *Service) lockForHead(scope memory.Scope, headID string) *sync.Mutex {
	key := scope.Kind + "\x00" + scope.ID + "\x00" + headID
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.headMu == nil {
		// A Service built as a bare &Service{} (not via NewService) still works.
		s.headMu = map[string]*sync.Mutex{}
	}
	lk, ok := s.headMu[key]
	if !ok {
		lk = &sync.Mutex{}
		s.headMu[key] = lk
	}
	return lk
}

// NewArtifactID mints a fresh logical-artifact head ID. Call this BEFORE
// creating the ArtifactRender CR so the CR can carry it in LabelArtifactID.
//
// This is the single place a nil newID means "production", which is why
// NewService just stores what it is handed: one definition covers every
// construction path, including a bare &Service{} — the same shape lockForHead
// already tolerates.
func (s *Service) NewArtifactID() string {
	if s.newID == nil {
		return memory.NewID(memartifact.Kind{})
	}
	return s.newID()
}

// RevisionResult is the finalized-revision summary the tools return.
type RevisionResult struct {
	RevisionID string
	ArtifactID string
	Seq        int
	Tags       []string // tags now pointing at this revision (incl. "latest")
	memrev.Revision
}

// revisionIDFor derives the deterministic revision ID from the CR UID so
// FinalizeRevision is idempotent across the prepare->await handoff.
//
// Recomputed on EVERY finalize rather than stored, which is what makes the
// idempotence unconditional: there is no remembered value to lose when a runner
// is killed between the two Puts, and the repairing artifact_await derives the
// same id from the same CR with nothing carried between them.
//
// The seam preserves that property rather than working around it — see
// WithRevisionIDMinter, whose contract is that it too is a function of the uid.
// nil is production.
func (s *Service) revisionIDFor(cr *spiceboxv1alpha1.ArtifactRender) string {
	if s.newRevisionID != nil {
		return s.newRevisionID(string(cr.UID))
	}
	return revisionIDFromUID(string(cr.UID))
}

// revisionIDFromUID is the production derivation: the id IS a hash of the uid,
// so it is idempotent by construction and holds no state at all.
func revisionIDFromUID(uid string) string {
	sum := sha256.Sum256([]byte(uid))
	return memrev.IDPrefix + hex.EncodeToString(sum[:8])
}

// FinalizeRevision records the just-rendered CR as a revision in memory: the
// immutable revision is Put, then the head is converged onto it (RevisionCount,
// latest + applied tags).
//
// It is idempotent AND convergent — different properties, both load-bearing.
// The two Puts share no transaction and no compare-and-set, so a runner killed
// between them (SIGTERM / OOM / eviction), or a head Put that errors, leaves
// the revision durable (RevisionTree reads the revision_of link, not the head)
// while the head still counts one fewer revision and still tags the PREVIOUS
// one as latest. A reader sees that half-finalized state directly:
// artifact-…#latest resolves to the older revision.
//
// That window is not transient, because the retry IS the designed flow:
// artifact_prepare tells the agent to call artifact_await, which re-finalizes
// the SAME CR (same UID ⇒ same revisionIDFor). The second finalize must
// therefore REPAIR the head rather than early-return over it — an early return
// reports "ready" with empty tags and leaves the artifact permanently
// unreachable by handle. So: always read both, Put the revision only when
// absent, and merge into the head every time, writing the head only when the
// merge changed it.
//
// Concurrency: the body is a read-modify-write of one head and memory.Entry
// carries no version, so there is no compare-and-set to make it atomic. It is
// serialized per (scope, head) via lockForHead, which suffices because this is
// the ONLY writer of the artifact head Kind anywhere and per session runs in
// exactly one process (the runner, through the single *artifacts.Service its
// loop and every artifact tool share); webd, `oap`, and the memory bundle
// handler only read. Without the lock, two artifact_prepare(revises:) calls in
// one assistant message (dispatchToolUses runs a turn's tool_uses on separate
// goroutines) both read RevisionCount=N, both mint seq=N+1, and the later head
// Put overwrites the earlier's applied tags — a revision vanishes from the head
// and its handle stops resolving.
//
// The lock is deliberately held across both durable Puts: read and write must
// commit as one unit, and two revisions of one artifact are inherently serial.
// Different heads use different mutexes, so an unrelated render never waits.
func (s *Service) FinalizeRevision(ctx context.Context, scope memory.Scope, cr *spiceboxv1alpha1.ArtifactRender) (RevisionResult, error) {
	headID := cr.Labels[LabelArtifactID]
	if headID == "" {
		return RevisionResult{}, fmt.Errorf("artifacts: CR %q missing %s label", cr.Name, LabelArtifactID)
	}
	revID := s.revisionIDFor(cr)

	lk := s.lockForHead(scope, headID)
	lk.Lock()
	defer lk.Unlock()

	rev, revExists, err := s.getRevision(ctx, scope, revID)
	if err != nil {
		return RevisionResult{}, err
	}
	head, headFound, err := s.GetHead(ctx, scope, headID)
	if err != nil {
		return RevisionResult{}, err
	}
	if !headFound {
		// Either a genuinely new artifact, or a torn finalize whose head Put
		// never landed. Both want the head built from the CR's intent; the
		// merge below decides the count and the tags.
		head = memartifact.Artifact{
			Name:         cr.Annotations[AnnoArtifactName],
			Description:  cr.Annotations[AnnoArtifactDescription],
			RendererKind: cr.Spec.Kind,
			Internal:     cr.Annotations[AnnoInternal] == "true",
		}
	}
	if head.Tags == nil {
		head.Tags = map[string]string{}
	}

	if !revExists {
		rev = memrev.Revision{
			Seq:               head.RevisionCount + 1,
			ChangeDescription: cr.Annotations[AnnoChangeDescription],
			RenderName:        cr.Name,
			OutputRef:         cr.Status.OutputRef,
			MIME:              cr.Status.OutputMIME,
			Size:              cr.Status.OutputSize,
			Filename:          cr.Status.OutputFilename,
			Warnings:          cr.Status.Warnings,
			CSP:               cr.Spec.CSP,
		}
		raw, merr := json.Marshal(rev)
		if merr != nil {
			return RevisionResult{}, fmt.Errorf("artifacts: marshal revision: %w", merr)
		}
		links := []memory.Link{
			{Relation: "revision_of", Kind: memartifact.Kind{}.Name(), ID: headID},
			{Relation: "of_render", Kind: "artifactrender", ID: cr.Name},
		}
		if parent := cr.Annotations[AnnoParentRevision]; parent != "" {
			links = append(links, memory.Link{Relation: "parent", Kind: memrev.Kind{}.Name(), ID: parent})
		}
		if pof := cr.Annotations[AnnoPreviewOf]; pof != "" {
			links = append(links, memory.Link{Relation: "preview_of", Kind: memartifact.Kind{}.Name(), ID: pof})
		}
		if prof := cr.Annotations[AnnoPreviewRevisionOf]; prof != "" {
			links = append(links, memory.Link{Relation: "preview_revision_of", Kind: memrev.Kind{}.Name(), ID: prof})
		}
		if _, perr := s.mem.Put(ctx, memory.Entry{
			Scope: scope, Kind: memrev.Kind{}.Name(), ID: revID,
			CreatedAt: s.nowUTC(), Links: links, Content: raw,
		}); perr != nil {
			return RevisionResult{}, fmt.Errorf("artifacts: put revision: %w", perr)
		}
	}

	if mergeRevisionIntoHead(&head, revID, rev.Seq, splitTags(cr.Annotations[AnnoAppliedTags])) || !headFound {
		hraw, merr := json.Marshal(head)
		if merr != nil {
			return RevisionResult{}, fmt.Errorf("artifacts: marshal head: %w", merr)
		}
		if _, perr := s.mem.Put(ctx, memory.Entry{
			Scope: scope, Kind: memartifact.Kind{}.Name(), ID: headID,
			CreatedAt: s.nowUTC(), Content: hraw,
		}); perr != nil {
			return RevisionResult{}, fmt.Errorf("artifacts: put head: %w", perr)
		}
	}

	return RevisionResult{RevisionID: revID, ArtifactID: headID, Seq: rev.Seq, Tags: tagsPointingAt(head, revID), Revision: rev}, nil
}

// mergeRevisionIntoHead converges head onto the (revID, seq) revision plus the
// CR's applied tags, reporting whether anything changed.
//
// Both rules are monotone in seq, so a LATE repair of an older torn revision
// can never move the head backwards:
//
//   - RevisionCount only ever rises, to the highest seq the head has seen;
//   - a tag moves to this revision only when its seq is strictly greater than
//     that highest seq — so the first writer at a given seq keeps the tag, and
//     a repair arriving after a newer revision finalized leaves latest alone.
//
// Comparing against RevisionCount is what keeps this read-free: RevisionCount
// IS the highest seq the head knows about, so seq <= it means some revision at
// least as new already owns the tags, with no need to load the tagged revision
// to compare.
func mergeRevisionIntoHead(head *memartifact.Artifact, revID string, seq int, applied []string) bool {
	if seq <= head.RevisionCount {
		// A revision at least as new already owns the count and the tags: this
		// is either the no-op re-finalize of the newest revision, or the late
		// repair of an older torn one. Neither may write.
		return false
	}
	head.RevisionCount = seq
	if head.Tags == nil {
		head.Tags = map[string]string{}
	}
	head.Tags[TagLatest] = revID
	for _, t := range applied {
		head.Tags[t] = revID
	}
	return true
}

// GetHead loads a logical-artifact head by ID.
func (s *Service) GetHead(ctx context.Context, scope memory.Scope, headID string) (memartifact.Artifact, bool, error) {
	res, err := s.mem.Query(ctx, memory.Query{Scope: scope, Kinds: []string{memartifact.Kind{}.Name()}, IDs: []string{headID}})
	if err != nil {
		return memartifact.Artifact{}, false, fmt.Errorf("artifacts: get head: %w", err)
	}
	if len(res.Entries) == 0 {
		return memartifact.Artifact{}, false, nil
	}
	var a memartifact.Artifact
	if err := json.Unmarshal(res.Entries[0].Content, &a); err != nil {
		return memartifact.Artifact{}, false, fmt.Errorf("artifacts: decode head: %w", err)
	}
	return a, true, nil
}

// GetRevision loads a revision by ID. ok=false when absent.
func (s *Service) GetRevision(ctx context.Context, scope memory.Scope, revID string) (memrev.Revision, bool, error) {
	return s.getRevision(ctx, scope, revID)
}

func (s *Service) getRevision(ctx context.Context, scope memory.Scope, revID string) (memrev.Revision, bool, error) {
	res, err := s.mem.Query(ctx, memory.Query{Scope: scope, Kinds: []string{memrev.Kind{}.Name()}, IDs: []string{revID}})
	if err != nil {
		return memrev.Revision{}, false, fmt.Errorf("artifacts: get revision: %w", err)
	}
	if len(res.Entries) == 0 {
		return memrev.Revision{}, false, nil
	}
	var r memrev.Revision
	if err := json.Unmarshal(res.Entries[0].Content, &r); err != nil {
		return memrev.Revision{}, false, fmt.Errorf("artifacts: decode revision: %w", err)
	}
	return r, true, nil
}

func splitTags(csv string) []string {
	if csv == "" {
		return nil
	}
	parts := strings.Split(csv, ",")
	out := parts[:0]
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// tagsPointingAt returns the tag names currently resolving to revID, in
// ascending name order.
//
// # Why the sort is load-bearing
//
// head.Tags is a Go map, and Go randomizes map iteration, so without this the
// returned slice came out in a different order on every call. That list is not
// an internal detail: it is RevisionResult.Tags, which artifact_prepare and
// artifact_await hand to the model, and RevisionView.Tags, which artifact_history
// hands to the model. Two identical artifact operations therefore returned two
// different results — invisible until a captured session was replayed and the
// only difference in the whole result was `["latest","<tag>"]` against
// `["<tag>","latest"]`.
//
// # Why ascending name rather than "latest" first
//
// The order is arbitrary-but-stable by design, not semantic. Nothing reads a
// position: the model is shown a set, and every lookup of the "latest" tag in
// this package goes through head.Tags[TagLatest] by NAME. Privileging one tag
// would be a rule a future reader has to know and re-apply the day a second
// system-applied tag appears — and an ordering rule nobody encoded is precisely
// the defect class this fixes. A total order over the values themselves needs
// no maintenance. Same discipline as channelassets.SortWarnings.
func tagsPointingAt(head memartifact.Artifact, revID string) []string {
	var out []string
	for name, target := range head.Tags {
		if target == revID {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}

// PreviewChildID returns the deterministic head ID of the internal preview
// artifact for a given source revision. Stable across calls so a re-offer
// reuses the same child head. It is a valid artifact head ID.
func (s *Service) PreviewChildID(sourceRevID string) string {
	sum := sha256.Sum256([]byte("preview:" + sourceRevID))
	return memartifact.IDPrefix + hex.EncodeToString(sum[:8])
}

// GetPreviewChild returns the RenderName of the latest revision of the preview
// child for sourceRevID, and ok=false when no preview has been generated yet
// (the live-viewer shows "generating preview…" in that case).
func (s *Service) GetPreviewChild(ctx context.Context, scope memory.Scope, sourceRevID string) (renderName string, ok bool, err error) {
	childID := s.PreviewChildID(sourceRevID)
	head, found, err := s.GetHead(ctx, scope, childID)
	if err != nil || !found {
		return "", false, err
	}
	latestRev := head.Tags[TagLatest]
	if latestRev == "" {
		return "", false, nil
	}
	rev, ok, err := s.getRevision(ctx, scope, latestRev)
	if err != nil || !ok {
		return "", false, err
	}
	return rev.RenderName, true, nil
}
