//go:build e2e

package threadrun

import (
	"context"
	"fmt"
	"io"
	"path"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelsd/pipeline"
	"github.com/authzed/openagentprimitives/pkg/platform/artifactstore"
	blobstore "github.com/authzed/openagentprimitives/pkg/platform/artifactstore/blob"
	"github.com/authzed/openagentprimitives/pkg/platform/extract"
	"github.com/authzed/openagentprimitives/test/e2e"

	// The zip exploder, so an archive bundle explodes for REAL here rather
	// than against a fabricated member list.
	_ "github.com/authzed/openagentprimitives/pkg/platform/extract/ziparchive"
)

// wireInboundAssets stands in for the operator's POST /inbound-asset route,
// which the in-process harness has no HTTP surface for.
//
// WHAT THIS PROVES, and what it does not. The exploding is real —
// extract.ExploderFor and the registered zip backend, the same code extractord
// runs — and so is everything downstream: channelsd carrying members into the
// turn, the single manifest line, the member blocks, hydration keeping them out
// of the native window, and fetch_artifact resolving a member handle. What it
// does NOT exercise is the operator's own HTTP route and its index rendering;
// those have their own tests in pkg/memory/httpsrv. The seam this covers is the
// one unit tests cannot reach: channelsd → memory → runner.
func wireInboundAssets(t *testing.T, h *e2e.Harness) {
	t.Helper()
	store := blobstore.NewMem()
	h.SetArtifactStore(store)

	h.SetInboundAssetUploader(func(ctx context.Context, ns, sess, mime, filename string, body io.Reader) (pipeline.InboundAssetResult, error) {
		base := path.Join(ns, sess, "inbound-asset", filename)
		ref, err := store.Put(ctx, base+"/raw", body)
		if err != nil {
			return pipeline.InboundAssetResult{}, err
		}

		exploder, isArchive := extract.ExploderFor(mime)
		if !isArchive {
			// Non-archive: store the bytes and report them as text, which is
			// all any existing bundle needs.
			rc, gerr := store.Get(ctx, ref)
			if gerr != nil {
				return pipeline.InboundAssetResult{Ref: string(ref)}, nil
			}
			defer func() { _ = rc.Close() }()
			b, _ := io.ReadAll(rc)
			textRef, terr := store.Put(ctx, base+"/text", strings.NewReader(string(b)))
			if terr != nil {
				return pipeline.InboundAssetResult{Ref: string(ref)}, nil
			}
			return pipeline.InboundAssetResult{
				Ref: string(ref), Extracted: true, TextRef: string(textRef),
			}, nil
		}

		return explodeForHarness(ctx, t, store, exploder, base, ref)
	})
}

// explodeForHarness runs the REAL exploder and stores every member, then
// renders an index naming them.
//
// The index format here is a stand-in for the operator's — deliberately the
// minimum a bundle needs to find a member by name — because httpsrv's renderer
// is unexported and widening its API for a test would be worse than a small
// duplicate whose only job is to be read by one bundle.
func explodeForHarness(ctx context.Context, t *testing.T, store artifactstore.Store, exploder extract.Exploder, base string, ref artifactstore.Ref) (pipeline.InboundAssetResult, error) {
	t.Helper()

	rc, err := store.Get(ctx, ref)
	if err != nil {
		return pipeline.InboundAssetResult{Ref: string(ref)}, nil
	}
	defer func() { _ = rc.Close() }()
	raw, err := io.ReadAll(rc)
	if err != nil {
		return pipeline.InboundAssetResult{Ref: string(ref)}, nil
	}

	var members []pipeline.InboundAssetMember
	var idx strings.Builder
	i := 0
	_, xerr := exploder.Explode(strings.NewReader(string(raw)), int64(len(raw)), extract.DefaultLimits,
		func(m extract.Member) error {
			mrc, oerr := m.Open()
			if oerr != nil {
				return oerr
			}
			defer func() { _ = mrc.Close() }()

			mref, perr := store.Put(ctx, fmt.Sprintf("%s/members/%04d/raw", base, i), mrc)
			if perr != nil {
				return perr
			}
			// Members here are text-shaped fixtures, so their raw handle IS
			// their text handle. A real deployment runs the extractor.
			members = append(members, pipeline.InboundAssetMember{
				Name: m.Name, MIME: m.MIME, SizeBytes: m.Size,
				Ref: string(mref), TextRef: string(mref),
			})
			fmt.Fprintf(&idx, "  %s  %s  %s\n", m.Name, m.MIME, mref)
			i++
			return nil
		})
	require.NoError(t, xerr, "the fixture archive must explode cleanly")

	header := fmt.Sprintf("archive — %d files\n\n", len(members))
	indexRef, ierr := store.Put(ctx, base+"/index", strings.NewReader(header+idx.String()))
	require.NoError(t, ierr)

	return pipeline.InboundAssetResult{
		Ref: string(ref), Extracted: true, TextRef: string(indexRef), Members: members,
	}, nil
}
