package imagebuild

import (
	"context"
	"fmt"
	"io"

	"github.com/authzed/openagentprimitives/pkg/platform/oap"
)

type Env interface {
	// Resolve reports whether ref is already available to the cluster, and the
	// ref the cluster should actually pull. These differ whenever delivery goes
	// through a registry: the bundle declares "demo-image:dev" but the cluster
	// pulls "<registry>/demo-image@sha256:…". Returning only a bool would force
	// the caller to record the undeliverable original.
	//
	// Registry-delivered refs come back DIGEST-PINNED, because a tag is
	// per-node cached under the default IfNotPresent pull policy and so cannot
	// express "the image I just published". Local delivery paths keep the bare
	// tag: the image never reaches a registry, so there is no digest to pin.
	Resolve(ctx context.Context, ref string) (resolvedRef string, present bool, err error)
	Deliver(ctx context.Context, b oap.ImageBuild, in Inputs, ref string) (resolvedRef string, err error)
	Confirm(prompt string) bool
	Secret(name string) (string, error)
	Path(name string) (string, error)
}

type Options struct {
	BuildMissing bool
	NoBuild      bool
	Rebuild      bool
	Platform     string
}

// Reconcile makes each declared image available to the current cluster,
// returning oldRef->resolvedRef. Present images (unless --rebuild) are reused
// verbatim, keeping a re-install a byte-identical SSA no-op.
func Reconcile(ctx context.Context, out io.Writer, images []oap.RequiredImage, env Env, opts Options) (map[string]string, error) {
	rewrites := make(map[string]string, len(images))
	for _, im := range images {
		if !opts.Rebuild {
			resolvedRef, present, err := env.Resolve(ctx, im.Ref)
			if err != nil {
				return nil, fmt.Errorf("reconcile %s: probe presence: %w", im.Ref, err)
			}
			if present {
				rewrites[im.Ref] = resolvedRef
				if resolvedRef != im.Ref {
					// A rewritten ref means delivery goes through a registry,
					// where the REBUILD decision is still a lookup of a mutable
					// tag: an edited bundle republished under the same tag reads
					// as present and is silently not rebuilt. (What gets
					// recorded is pinned to that tag's current digest, so the
					// cluster at least pulls exactly what was probed.) Local
					// paths always report absent, so they never reach this
					// message.
					fmt.Fprintf(out, "  image %s: already present as %s — reusing it (pass --rebuild-images to rebuild and push over that tag)\n", im.Ref, resolvedRef)
				} else {
					fmt.Fprintf(out, "  image %s: already present as %s\n", im.Ref, resolvedRef)
				}
				continue
			}
		}
		if im.Build == nil {
			return nil, fmt.Errorf("image %s is not available and the bundle declares no build recipe for it — supply a registry ref or add requires.images[].build", im.Ref)
		}
		if opts.NoBuild {
			return nil, fmt.Errorf("image %s is not available and --no-build was set (run without --no-build to build it, or make the ref pullable)", im.Ref)
		}
		if !opts.BuildMissing && !env.Confirm(fmt.Sprintf("Image %s is not available. Build it now?", im.Ref)) {
			return nil, fmt.Errorf("image %s not available and build declined", im.Ref)
		}
		in := Inputs{
			Secrets:       map[string]string{},
			BuildContexts: map[string]string{},
			Platform:      opts.Platform,
		}
		for _, name := range im.Build.Secrets {
			v, err := env.Secret(name)
			if err != nil {
				return nil, fmt.Errorf("reconcile %s: collect secret %q: %w", im.Ref, name, err)
			}
			in.Secrets[name] = v
		}
		for _, name := range im.Build.BuildContexts {
			p, err := env.Path(name)
			if err != nil {
				return nil, fmt.Errorf("reconcile %s: collect build-context %q: %w", im.Ref, name, err)
			}
			in.BuildContexts[name] = p
		}
		resolved, err := env.Deliver(ctx, *im.Build, in, im.Ref)
		if err != nil {
			return nil, fmt.Errorf("reconcile %s: build+deliver: %w", im.Ref, err)
		}
		rewrites[im.Ref] = resolved
	}
	return rewrites, nil
}
