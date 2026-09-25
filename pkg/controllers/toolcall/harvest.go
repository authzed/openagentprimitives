package toolcall

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"text/template"

	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/authzed/openagentprimitives/pkg/tools/exec"
	"github.com/authzed/openagentprimitives/pkg/tools/sandboxkinds"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// syncBuffer is a bytes.Buffer whose writer and readers may be different
// goroutines.
//
// harvestOutputs drains the exec stream's stderr on a side goroutine and reads
// the captured text from the reconcile goroutine on its error paths, so the
// buffer itself must be safe to touch from both. Neither the stdlib nor
// anything in go.mod offers a concurrent buffer to reach for.
//
// The mutex is necessary and NOT sufficient: it makes the buffer race-free
// without establishing any ordering between the copy and the read, and
// stream.Wait() reports only that the PROCESS exited. harvestOutputs therefore
// joins the copier explicitly (awaitStderr) before every read — without that,
// a failing tool's stderr can be missing from the error text, which is where
// the operator looks first and the one place the exit code cannot explain
// itself.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// harvestOutputs reads each declared CaptureOutputs path and stores it in the
// ArtifactStore, returning the list of OutputArtifacts to set on status. When
// the session's backend implements sandboxkinds.FileTransferer it reads each
// path directly through GetFile; otherwise it runs `tar -c <paths...>` inside
// the pod and parses the returned stream.
func (r *Reconciler) harvestOutputs(
	ctx context.Context,
	sess *spiceboxv1alpha1.SpiceboxSession,
	tc *spiceboxv1alpha1.ToolCall,
) ([]spiceboxv1alpha1.OutputArtifact, error) {
	if len(tc.Spec.CaptureOutputs) == 0 {
		return nil, nil
	}

	paths, err := renderCapturePaths(tc, sess)
	if err != nil {
		return nil, err
	}

	// Bound the whole operation, BOTH transports, before the branch below can
	// return — see fileTransferTimeout. Without this the native path runs
	// unbounded, and ToolCall reconciliation is serialized, so one hung call
	// stalls every ToolCall in the cluster with no error and no terminal
	// condition.
	ctx, cancel := context.WithTimeout(ctx, fileTransferTimeout)
	defer cancel()

	if ft, h, ok := r.fileTransfererFor(sess); ok {
		return r.harvestOutputsNative(ctx, ft, h, sess, tc, paths)
	}

	executor, err := r.executorFor(sess)
	if err != nil {
		return nil, err
	}
	streamer, ok := executor.(exec.StreamingExecutor)
	if !ok {
		return nil, fmt.Errorf("executor does not support streaming (output harvest requires it)")
	}

	args := []string{"-cf", "-"}
	for _, p := range paths {
		args = append(args, p)
	}
	cmd := append([]string{"tar"}, args...)

	stream, err := streamer.StreamExec(ctx, exec.Request{
		Command: cmd,
		// `tar -c` is a one-shot producer that never reads stdin. Pass an empty
		// (already-EOF) reader so StreamExec's finite-stdin path is taken: the
		// kubelet drains it and tar sees stdin at EOF immediately. With a nil
		// Stdin, StreamExec instead opens a LIVE interactive pipe (for
		// channel-fed interactive tools), and the exec never completes — tar's
		// stdin stays open, StreamWithContext blocks, and Wait() hits the 30s
		// deadline. That timeout produced zero OutputArtifacts, which surfaced
		// downstream as a file: secret output "not produced" even though the
		// producer wrote the file correctly.
		Stdin: bytes.NewReader(nil),
	})
	if err != nil {
		return nil, fmt.Errorf("stream exec: %w", err)
	}
	defer stream.Close()

	// Close stdin immediately — tar doesn't read from stdin in -c mode.
	_ = stream.Stdin.Close()

	tarReader := tar.NewReader(stream.Stdout)
	var out []spiceboxv1alpha1.OutputArtifact
	stderrBuf := &syncBuffer{}
	// stderrCopied closes when the copier has drained stream.Stderr to EOF.
	// syncBuffer alone is not enough: it makes the buffer safe to touch from
	// two goroutines, so -race stays quiet, but it establishes no ordering
	// between the copy and the reads below. stream.Wait() reports that the
	// PROCESS exited; it says nothing about whether the copy finished. Without
	// this signal, a failing tool's stderr can be absent from the error text —
	// "tar exit 2: " with nothing after it — losing the diagnostic exactly
	// where the operator needs it, and losing it silently.
	stderrCopied := make(chan struct{})
	// The copier must not touch sess or tc. Reconcile writes both back through
	// Status().Update(), whose decode rewrites their fields in place, so a
	// goroutine still reading sess.Name races the reconcile that owns them.
	// Copy the two identifiers the log line needs before starting.
	logSession, logToolCall := sess.Name, tc.Name
	go func() {
		defer close(stderrCopied)
		if _, err := io.Copy(stderrBuf, stream.Stderr); err != nil {
			// Not fatal: whatever arrived before the failure is still the best
			// diagnostic available, and the exit code below is the real
			// verdict. Record it so a truncated stderr is explainable rather
			// than mysterious.
			log.FromContext(ctx).Info("toolcall harvest: copying tar stderr failed; the captured stderr may be truncated",
				"session", logSession, "toolcall", logToolCall, "err", err.Error())
		}
	}()

	// awaitStderr blocks until the copier finishes, bounded by ctx so a stream
	// that never reaches EOF cannot hang the harvest. On timeout the buffer is
	// read as-is — a partial diagnostic beats none.
	awaitStderr := func() {
		select {
		case <-stderrCopied:
		case <-ctx.Done():
		}
	}

	for {
		hdr, err := tarReader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			awaitStderr()
			return nil, fmt.Errorf("tar.Next: %w (stderr: %s)", err, stderrBuf.String())
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		if hdr.Size > (16 << 20) {
			return nil, fmt.Errorf("output file %s too large: %d bytes", hdr.Name, hdr.Size)
		}
		data := make([]byte, hdr.Size)
		if _, err := io.ReadFull(tarReader, data); err != nil {
			return nil, fmt.Errorf("read body for %s: %w", hdr.Name, err)
		}
		key := fmt.Sprintf("%s/%s/%s/out/%s", tc.Namespace, sess.Name, tc.UID, hdr.Name)
		ref, putErr := r.Store.Put(ctx, key, bytes.NewReader(data))
		if putErr != nil {
			return nil, fmt.Errorf("store %s: %w", hdr.Name, putErr)
		}
		out = append(out, spiceboxv1alpha1.OutputArtifact{
			Path:        hdr.Name,
			ArtifactRef: string(ref),
			Size:        hdr.Size,
		})
	}

	// tar pads its output to a full record (default 20*512 = 10240 bytes) with
	// zero blocks AFTER the two-block end-of-archive marker. tar.Reader stops at
	// that marker and never consumes the trailing padding, so the loop above
	// breaks with bytes still buffered in the pod's stdout. StreamExec relays
	// that stdout through a SYNCHRONOUS io.Pipe (remote.Binder), so the
	// executor goroutine BLOCKS writing the unread padding — it never returns,
	// its exit-code channel is never signalled, and stream.Wait() below then
	// hangs the entire ctx deadline. That surfaced as a spurious
	// "wait: context deadline exceeded" even though tar exited 0 and the file
	// was already captured into `out` — and the caller discards `out` on any
	// harvest error, so the secret output looked "not produced". Drain the
	// residue to EOF so the exec completes and Wait sees the real exit code.
	if _, err := io.Copy(io.Discard, stream.Stdout); err != nil {
		return out, fmt.Errorf("drain trailing stdout: %w", err)
	}

	res, waitErr := stream.Wait()
	if waitErr != nil {
		return out, fmt.Errorf("wait: %w", waitErr)
	}
	if res.ExitCode != 0 {
		awaitStderr()
		return out, fmt.Errorf("tar exit %d: %s", res.ExitCode, stderrBuf.String())
	}
	return out, nil
}

// harvestOutputsNative reads each declared output path directly through the
// backend's FileTransferer, skipping the tar-over-exec path entirely. Errors
// are shaped the same as the tar path's per-file failures, so a harvest
// failure surfaces identically regardless of which mechanism moved the file.
// Path is slash-stripped to match the tar path's tar.Header.Name (GNU tar
// strips the leading "/" from absolute member names).
func (r *Reconciler) harvestOutputsNative(
	ctx context.Context,
	ft sandboxkinds.FileTransferer,
	h sandboxkinds.Handle,
	sess *spiceboxv1alpha1.SpiceboxSession,
	tc *spiceboxv1alpha1.ToolCall,
	paths []string,
) ([]spiceboxv1alpha1.OutputArtifact, error) {
	var out []spiceboxv1alpha1.OutputArtifact
	for _, p := range paths {
		data, err := ft.GetFile(ctx, h, p)
		if err != nil {
			return out, fmt.Errorf("get file %s: %w", p, err)
		}
		if int64(len(data)) > (16 << 20) {
			return out, fmt.Errorf("output file %s too large: %d bytes", p, len(data))
		}
		name := strings.TrimPrefix(p, "/")
		key := fmt.Sprintf("%s/%s/%s/out/%s", tc.Namespace, sess.Name, tc.UID, name)
		ref, putErr := r.Store.Put(ctx, key, bytes.NewReader(data))
		if putErr != nil {
			return out, fmt.Errorf("store %s: %w", name, putErr)
		}
		out = append(out, spiceboxv1alpha1.OutputArtifact{
			Path:        name,
			ArtifactRef: string(ref),
			Size:        int64(len(data)),
		})
	}
	return out, nil
}

// renderCapturePaths expands {{.callId}} and {{.sessionName}} in each pattern.
func renderCapturePaths(tc *spiceboxv1alpha1.ToolCall, sess *spiceboxv1alpha1.SpiceboxSession) ([]string, error) {
	data := map[string]string{
		"callId":      string(tc.UID),
		"sessionName": sess.Name,
	}
	var out []string
	for _, p := range tc.Spec.CaptureOutputs {
		tmpl, err := template.New("capture").Option("missingkey=error").Parse(p)
		if err != nil {
			return nil, fmt.Errorf("parse %q: %w", p, err)
		}
		var b strings.Builder
		if err := tmpl.Execute(&b, data); err != nil {
			return nil, fmt.Errorf("render %q: %w", p, err)
		}
		rendered := b.String()
		if !filepath.IsAbs(rendered) {
			return nil, fmt.Errorf("captureOutputs path must be absolute: %q", rendered)
		}
		out = append(out, rendered)
	}
	return out, nil
}
