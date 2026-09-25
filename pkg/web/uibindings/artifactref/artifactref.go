// Package artifactref is the "artifact" uibindings.Resolver: a read-only
// resolution of an artifact handle to its rendered bytes, scoped to the
// viewer's own session exactly the way the artifact-view page's own deps
// already resolve handles (internal/cmd/webd's artifactViewDeps.ResolveAssetURL) — a
// handle belonging to a different session simply isn't found within this
// scope, which is what makes it unresolvable rather than merely unauthorized.
package artifactref

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/authzed/openagentprimitives/pkg/authz/toolguard"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/platform/artifacts"
	"github.com/authzed/openagentprimitives/pkg/web/uibindings"
	"github.com/authzed/openagentprimitives/pkg/web/uibindings/registry"
)

// source is this resolver's uibindings.Request.Source / Resolver.Source()
// value, and the string a component prop's Binding declares to route here.
const source = "artifact"

// tooLargeMessage is the browser-facing copy for a result over the UI
// ingress ceiling — matches memoryref's copy, which matches the posture
// toolguard's own ui_ingress deny message takes for the tool-call binding
// path (pkg/authz/toolguard/events.go's byteDenyMessage).
const tooLargeMessage = "This view's data is too large to display. Narrow the time span, add a filter, or ask for fewer rows."

// Resolver is the "artifact" uibindings.Resolver. It carries no byte-fetching
// collaborator of its own — that lives on Deps.ArtifactRenderBytes, reached
// fresh per Resolve call — so New takes no arguments.
type Resolver struct{}

// New returns the "artifact" resolver.
func New() uibindings.Resolver { return Resolver{} }

func init() { registry.Register(New()) }

// Source implements uibindings.Resolver.
func (Resolver) Source() string { return source }

// Resolve maps req.Ref (an artifact handle — a head, optionally with "#tag",
// or a revision) to its rendered bytes. Order, all fail-closed: a nil
// Artifacts or ArtifactRenderBytes errors before any lookup; the handle
// resolves SCOPED to the viewer's own session, so one from another session is
// simply not found rather than merely denied; oversized bytes are REFUSED, not
// truncated; JSON content binds verbatim (for ap:table) and everything else
// binds as a JSON string (for ap:markdown.body).
func (Resolver) Resolve(ctx context.Context, d uibindings.Deps, req uibindings.Request) (uibindings.Result, error) {
	logger := d.Logger()
	session := req.Namespace + "/" + req.Session

	// Artifacts() returns a CONCRETE pointer (unlike Memory()'s interface), so
	// this nil check is honest by construction — see uibindings.Deps' doc.
	svc := d.Artifacts()
	if svc == nil {
		logger.Info("ui data binding artifact resolve failed: Artifacts is not configured",
			"session", session, "path", req.Path, "ref", req.Ref)
		return uibindings.Result{}, errors.New("uibindings/artifact: Artifacts is not configured")
	}
	// ArtifactRenderBytes() returns a FUNC type directly (not boxed behind an
	// interface), so this nil check is likewise honest — see uibindings.Deps'
	// doc for why the fetch lives there rather than as a constructor argument.
	fetch := d.ArtifactRenderBytes()
	if fetch == nil {
		logger.Info("ui data binding artifact resolve failed: ArtifactRenderBytes is not configured",
			"session", session, "path", req.Path, "ref", req.Ref)
		return uibindings.Result{}, errors.New("uibindings/artifact: ArtifactRenderBytes is not configured")
	}

	scope := memory.Scope{Kind: "session", ID: session}
	renderName, err := svc.ResolveToRender(ctx, scope, req.Ref)
	if err != nil {
		if errors.Is(err, artifacts.ErrNotFound) {
			logger.Info("ui data binding artifact resolve failed: handle not found in this session",
				"session", session, "path", req.Path, "ref", req.Ref)
			return uibindings.Result{}, errors.New("this view's data is not available")
		}
		logger.Info("ui data binding artifact resolve failed: resolve error",
			"session", session, "path", req.Path, "ref", req.Ref, "err", err.Error())
		return uibindings.Result{}, errors.New("this view's data could not be loaded")
	}

	b, _, err := fetch(ctx, req.Namespace, req.Session, renderName)
	if err != nil {
		logger.Info("ui data binding artifact resolve failed: fetch error",
			"session", session, "path", req.Path, "ref", req.Ref, "err", err.Error())
		return uibindings.Result{}, errors.New("this view's data could not be loaded")
	}
	if int64(len(b)) > toolguard.DefaultUIIngressBytes {
		logger.Info("ui data binding artifact resolve refused: result over the UI ingress ceiling",
			"session", session, "path", req.Path, "ref", req.Ref,
			"bytes", len(b), "limit", toolguard.DefaultUIIngressBytes)
		return uibindings.Result{}, errors.New(tooLargeMessage)
	}

	if json.Valid(b) {
		return uibindings.Result{Value: json.RawMessage(b)}, nil
	}
	value, err := json.Marshal(string(b))
	if err != nil {
		logger.Info("ui data binding artifact resolve failed: encode result",
			"session", session, "path", req.Path, "ref", req.Ref, "err", err.Error())
		return uibindings.Result{}, errors.New("this view's data could not be loaded")
	}
	return uibindings.Result{Value: value}, nil
}
