package sessionevents

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/authzed/openagentprimitives/pkg/agent/sessionevents"
	"github.com/authzed/openagentprimitives/pkg/agent/sessionevents/native"
	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/infoleakagetaint"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type Authority interface {
	CheckOnResource(context.Context, string, string, string, identity.CanonicalUserID, bool) (bool, error)
}

// NativeSessions pins native session streams and retains their current flow
// dependencies. Signed evidence cannot name a recreated or deleted session.
type NativeSessions struct {
	Reader client.Reader
	Memory memory.Memory
	Auth   Authority
}

func (n *NativeSessions) session(ctx context.Context, source sessionevents.Source) (*v1.AgentSession, error) {
	if n == nil || n.Reader == nil || n.Memory == nil {
		return nil, sessionevents.ErrDenied
	}
	ns, name, ok := strings.Cut(source.ID, "/")
	if !ok || ns != source.Namespace || source.Kind != native.Key || name == "" {
		return nil, sessionevents.ErrDenied
	}
	var sess v1.AgentSession
	if err := n.Reader.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &sess); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, sessionevents.ErrDenied
		}
		return nil, err
	}
	if sess.UID == "" || !sess.DeletionTimestamp.IsZero() || (source.UID != "" && string(sess.UID) != source.UID) {
		return nil, sessionevents.ErrDenied
	}
	return &sess, nil
}

func (n *NativeSessions) dependencies(ctx context.Context, source sessionevents.Source) ([]sessionevents.Dependency, error) {
	ctx = memory.WithCaller(memory.WithSystemApproval(ctx, "system:operator"), "system:operator")
	page, err := n.Memory.Query(ctx, memory.Query{Scope: memory.Scope{Kind: "session", ID: source.ID}, Kinds: []string{infoleakagetaint.KindName}, Limit: 10000})
	if err != nil {
		return nil, err
	}
	if page.Truncated || page.Partial {
		return nil, sessionevents.ErrDenied
	}
	deps := []sessionevents.Dependency{{ResourceType: "agentsession", ResourceID: source.ID, Permission: "read_transcript"}}
	for _, entry := range page.Entries {
		var dep infoleakagetaint.TaintRecord
		if err := json.Unmarshal(entry.Content, &dep); err != nil {
			return nil, err
		}
		deps = append(deps, sessionevents.Dependency{ResourceType: dep.ResourceType, ResourceID: dep.ResourceID, Permission: dep.Permission})
	}
	return deps, nil
}

func (n *NativeSessions) CheckSource(ctx context.Context, source sessionevents.Source, entry memory.Entry) ([]sessionevents.Dependency, error) {
	if source.UID == "" || entry.Scope != (memory.Scope{Kind: "session", ID: source.ID}) {
		return nil, sessionevents.ErrDenied
	}
	if _, err := n.session(ctx, source); err != nil {
		return nil, err
	}
	return n.dependencies(ctx, source)
}

func (n *NativeSessions) Resolve(ctx context.Context, principal string, source sessionevents.Source) (sessionevents.Source, error) {
	sess, err := n.session(ctx, source)
	if err != nil {
		return source, err
	}
	source.UID = string(sess.UID)
	return source, n.Check(ctx, principal, source, nil)
}

func (n *NativeSessions) Check(ctx context.Context, principal string, source sessionevents.Source, deps []sessionevents.Dependency) error {
	if n == nil || n.Auth == nil || principal == "" || source.UID == "" {
		return sessionevents.ErrDenied
	}
	if _, err := n.session(ctx, source); err != nil {
		return err
	}
	current, err := n.dependencies(ctx, source)
	if err != nil {
		return err
	}
	owner := identity.CanonicalFromTrusted(principal, "verified event-watch principal")
	for _, dep := range append(current, deps...) {
		if dep.ResourceType == "" || dep.ResourceID == "" || dep.Permission == "" {
			return sessionevents.ErrDenied
		}
		allowed, err := n.Auth.CheckOnResource(ctx, dep.ResourceType, dep.ResourceID, dep.Permission, owner, true)
		if err != nil {
			return err
		}
		if !allowed {
			return sessionevents.ErrDenied
		}
	}
	return nil
}

func (n *NativeSessions) Dependencies(ctx context.Context, principal string, source sessionevents.Source) ([]sessionevents.Dependency, error) {
	if n == nil || n.Reader == nil || n.Memory == nil || n.Auth == nil {
		return nil, sessionevents.ErrDenied
	}
	deps, err := n.dependencies(ctx, source)
	if err != nil {
		return nil, err
	}
	if err := n.Check(ctx, principal, source, deps); err != nil {
		return nil, err
	}
	return deps, nil
}
