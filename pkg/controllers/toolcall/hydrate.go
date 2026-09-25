package toolcall

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"io"
	"path"
	"strings"
	"time"

	"github.com/authzed/openagentprimitives/pkg/platform/artifactstore"
	"github.com/authzed/openagentprimitives/pkg/tools/exec"
	"github.com/authzed/openagentprimitives/pkg/tools/sandboxkinds"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// fileTransferTimeout bounds one artifact hydrate or harvest, whichever
// transport carries it — the backend's native FileTransferer or the tar stream
// over exec. Shared by both so the two paths cannot drift into different
// budgets, and so neither can end up with none.
const fileTransferTimeout = 30 * time.Second

// hydrateInputs materializes inputArtifacts at /work/in/<callId>/<path> in the
// sandbox. When the session's backend implements sandboxkinds.FileTransferer
// it writes each artifact directly through PutFile; otherwise it streams a
// tar archive over stdin to a `tar -x` invocation, using executor (already
// bound to the session's sandbox — see executorFor).
func (r *Reconciler) hydrateInputs(ctx context.Context, sess *spiceboxv1alpha1.SpiceboxSession, callID string, executor exec.Executor, artifacts []spiceboxv1alpha1.InputArtifact) error {
	if len(artifacts) == 0 {
		return nil
	}

	destDir := "/work/in/" + callID

	// Bound the whole operation, BOTH transports, before the branch below can
	// return. ToolCall reconciliation is serialized (SetupWithManager sets no
	// MaxConcurrentReconciles, so it defaults to 1), so a single hung
	// file-transfer call stalls ToolCall reconciliation cluster-wide with no
	// error and no terminal condition. A backend's native file API is a remote
	// call like any other; a deadline the tar path has and the native path does
	// not is the deadline nobody notices missing.
	ctx, cancel := context.WithTimeout(ctx, fileTransferTimeout)
	defer cancel()

	if ft, h, ok := r.fileTransfererFor(sess); ok {
		return r.hydrateInputsNative(ctx, ft, h, destDir, artifacts)
	}

	streamer, ok := executor.(exec.StreamingExecutor)
	if !ok {
		return fmt.Errorf("executor does not support streaming (input hydration requires it)")
	}

	archive, err := r.buildInputTar(ctx, artifacts)
	if err != nil {
		return fmt.Errorf("build tar: %w", err)
	}

	cmd := []string{"sh", "-c", fmt.Sprintf("mkdir -p %q && tar -x -C %q", destDir, destDir)}

	stream, err := streamer.StreamExec(ctx, exec.Request{
		Command: cmd,
	})
	if err != nil {
		return fmt.Errorf("stream exec: %w", err)
	}
	defer stream.Close()

	// Write archive bytes to stdin, then close.
	if _, err := io.Copy(stream.Stdin, archive); err != nil {
		return fmt.Errorf("write tar: %w", err)
	}
	if err := stream.Stdin.Close(); err != nil {
		return fmt.Errorf("close stdin: %w", err)
	}

	// Drain stdout (discard) and stderr (capture for error reporting).
	stderrBuf := &bytes.Buffer{}
	go func() { _, _ = io.Copy(io.Discard, stream.Stdout) }()
	go func() { _, _ = io.Copy(stderrBuf, stream.Stderr) }()

	res, waitErr := stream.Wait()
	if waitErr != nil {
		return fmt.Errorf("wait: %w", waitErr)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("tar exit %d: %s", res.ExitCode, stderrBuf.String())
	}
	return nil
}

func (r *Reconciler) buildInputTar(ctx context.Context, artifacts []spiceboxv1alpha1.InputArtifact) (io.Reader, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)

	for _, a := range artifacts {
		clean, err := cleanArtifactPath(a.Path)
		if err != nil {
			return nil, err
		}

		rc, err := r.Store.Get(ctx, artifactstore.Ref(a.ArtifactRef))
		if err != nil {
			return nil, fmt.Errorf("fetch %s: %w", a.ArtifactRef, err)
		}
		data, err := io.ReadAll(rc)
		_ = rc.Close()
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", a.ArtifactRef, err)
		}
		hdr := &tar.Header{
			Name: clean,
			Mode: 0o644,
			Size: int64(len(data)),
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return nil, err
		}
		if _, err := tw.Write(data); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return &buf, nil
}

// hydrateInputsNative writes each input artifact directly through the
// backend's FileTransferer, skipping the tar-over-exec path entirely. Errors
// are shaped the same as the tar path's per-file failures, so a hydration
// failure surfaces identically regardless of which mechanism moved the file.
func (r *Reconciler) hydrateInputsNative(ctx context.Context, ft sandboxkinds.FileTransferer, h sandboxkinds.Handle, destDir string, artifacts []spiceboxv1alpha1.InputArtifact) error {
	for _, a := range artifacts {
		clean, err := cleanArtifactPath(a.Path)
		if err != nil {
			return err
		}

		rc, err := r.Store.Get(ctx, artifactstore.Ref(a.ArtifactRef))
		if err != nil {
			return fmt.Errorf("fetch %s: %w", a.ArtifactRef, err)
		}
		data, err := io.ReadAll(rc)
		_ = rc.Close()
		if err != nil {
			return fmt.Errorf("read %s: %w", a.ArtifactRef, err)
		}

		if err := ft.PutFile(ctx, h, path.Join(destDir, clean), data); err != nil {
			return fmt.Errorf("put file %s: %w", clean, err)
		}
	}
	return nil
}

// cleanArtifactPath validates and cleans an InputArtifact.Path, shared by
// both the tar path (as tar.Header.Name) and the native FileTransferer path
// (joined onto destDir) so the two mechanisms enforce the identical
// relative-and-non-escaping contract.
func cleanArtifactPath(p string) (string, error) {
	if p == "" {
		return "", fmt.Errorf("inputArtifacts[*].path is required")
	}
	if path.IsAbs(p) {
		return "", fmt.Errorf("inputArtifacts[*].path must be relative: %s", p)
	}
	clean := path.Clean(p)
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("inputArtifacts[*].path must not escape: %s", p)
	}
	return clean, nil
}
