package buildx_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/buildx"
)

// proxyTimeout is the stderr Docker Desktop produces when the daemon cannot
// open a connection to its own configured HTTP proxy — the failure this
// package's retry exists for.
const proxyTimeout = `ERROR: failed to build: Put "https://us-east1-docker.pkg.dev/v2/p/ap/toolchain-node/manifests/dev": proxyconnect tcp: dial tcp 192.168.65.1:3128: i/o timeout`

// clientTimeout is the other shape the same overloaded machine produces: the
// dial succeeds, so nothing in the dial vocabulary appears, and the failure
// surfaces as Go's http.Client giving up while awaiting response headers.
const clientTimeout = `ERROR: failed to build: Error response from daemon: Get "https://us-east1-docker.pkg.dev/v2/": net/http: request canceled while waiting for connection (Client.Timeout exceeded while awaiting headers)`

// fastRetry keeps the backoff out of the test's wall clock. Attempts stays at
// the production default so the exhaustion cases assert the real count.
var fastRetry = buildx.Retry{Attempts: 3, InitialWait: time.Millisecond}

func TestMetadataDigest(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    string
		wantErr string
	}{
		{name: "digest present: extracted verbatim", in: `{"containerimage.digest":"sha256:abc"}`, want: "sha256:abc"},
		{name: "key absent: error names the missing key", in: `{"other":"x"}`, wantErr: "no containerimage.digest"},
		{name: "empty digest: error, never an empty ref", in: `{"containerimage.digest":""}`, wantErr: "is empty"},
		{name: "non-sha256 digest: error, not passed through", in: `{"containerimage.digest":"md5:abc"}`, wantErr: "not a sha256 digest"},
		{name: "malformed json: parse error", in: `{`, wantErr: "parse buildx metadata"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := buildx.MetadataDigest([]byte(tc.in))
			if tc.wantErr != "" {
				assert.ErrorContains(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestTranslatePushError(t *testing.T) {
	const reg = "us-east1-docker.pkg.dev/my-proj/ap"
	cases := []struct {
		name   string
		stderr string
		want   string
	}{
		{name: "denied names the docker auth remedy", stderr: "denied: permission", want: "gcloud auth configure-docker us-east1-docker.pkg.dev"},
		{name: "unauthenticated names the docker auth remedy", stderr: "Unauthenticated request", want: "gcloud auth configure-docker"},
		{name: "missing repo names --create-registry", stderr: "NOT_FOUND: repo", want: "--create-registry"},
		{name: "missing repo also covers oap agent install, which has no --create-registry", stderr: "NOT_FOUND: repo", want: "oap agent install"},
		{name: "proxy timeout names the daemon proxy bypass", stderr: proxyTimeout, want: "bypass"},
		{name: "client timeout names the network remedy, not buildx setup", stderr: clientTimeout, want: "could not complete the transfer"},
		{name: "proxy timeout names the registry host to bypass", stderr: proxyTimeout, want: "us-east1-docker.pkg.dev"},
		{name: "anything else names buildx setup", stderr: "boom", want: "docker buildx create --use"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := buildx.TranslatePushError("demo-image", reg, tc.stderr, errors.New("exit 1"))
			assert.ErrorContains(t, err, tc.want)
		})
	}

	// The buildx-setup remedy is the default arm's advice and is actively
	// misleading for a network fault: buildx demonstrably worked, since it built
	// the image and uploaded its layers before the manifest PUT lost the proxy.
	t.Run("proxy timeout does not send the user to fix buildx", func(t *testing.T) {
		err := buildx.TranslatePushError("demo-image", reg, proxyTimeout, errors.New("exit 1"))
		assert.NotContains(t, err.Error(), "docker buildx create --use")
	})
}

func TestRetryable(t *testing.T) {
	cases := []struct {
		name   string
		stderr string
		want   bool
	}{
		{name: "proxy dial timeout: retryable", stderr: proxyTimeout, want: true},
		{name: "TLS handshake timeout: retryable", stderr: "net/http: TLS handshake timeout", want: true},
		// Go's http.Client surfaces a stalled registry as a client-side timeout
		// rather than a dial error, so it shares none of the dial vocabulary. The
		// first cut of this list was enumerated from one observed sample and
		// missed the entire family below, which left the common case unretried.
		{name: "client timeout awaiting headers: retryable", stderr: `Error response from daemon: Get "https://us-east1-docker.pkg.dev/v2/": net/http: request canceled while waiting for connection (Client.Timeout exceeded while awaiting headers)`, want: true},
		{name: "request canceled mid-flight: retryable", stderr: "net/http: request canceled", want: true},
		{name: "response header timeout: retryable", stderr: "net/http: timeout awaiting response headers", want: true},
		{name: "context deadline exceeded: retryable", stderr: `Get "https://reg/v2/": context deadline exceeded`, want: true},
		{name: "broken pipe mid-upload: retryable", stderr: "write tcp 10.0.0.1:5000->1.2.3.4:443: write: broken pipe", want: true},
		{name: "closed network connection: retryable", stderr: "use of closed network connection", want: true},
		{name: "http2 connection lost: retryable", stderr: "http2: client connection lost", want: true},
		{name: "server misbehaving: retryable", stderr: "dial tcp: lookup reg on 1.1.1.1:53: server misbehaving", want: true},
		{name: "connection reset: retryable", stderr: "read tcp: connection reset by peer", want: true},
		{name: "registry 503: retryable", stderr: "unexpected status: 503 Service Unavailable", want: true},
		{name: "auth denied: not retryable, retrying only delays the real remedy", stderr: "denied: permission", want: false},
		{name: "missing repo: not retryable, the repo will not appear on its own", stderr: "NOT_FOUND: repo", want: false},
		{name: "unclassified failure: not retryable", stderr: "boom", want: false},
		// `docker buildx build --push` builds AND pushes in one invocation, so the
		// widened network vocabulary must not start retrying genuine build
		// failures — that would triple the wait on a broken Dockerfile.
		{name: "dockerfile step failed: not retryable, the build is broken not the link", stderr: `ERROR: failed to solve: process "/bin/sh -c npm ci" did not complete successfully: exit code: 1`, want: false},
		// A denied push whose stderr also carries a timeout must stay permanent:
		// classification is fail-closed toward reporting the actionable fault.
		{name: "denied alongside a timeout: not retryable, auth wins", stderr: "denied: permission\n" + proxyTimeout, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, buildx.Retryable(tc.stderr))
		})
	}
}

func TestRunPush(t *testing.T) {
	const reg = "us-east1-docker.pkg.dev/my-proj/ap"

	t.Run("transient failure then success: retried, no error surfaced", func(t *testing.T) {
		calls := 0
		err := buildx.RunPush(context.Background(), io.Discard, "demo-image", reg, fastRetry,
			func(context.Context) (string, error) {
				calls++
				if calls == 1 {
					return proxyTimeout, errors.New("exit status 1")
				}
				return "", nil
			})
		require.NoError(t, err)
		assert.Equal(t, 2, calls, "must retry the transient failure exactly once before succeeding")
	})

	t.Run("permanent failure: attempted once, names the auth remedy", func(t *testing.T) {
		calls := 0
		err := buildx.RunPush(context.Background(), io.Discard, "demo-image", reg, fastRetry,
			func(context.Context) (string, error) {
				calls++
				return "denied: permission", errors.New("exit status 1")
			})
		require.Error(t, err)
		assert.Equal(t, 1, calls, "an auth failure must not be retried")
		assert.ErrorContains(t, err, "gcloud auth configure-docker")
	})

	t.Run("transient throughout: exhausts Attempts, then names the proxy remedy", func(t *testing.T) {
		calls := 0
		err := buildx.RunPush(context.Background(), io.Discard, "demo-image", reg, fastRetry,
			func(context.Context) (string, error) {
				calls++
				return proxyTimeout, errors.New("exit status 1")
			})
		require.Error(t, err)
		assert.Equal(t, fastRetry.Attempts, calls, "must make exactly Attempts attempts")
		assert.ErrorContains(t, err, "bypass")
		assert.ErrorContains(t, err, "after 3 attempts")
	})

	t.Run("retrying: each retry is announced with its wait", func(t *testing.T) {
		var log bytes.Buffer
		calls := 0
		err := buildx.RunPush(context.Background(), &log, "demo-image", reg, fastRetry,
			func(context.Context) (string, error) {
				calls++
				if calls == 1 {
					return proxyTimeout, errors.New("exit status 1")
				}
				return "", nil
			})
		require.NoError(t, err)
		assert.Contains(t, log.String(), "demo-image", "the retry notice must name the image being pushed")
		assert.Contains(t, log.String(), "retrying", "a silent retry leaves a stalled build unexplained")
	})

	// oap builds several images concurrently under errgroup.WithContext, so a
	// sibling's permanent failure cancels this push mid-retry. That is a
	// cancellation, not a proxy fault, and must not be reported as one.
	t.Run("canceled context: stops retrying and reports cancellation, not a proxy fault", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		calls := 0
		err := buildx.RunPush(ctx, io.Discard, "demo-image", reg,
			buildx.Retry{Attempts: 5, InitialWait: time.Hour},
			func(context.Context) (string, error) {
				calls++
				cancel()
				return proxyTimeout, errors.New("exit status 1")
			})
		require.Error(t, err)
		assert.ErrorIs(t, err, context.Canceled)
		assert.Equal(t, 1, calls, "cancellation must abort the retry loop, not wait out the backoff")
		assert.NotContains(t, err.Error(), "bypass", "a canceled push must not be blamed on the proxy")
	})

	t.Run("zero Retry: falls back to the package defaults", func(t *testing.T) {
		calls := 0
		err := buildx.RunPush(context.Background(), io.Discard, "demo-image", reg, buildx.Retry{},
			func(context.Context) (string, error) {
				calls++
				return "", nil
			})
		require.NoError(t, err)
		assert.Equal(t, 1, calls)
	})
}

func TestRegistryHost(t *testing.T) {
	assert.Equal(t, "us-east1-docker.pkg.dev", buildx.RegistryHost("us-east1-docker.pkg.dev/my-proj/ap"))
	assert.Equal(t, "myreg.io", buildx.RegistryHost("myreg.io"))
}
