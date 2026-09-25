package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/permsurface"
)

// permissionSurfaceEntries projects the enumerated surface into the CRD shape
// published on AgentSession.status.
//
// Deliberately a pure, total function of the descriptors: this value lands in
// an applied status field, so anything volatile in it — input order, a
// timestamp, a map iteration — would rewrite the object on every session start
// and re-trigger every watcher. Sorting and deduplication here are what make a
// re-publish of an unchanged envelope a no-op diff.
//
// Note what is NOT projected: Permission and ResourceType, both recoverable by
// parsing the handle, and Provenance.Condition. The CEL condition is
// deliberately dropped — it says WHEN a tool reaches a handle, and a surface
// answers WHETHER. Listing conditions here would invite a reader to treat the
// snapshot as the dispatch rule, which is exactly the confusion the
// observation-not-authority line exists to prevent.
func permissionSurfaceEntries(surface []permsurface.Descriptor) []spiceboxv1alpha1.PermissionSurfaceEntry {
	if len(surface) == 0 {
		return nil
	}
	out := make([]spiceboxv1alpha1.PermissionSurfaceEntry, 0, len(surface))
	for _, d := range surface {
		tools := make([]string, 0, len(d.Via))
		for _, v := range d.Via {
			if v.Tool != "" && !slices.Contains(tools, v.Tool) {
				tools = append(tools, v.Tool)
			}
		}
		slices.Sort(tools)
		out = append(out, spiceboxv1alpha1.PermissionSurfaceEntry{
			Handle:      d.Handle.String(),
			StateImpact: string(d.StateImpact),
			Tools:       tools,
		})
	}
	slices.SortFunc(out, func(a, b spiceboxv1alpha1.PermissionSurfaceEntry) int {
		return strings.Compare(a.Handle, b.Handle)
	})
	return out
}

// PublishPermissionSurface records the enumerated surface on
// AgentSession.status.permissionSurface.
//
// Written once at session start, because that is when the tool envelope is
// fixed: the surface is a pure function of it, so re-deriving it later would
// produce the same bytes at the cost of a write. A resourceVersion-free merge
// patch scoped to this ONE field, mirroring the other runner-owned status
// writes — an unincluded field is never re-sent, so this can never clobber a
// concurrent operator or channelsd write to a different field.
//
// Best-effort by design, and that is a deliberate asymmetry: this field is
// reporting, not a gate. Failing session start because a display value could
// not be published would trade a working agent for a populated CLI column. The
// error is logged with enough context to find it, per the no-silent-errors
// rule, and returned so the caller can decide — the runner logs and continues.
func (s *StatusPatcher) PublishPermissionSurface(ctx context.Context, surface []permsurface.Descriptor) error {
	if s.local {
		return nil
	}
	entries := permissionSurfaceEntries(surface)
	if len(entries) == 0 {
		// Nothing to say. Deliberately not a write of `[]`: a session whose
		// tools reach nothing and a session whose surface was never computed
		// are different states, and an empty array would assert the first.
		return nil
	}
	body, err := json.Marshal(entries)
	if err != nil {
		return fmt.Errorf("marshal permissionSurface: %w", err)
	}
	patch := fmt.Sprintf(`{"status":{"permissionSurface":%s}}`, string(body))
	return s.c.Status().Patch(ctx,
		&spiceboxv1alpha1.AgentSession{ObjectMeta: metav1.ObjectMeta{Name: s.key.Name, Namespace: s.key.Namespace}},
		client.RawPatch(types.MergePatchType, []byte(patch)))
}
