// Package anthropicbridge implements modality.Bridge (Tier-2 byte transport)
// against Anthropic's Files API. The bridge's own logic — shuttling bytes
// between artifactstore and the provider's file store — is isolated behind
// the local FilesClient interface so it can be unit-tested with a fake, with
// no network dependency. SDKFilesClient is the thin production adapter over
// anthropic-sdk-go's Beta.Files service.
package anthropicbridge

import (
	"context"
	"fmt"
	"io"
	"strings"

	sdk "github.com/anthropics/anthropic-sdk-go"

	"github.com/authzed/openagentprimitives/pkg/agent/modality"
	"github.com/authzed/openagentprimitives/pkg/platform/artifactstore"
)

// FilesClient is the minimal provider file-store surface the bridge needs.
// Kept local so the bridge logic is testable without network; the production
// impl (SDKFilesClient) wraps the anthropic-sdk-go Beta.Files service.
type FilesClient interface {
	Download(ctx context.Context, fileID string) (io.ReadCloser, error)
	Upload(ctx context.Context, name string, r io.Reader) (fileID string, err error)
}

type bridge struct {
	store artifactstore.Store
	fc    FilesClient
}

// New builds a modality.Bridge over an artifactstore + a provider FilesClient.
func New(store artifactstore.Store, fc FilesClient) modality.Bridge {
	return &bridge{store: store, fc: fc}
}

// IntoStore pulls a provider-container file into artifactstore (files-out).
func (b *bridge) IntoStore(ctx context.Context, providerFileID string) (artifactstore.Ref, error) {
	rc, err := b.fc.Download(ctx, providerFileID)
	if err != nil {
		return "", fmt.Errorf("anthropicbridge: download %s: %w", providerFileID, err)
	}
	defer rc.Close()
	// IntoStore is dormant today (no caller wires anthropicbridge.New in
	// production; files-out inlines bytes directly via FileDownloader —
	// see meta.ArtifactPrepareConfig.FileDownloader). It exists for a future
	// operator-side files-out path; when wired there, the caller owns
	// cleaning up this intermediate store entry —
	// artifact_prepare has no store_ref source (removed: no session-ownership
	// check on a model-supplied ref), so this is NOT reached via that tool.
	ref, err := b.store.Put(ctx, "native-file/"+providerFileID, rc)
	if err != nil {
		return "", fmt.Errorf("anthropicbridge: store %s: %w", providerFileID, err)
	}
	return ref, nil
}

// IntoContainer pushes artifactstore bytes into the provider file store (files-in),
// returning the provider file id to reference in a container_upload block.
func (b *bridge) IntoContainer(ctx context.Context, ref artifactstore.Ref) (string, error) {
	rc, err := b.store.Get(ctx, ref)
	if err != nil {
		return "", fmt.Errorf("anthropicbridge: get %s: %w", ref, err)
	}
	defer rc.Close()
	id, err := b.fc.Upload(ctx, filenameFromRef(ref), rc)
	if err != nil {
		return "", fmt.Errorf("anthropicbridge: upload %s: %w", ref, err)
	}
	return id, nil
}

// filenameFromRef derives a human-meaningful filename from an artifactstore
// ref for the provider upload, so files don't all land as the SDK's
// "anonymous_file" default. It strips any "scheme://" prefix and takes the
// last "/"-separated segment.
func filenameFromRef(ref artifactstore.Ref) string {
	s := string(ref)
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+len("://"):]
	}
	if i := strings.LastIndex(s, "/"); i >= 0 {
		s = s[i+1:]
	}
	if s == "" {
		return "artifact"
	}
	return s
}

// SDKFilesClient adapts the anthropic-sdk-go Beta.Files service to FilesClient.
type SDKFilesClient struct{ client *sdk.Client }

// NewSDKFilesClient wraps client's Beta.Files service as a FilesClient.
func NewSDKFilesClient(client *sdk.Client) SDKFilesClient { return SDKFilesClient{client: client} }

// Download fetches fileID's bytes via the Anthropic Files API.
func (c SDKFilesClient) Download(ctx context.Context, fileID string) (io.ReadCloser, error) {
	resp, err := c.client.Beta.Files.Download(ctx, fileID, sdk.BetaFileDownloadParams{
		Betas: []sdk.AnthropicBeta{sdk.AnthropicBetaFilesAPI2025_04_14},
	})
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

// Upload writes r's bytes to the Anthropic Files API under name and returns
// the new file's ID. sdk.File wraps r so the SDK's apiform multipart encoder
// (internal/apiform/encoder.go) picks it up via the reader's Filename()
// method instead of falling back to its "anonymous_file" default.
func (c SDKFilesClient) Upload(ctx context.Context, name string, r io.Reader) (string, error) {
	meta, err := c.client.Beta.Files.Upload(ctx, sdk.BetaFileUploadParams{
		File:  sdk.File(r, name, ""),
		Betas: []sdk.AnthropicBeta{sdk.AnthropicBetaFilesAPI2025_04_14},
	})
	if err != nil {
		return "", err
	}
	return meta.ID, nil
}

var _ FilesClient = SDKFilesClient{}
