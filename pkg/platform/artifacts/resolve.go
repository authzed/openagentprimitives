package artifacts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/memory"
	memartifact "github.com/authzed/openagentprimitives/pkg/memory/kinds/artifact"
	memrev "github.com/authzed/openagentprimitives/pkg/memory/kinds/artifactrevision"
)

// ErrNotFound means the artifact handle or its head revision was not found in
// memory (e.g. the in-memory operator restarted and lost it). Callers can use
// errors.Is to distinguish a missing artifact (→ 404) from a backend/transport
// error (→ 500).
var ErrNotFound = errors.New("artifacts: artifact or revision not found")

// RevisionView is one node of an artifact's revision tree. JSON tags
// keep the agent-facing contract (artifact_history) and any future
// JSON consumer consistent with artifact_prepare's snake_case result.
type RevisionView struct {
	RevisionID string `json:"revision_id"`
	// Seq is this revision's 1-based position in the artifact's order; higher is newer.
	Seq               int    `json:"seq"`
	ChangeDescription string `json:"change_description,omitempty"`
	// ParentID is the revision this one revises; empty for the first revision.
	ParentID string `json:"parent_id,omitempty"`
	// RenderName is the ArtifactRender CR holding this revision's bytes.
	RenderName string `json:"render_name"`
	// OutputRef is the artifact-store key the rendered bytes live under.
	OutputRef string `json:"output_ref"`
	MIME      string `json:"mime"`
	// Size is the rendered output in bytes.
	Size     int64  `json:"size"`
	Filename string `json:"filename,omitempty"`
	// Tags are the tag names currently pointing at THIS revision (e.g. "latest").
	Tags []string `json:"tags,omitempty"`
	// CreatedAt is UTC, formatted "2006-01-02T15:04:05Z".
	CreatedAt string `json:"created_at"`
}

// ArtifactView is one logical artifact for listings.
type ArtifactView struct {
	ArtifactID  string `json:"artifact_id"`
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
	// RendererKind is the registered renderer producing this artifact's bytes.
	RendererKind string `json:"renderer_kind"`
	// RevisionCount is the highest revision seq the head has seen.
	RevisionCount int `json:"revision_count"`
	// Tags maps each tag name to the revision id it currently points at.
	Tags map[string]string `json:"tags,omitempty"`
	// CreatedAt is UTC, formatted "2006-01-02T15:04:05Z".
	CreatedAt string `json:"created_at"`
}

// ResolveToRender maps any handle form to the ArtifactRender CR name that
// holds its bytes. Callers then run the existing CR ownership + Ready
// checks against that name.
func (s *Service) ResolveToRender(ctx context.Context, scope memory.Scope, handle string) (string, error) {
	rev, err := s.resolveRevision(ctx, scope, handle)
	if err != nil {
		return "", err
	}
	if rev.RenderName == "" {
		return "", fmt.Errorf("artifacts: handle %q resolved to a revision with no render: %w", handle, ErrNotFound)
	}
	return rev.RenderName, nil
}

// ResolveWidgetCSP maps any handle form to the widget's declared CSP
// (nil when the widget declared none, or the resolved revision predates
// CSP persistence) — mirrors ResolveToRender, reusing the same
// resolveRevision lookup so both agree on which revision a handle names.
func (s *Service) ResolveWidgetCSP(ctx context.Context, scope memory.Scope, handle string) (*spiceboxv1alpha1.WidgetCSP, error) {
	rev, err := s.resolveRevision(ctx, scope, handle)
	if err != nil {
		return nil, err
	}
	return rev.CSP, nil
}

// ResolveRevisionTarget maps a revises handle to the (headID, parent
// revisionID, rendererKind) used to create a new revision.
func (s *Service) ResolveRevisionTarget(ctx context.Context, scope memory.Scope, handle string) (headID, parentRevID, rendererKind string, err error) {
	rev, head, headEntryID, err := s.resolveRevisionWithHead(ctx, scope, handle)
	if err != nil {
		return "", "", "", err
	}
	return headEntryID, rev.id, head.RendererKind, nil
}

type resolvedRev struct {
	id string
	memrev.Revision
}

func (s *Service) resolveRevision(ctx context.Context, scope memory.Scope, handle string) (resolvedRev, error) {
	rev, _, _, err := s.resolveRevisionWithHead(ctx, scope, handle)
	return rev, err
}

func (s *Service) resolveRevisionWithHead(ctx context.Context, scope memory.Scope, handle string) (resolvedRev, memartifact.Artifact, string, error) {
	// Tags live on head artifacts (artifact-ID#tag → the currently-tagged
	// revision). A `#tag` on an ALREADY revision-specific handle — a
	// revision id (artrev-…) or a render CR name (ar-…) — is redundant, so
	// strip it here rather than let it ride into an id/name lookup that would
	// then silently fail to match. This centralizes the strip so every caller
	// (live-view ResolveAssetURL, the ZIP bundler) agrees with what the
	// artifact-ref sanitizer advertises (`#tag` allowed on all three forms).
	// The artifact-… branch below KEEPS the tag: it needs it to select a rev.
	if !strings.HasPrefix(handle, memartifact.IDPrefix) {
		if i := strings.IndexByte(handle, '#'); i >= 0 {
			handle = handle[:i]
		}
	}
	switch {
	case strings.HasPrefix(handle, memartifact.IDPrefix): // artifact-… or artifact-…#tag
		headID := handle
		tag := TagLatest
		if i := strings.IndexByte(handle, '#'); i >= 0 {
			headID, tag = handle[:i], handle[i+1:]
		}
		head, ok, err := s.GetHead(ctx, scope, headID)
		if err != nil {
			return resolvedRev{}, memartifact.Artifact{}, "", err
		}
		if !ok {
			return resolvedRev{}, memartifact.Artifact{}, "", fmt.Errorf("artifacts: artifact %q not found in this session: %w", headID, ErrNotFound)
		}
		revID, ok := head.Tags[tag]
		if !ok || revID == "" {
			return resolvedRev{}, memartifact.Artifact{}, "", fmt.Errorf("artifacts: tag %q not found on artifact %q: %w", tag, headID, ErrNotFound)
		}
		rev, ok, err := s.getRevision(ctx, scope, revID)
		if err != nil || !ok {
			return resolvedRev{}, memartifact.Artifact{}, "", fmt.Errorf("artifacts: revision %q (tag %q) not found: %w", revID, tag, ErrNotFound)
		}
		return resolvedRev{id: revID, Revision: rev}, head, headID, nil

	case strings.HasPrefix(handle, memrev.IDPrefix): // artrev-…
		rev, ok, err := s.getRevision(ctx, scope, handle)
		if err != nil {
			return resolvedRev{}, memartifact.Artifact{}, "", err
		}
		if !ok {
			return resolvedRev{}, memartifact.Artifact{}, "", fmt.Errorf("artifacts: revision %q not found in this session: %w", handle, ErrNotFound)
		}
		headID, head, err := s.headForRevision(ctx, scope, handle)
		if err != nil {
			return resolvedRev{}, memartifact.Artifact{}, "", err
		}
		return resolvedRev{id: handle, Revision: rev}, head, headID, nil

	default: // ar-… CR name
		res, err := s.mem.Query(ctx, memory.Query{
			Scope: scope, Kinds: []string{memrev.Kind{}.Name()},
			LinkedTo: []memory.LinkFilter{{Relation: "of_render", Kind: "artifactrender", ID: handle}},
		})
		if err != nil {
			return resolvedRev{}, memartifact.Artifact{}, "", fmt.Errorf("artifacts: resolve render %q: %w", handle, err)
		}
		if len(res.Entries) == 0 {
			return resolvedRev{}, memartifact.Artifact{}, "", fmt.Errorf("artifacts: handle %q not found in this session: %w", handle, ErrNotFound)
		}
		var rev memrev.Revision
		if err := json.Unmarshal(res.Entries[0].Content, &rev); err != nil {
			return resolvedRev{}, memartifact.Artifact{}, "", fmt.Errorf("artifacts: decode revision: %w", err)
		}
		revID := res.Entries[0].ID
		headID, head, err := s.headForRevision(ctx, scope, revID)
		if err != nil {
			return resolvedRev{}, memartifact.Artifact{}, "", err
		}
		return resolvedRev{id: revID, Revision: rev}, head, headID, nil
	}
}

func (s *Service) headForRevision(ctx context.Context, scope memory.Scope, revID string) (string, memartifact.Artifact, error) {
	res, err := s.mem.Query(ctx, memory.Query{Scope: scope, Kinds: []string{memrev.Kind{}.Name()}, IDs: []string{revID}})
	if err != nil {
		return "", memartifact.Artifact{}, fmt.Errorf("artifacts: get revision %q: %w", revID, err)
	}
	if len(res.Entries) == 0 {
		return "", memartifact.Artifact{}, fmt.Errorf("artifacts: revision %q not found", revID)
	}
	headID := ""
	for _, l := range res.Entries[0].Links {
		if l.Relation == "revision_of" {
			headID = l.ID
		}
	}
	if headID == "" {
		return "", memartifact.Artifact{}, fmt.Errorf("artifacts: revision %q has no head link", revID)
	}
	head, ok, err := s.GetHead(ctx, scope, headID)
	if err != nil {
		return "", memartifact.Artifact{}, err
	}
	if !ok {
		return "", memartifact.Artifact{}, fmt.Errorf("artifacts: head %q not found", headID)
	}
	return headID, head, nil
}

// RevisionTree returns an artifact's revisions ordered by Seq ascending.
func (s *Service) RevisionTree(ctx context.Context, scope memory.Scope, headID string) ([]RevisionView, error) {
	head, ok, err := s.GetHead(ctx, scope, headID)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("artifacts: artifact %q not found", headID)
	}
	res, err := s.mem.Query(ctx, memory.Query{
		Scope: scope, Kinds: []string{memrev.Kind{}.Name()},
		LinkedTo: []memory.LinkFilter{{Relation: "revision_of", Kind: memartifact.Kind{}.Name(), ID: headID}},
	})
	if err != nil {
		return nil, fmt.Errorf("artifacts: list revisions: %w", err)
	}
	views := make([]RevisionView, 0, len(res.Entries))
	for _, e := range res.Entries {
		var rev memrev.Revision
		if err := json.Unmarshal(e.Content, &rev); err != nil {
			return nil, fmt.Errorf("artifacts: decode revision %q: %w", e.ID, err)
		}
		parent := ""
		for _, l := range e.Links {
			if l.Relation == "parent" {
				parent = l.ID
			}
		}
		views = append(views, RevisionView{
			RevisionID: e.ID, Seq: rev.Seq, ChangeDescription: rev.ChangeDescription,
			ParentID: parent, RenderName: rev.RenderName, OutputRef: rev.OutputRef,
			MIME: rev.MIME, Size: rev.Size, Filename: rev.Filename,
			Tags: tagsPointingAt(head, e.ID), CreatedAt: e.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
		})
	}
	sort.Slice(views, func(i, j int) bool { return views[i].Seq < views[j].Seq })
	return views, nil
}

// ListArtifacts returns every logical artifact in the session scope.
func (s *Service) ListArtifacts(ctx context.Context, scope memory.Scope) ([]ArtifactView, error) {
	res, err := s.mem.Query(ctx, memory.Query{Scope: scope, Kinds: []string{memartifact.Kind{}.Name()}})
	if err != nil {
		return nil, fmt.Errorf("artifacts: list artifacts: %w", err)
	}
	out := make([]ArtifactView, 0, len(res.Entries))
	for _, e := range res.Entries {
		var a memartifact.Artifact
		if err := json.Unmarshal(e.Content, &a); err != nil {
			return nil, fmt.Errorf("artifacts: decode artifact %q: %w", e.ID, err)
		}
		// Skip system-managed internal artifacts (e.g. bundled-only preview children).
		if a.Internal {
			continue
		}
		out = append(out, ArtifactView{
			ArtifactID: e.ID, Name: a.Name, Description: a.Description, RendererKind: a.RendererKind,
			RevisionCount: a.RevisionCount, Tags: a.Tags, CreatedAt: e.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
		})
	}
	// Ordered by creation, then by id. The id tiebreak is what makes this a
	// TOTAL order, and it is load-bearing: CreatedAt is formatted to second
	// granularity above, so two artifacts made in the same second carry equal
	// keys, and sort.Slice is not stable — equal keys would come back in an
	// arbitrary order that varies run to run. The model reads this list, so two
	// identical reads returning two different sequences is a production defect.
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt != out[j].CreatedAt {
			return out[i].CreatedAt < out[j].CreatedAt
		}
		return out[i].ArtifactID < out[j].ArtifactID
	})
	return out, nil
}
