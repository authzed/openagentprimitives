// Package buildx holds the `docker buildx` push plumbing shared by the two
// build paths: oap's own first-party image targets
// (cmd/oap/internal/installcmd/build.go) and .oap bundle recipes
// (cmd/oap/internal/imagebuild).
//
// Only the plumbing is shared, not the argv construction — a first-party target
// is a dockerfile plus a context, while a bundle recipe also carries secrets,
// build-contexts, and build-args. Forcing one argv builder to serve both would
// mean either dropping recipe inputs or bolting unused parameters onto oap build.
package buildx

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/cenkalti/backoff/v4"
)

// RegistryHost returns a registry's host — the segment before the first "/".
func RegistryHost(registry string) string {
	if i := strings.Index(registry, "/"); i >= 0 {
		return registry[:i]
	}
	return registry
}

// MetadataDigest extracts the pushed image digest ("sha256:…") from the JSON
// `docker buildx build --metadata-file` output, whose top level carries a
// "containerimage.digest" key. Returns an error if the key is absent or empty.
func MetadataDigest(b []byte) (string, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		return "", fmt.Errorf("parse buildx metadata: %w", err)
	}
	raw, ok := m["containerimage.digest"]
	if !ok {
		return "", fmt.Errorf("buildx metadata has no containerimage.digest")
	}
	var d string
	if err := json.Unmarshal(raw, &d); err != nil {
		return "", fmt.Errorf("parse containerimage.digest: %w", err)
	}
	if d == "" {
		return "", fmt.Errorf("buildx metadata containerimage.digest is empty")
	}
	if !strings.HasPrefix(d, "sha256:") {
		return "", fmt.Errorf("buildx metadata containerimage.digest is not a sha256 digest: %q", d)
	}
	return d, nil
}

// failureKind is what a push failure's stderr says went wrong. One
// classification serves both TranslatePushError (which remedy to name) and
// Retryable (whether another attempt could plausibly succeed), so the two can
// never disagree about the same stderr.
type failureKind int

const (
	failureUnknown failureKind = iota
	failureDenied
	failureMissingRepo
	failureNetwork
)

// networkFailures are the substrings that mark a push as having died in
// transit rather than having been refused. The daemon reaches the registry
// through whatever proxy it was configured with — Docker Desktop points one at
// the host by default — so a loaded machine can lose the connection to that hop
// while the registry itself is perfectly healthy.
//
// These are grouped by the layer that emits them rather than collected from
// whichever failure was observed most recently. A loaded machine produces
// several shapes that share no vocabulary — a failed dial says "i/o timeout"
// and names no client, while a stalled response says "Client.Timeout exceeded"
// and names no dial — so enumerating from one sample reliably misses the rest.
var networkFailures = []string{
	// Dial and connection setup.
	"proxyconnect", // the daemon could not reach its own configured HTTP proxy
	"i/o timeout",
	"connection reset by peer",
	"connection refused",
	"no route to host",
	"network is unreachable",
	"server misbehaving", // DNS resolver itself timed out

	// TLS.
	"TLS handshake timeout",

	// net/http client-side deadlines. The registry answered the dial and then
	// stalled, so none of the dial vocabulary above appears.
	"Client.Timeout exceeded",
	"request canceled",
	"timeout awaiting response headers",
	"context deadline exceeded",

	// Connection lost mid-transfer.
	"unexpected EOF",
	"broken pipe",
	"use of closed network connection",
	"http2: client connection lost",

	// Registry answered, but with something a retry can clear.
	"temporarily unavailable",
	"500 Internal Server Error",
	"502 Bad Gateway",
	"503 Service Unavailable",
	"504 Gateway Timeout",
}

// classify reads a push failure's stderr. The refusal cases are tested before
// the network ones deliberately: a denied push whose stderr also mentions a
// timeout must stay permanent, so that the error names the credential the user
// has to fix rather than being retried into a slower version of itself.
func classify(stderr string) failureKind {
	switch {
	case strings.Contains(stderr, "denied") || strings.Contains(stderr, "Unauthenticated") || strings.Contains(stderr, "uploadArtifacts"):
		return failureDenied
	case strings.Contains(stderr, "NOT_FOUND") || strings.Contains(stderr, "may not exist"):
		return failureMissingRepo
	default:
		for _, s := range networkFailures {
			if strings.Contains(stderr, s) {
				return failureNetwork
			}
		}
		return failureUnknown
	}
}

// Retryable reports whether a push failure is a transient network fault worth
// another attempt. Auth and missing-repository failures are not: retrying those
// only delays the error that names what the user has to do.
func Retryable(stderr string) bool { return classify(stderr) == failureNetwork }

// TranslatePushError turns a raw `docker buildx --push` failure into an error
// naming the remedy, based on what the daemon wrote to stderr.
func TranslatePushError(name, registry, stderr string, err error) error {
	switch classify(stderr) {
	case failureDenied:
		return fmt.Errorf("pushing %s to %s was denied — Docker is not authenticated to that registry. For GKE Artifact Registry run `gcloud auth configure-docker %s`, then retry: %w", name, registry, RegistryHost(registry), err)
	case failureMissingRepo:
		return fmt.Errorf("pushing %s failed — the registry repository may not exist. Create it first: `oap build`/`oap init` can do so with --create-registry; `oap agent install` cannot, so create the repository in %s manually (or point --image-registry at one that already exists): %w", name, registry, err)
	case failureNetwork:
		return fmt.Errorf("pushing %s never reached %s — the Docker daemon could not complete the transfer. If `docker info` reports an HTTP/HTTPS proxy (Docker Desktop configures one by default), add %s to its bypass list under Settings → Resources → Proxies; otherwise check the daemon's connectivity to that host: %w", name, registry, RegistryHost(registry), err)
	default:
		return fmt.Errorf("docker buildx %s: %w (is buildx/BuildKit set up? try `docker buildx create --use`)", name, err)
	}
}

// Retry policy defaults for RunPush. Three attempts covers the common case — a
// single dropped connection on a loaded machine — without making a genuinely
// unreachable registry take a visibly long time to say so.
const (
	DefaultAttempts    = 3
	DefaultInitialWait = 2 * time.Second
)

// Retry bounds RunPush's retries. The zero value means the defaults above.
type Retry struct {
	Attempts    int           // total attempts, including the first
	InitialWait time.Duration // wait before the first retry; doubles thereafter
}

// Attempt runs one push and returns whatever the docker command wrote to
// stderr, so RunPush can classify the failure. It returns nil on success.
type Attempt func(ctx context.Context) (stderr string, err error)

// RunPush runs attempt, retrying transient network failures with jittered
// exponential backoff, and translates whatever failure survives.
//
// Retrying is cheap in exactly the case that matters: the blobs an attempt
// already uploaded stay in the registry and BuildKit's cache stays warm, so a
// retry after a dropped manifest PUT re-uploads nothing and re-builds nothing.
// The jitter is not incidental — oap pushes several images concurrently, so a
// fixed delay would send them all back at the same overloaded hop together.
func RunPush(ctx context.Context, log io.Writer, name, registry string, r Retry, attempt Attempt) error {
	attempts := r.Attempts
	if attempts <= 0 {
		attempts = DefaultAttempts
	}
	wait := r.InitialWait
	if wait <= 0 {
		wait = DefaultInitialWait
	}

	eb := backoff.NewExponentialBackOff()
	eb.InitialInterval = wait
	eb.MaxElapsedTime = 0 // bounded by the attempt count, not by the clock

	var lastStderr string
	tries := 0
	op := func() error {
		tries++
		stderr, err := attempt(ctx)
		if err == nil {
			return nil
		}
		lastStderr = stderr
		if !Retryable(stderr) {
			return backoff.Permanent(err)
		}
		return err
	}
	notify := func(_ error, d time.Duration) {
		fmt.Fprintf(log, "==> pushing %s hit a transient network error (attempt %d of %d); retrying in %s\n",
			name, tries, attempts, d.Round(time.Second))
	}

	policy := backoff.WithContext(backoff.WithMaxRetries(eb, uint64(attempts-1)), ctx)
	err := backoff.RetryNotify(op, policy, notify)
	if err == nil {
		return nil
	}
	// A cancelled push is a cancellation, not a registry fault: oap builds images
	// concurrently under errgroup.WithContext, so a sibling's permanent failure
	// tears this one down mid-flight. Blaming that on the proxy would send the
	// user after the wrong problem.
	if cerr := ctx.Err(); cerr != nil && errors.Is(err, cerr) {
		return fmt.Errorf("pushing %s was canceled: %w", name, err)
	}
	translated := TranslatePushError(name, registry, lastStderr, err)
	if tries > 1 {
		return fmt.Errorf("after %d attempts: %w", tries, translated)
	}
	return translated
}
