//go:build integration || e2e

// Package testspicedb spins up an authzed/spicedb container in
// `serve-testing` mode for integration tests. Endpoint(t) starts a
// container scoped to the calling test and registers a t.Cleanup that
// purges it when the test finishes (pass or fail) — so a `go test` run
// never leaks containers. Per-test isolation also comes from each test
// choosing its own bearer token: `serve-testing` keys datastores by
// token, so different tokens see different datastores.
//
// Pattern lifted from authzed/spicedb's own integration tests
// (cmd/spicedb/servetesting_integration_test.go).
package testspicedb

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	v1 "github.com/authzed/authzed-go/proto/authzed/api/v1"
	"github.com/authzed/grpcutil"
	"github.com/ory/dockertest/v3"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	authzschema "github.com/authzed/openagentprimitives/pkg/authz/spicedb/schema"
)

// Schema is the canonical agentprimitives SpiceDB schema (pkg/authz/spicedb/schema),
// written verbatim into each test datastore at fixture startup. Sourced from
// the single embedded copy so test fixtures never drift from the operator's
// runtime schema.
var Schema = authzschema.Schema

// endpointMemo caches the spicedb container address per *testing.T.
// Endpoint(t) is documented to give "each test its own container", and
// callers rely on that: a test may resolve the endpoint in more than one
// place (e.g. once in a connection helper that writes the schema, and
// again in the test body that reads it). Without memoization each call to
// start() launches a SEPARATE container, so the write and the read
// silently target different datastores and the read sees no schema.
// Keying the memo on *testing.T scopes exactly one container per test.
var (
	endpointMemoMu sync.Mutex
	endpointMemo   = map[*testing.T]string{}
)

// Endpoint starts an authzed/spicedb container in serve-testing mode,
// scoped to the calling test, and returns its host:port. A t.Cleanup
// registered on t purges the container when the test finishes (pass or
// fail), so a `go test` run never leaks spicedb containers. If Docker is
// unreachable the test is skipped; if Docker is reachable and spicedb still
// fails to come up, the test fails.
//
// Each test gets its own container, and repeated Endpoint(t) calls within
// one test return the SAME container (memoized per *testing.T). Call
// WriteSchema once to load the schema, and pass a UNIQUE bearer token so
// serve-testing routes the test to its own isolated datastore.
func Endpoint(t *testing.T) string {
	t.Helper()
	endpointMemoMu.Lock()
	defer endpointMemoMu.Unlock()
	if addr, ok := endpointMemo[t]; ok {
		return addr
	}
	addr, err := start(t)
	if errors.Is(err, ErrDockerUnavailable) {
		t.Skipf("skipping: %v", err)
	}
	if err != nil {
		t.Fatalf("testspicedb: docker is reachable but spicedb would not start: %v", err)
	}
	endpointMemo[t] = addr
	t.Cleanup(func() {
		endpointMemoMu.Lock()
		delete(endpointMemo, t)
		endpointMemoMu.Unlock()
	})
	return addr
}

// containerStartAttempts bounds retries of the run+probe cycle.
//
// Under a heavily parallel e2e run the probe can land on a port Docker has just
// recycled from another service, which surfaces as a gRPC handshake error
// ("frame too large, note that the frame header looked like an HTTP/1.1
// header") rather than as an unreachable port. That is transient and a fresh
// container gets a fresh port, so it is worth retrying — but only a couple of
// times, because a genuinely broken image must still fail rather than spin.
const containerStartAttempts = 3

// startContainer launches a spicedb container, retrying transient startup
// failures. See startContainerOnce for the single-attempt contract.
func startContainer() (string, *dockertest.Resource, *dockertest.Pool, error) {
	var lastErr error
	for attempt := 1; attempt <= containerStartAttempts; attempt++ {
		addr, res, pool, err := startContainerOnce()
		if err == nil {
			return addr, res, pool, nil
		}
		// No Docker is terminal: retrying cannot conjure a daemon.
		if errors.Is(err, ErrDockerUnavailable) {
			return "", nil, nil, err
		}
		lastErr = err
		fmt.Fprintf(os.Stderr, "testspicedb: container start attempt %d/%d failed: %v\n",
			attempt, containerStartAttempts, err)
	}
	return "", nil, nil, fmt.Errorf("after %d attempts: %w", containerStartAttempts, lastErr)
}

// startContainerOnce makes ONE attempt to launch a spicedb container and wait
// for it to be ready, returning the host:port address along with the resource
// and pool handles so the caller can purge the container when it is no longer
// needed. The Docker daemon also force-removes the container after 1200 s
// (resource.Expire backstop) so a SIGKILLed process cannot leak it. A failed
// readiness probe purges its own container before returning.
func startContainerOnce() (addr string, res *dockertest.Resource, pool *dockertest.Pool, err error) {
	pool, err = dockertest.NewPool("")
	if err != nil {
		// The ONLY skippable outcome: this machine has no reachable Docker
		// daemon, so it cannot run these tests at all. Every failure after this
		// point means Docker works and SpiceDB is genuinely broken — those must
		// fail, not skip, or a whole suite reports green having run nothing.
		return "", nil, nil, fmt.Errorf("%w: %v", ErrDockerUnavailable, err)
	}
	pool.MaxWait = 60 * time.Second

	res, err = pool.RunWithOptions(&dockertest.RunOptions{
		Repository:   "authzed/spicedb",
		Tag:          "v1.56.2",
		Cmd:          []string{"serve-testing", "--skip-release-check"},
		ExposedPorts: []string{"50051/tcp"},
		// Label every container this fixture starts. `docker ps
		// --filter ancestor=authzed/spicedb` cannot tell a suite's container from
		// a developer's own SpiceDB running on the same machine, which is exactly
		// the distinction anything that counts — or reaps — them needs. Setting
		// AP_TEST_RUN_ID additionally scopes containers to one `go test`
		// invocation, so a measurement taken while another suite runs is still
		// exact.
		Labels: map[string]string{
			LabelFixture:    LabelFixtureValue,
			LabelOwnerPID:   strconv.Itoa(os.Getpid()),
			LabelOwnerStart: SelfStartToken(),
			LabelRunID:      os.Getenv(EnvRunID),
		},
	})
	if err != nil {
		return "", nil, nil, fmt.Errorf("run spicedb container: %w", err)
	}

	// Having just paid for a docker round trip, sweep up any container this
	// fixture started for a process that is now gone. See reapAbandoned.
	reapAbandoned(pool)

	// Hard backstop for a test process that dies without running its cleanups
	// (SIGKILL, `go test -timeout` kill, a panic in TestMain). dockertest's
	// Expire issues a docker `stop` with this many seconds of grace, and the
	// daemon carries that out even after the client disconnects: containers
	// left behind on this machine show FinishedAt exactly StartedAt+1200s with
	// exit 137, which is what proves the backstop fires.
	//
	// Two consequences worth knowing, because the observable behaviour is not
	// what "expire" suggests:
	//   - It STOPS, it does not REMOVE. A killed run leaves an `Exited (137)`
	//     container behind; `docker ps -a` accumulates them.
	//   - The clock starts HERE, at container creation — not at last use. So
	//     this doubles as a hard cap on a shared container's life: a single test
	//     binary that runs longer than the TTL loses SpiceDB mid-run. 1200s is
	//     an order of magnitude above the slowest package today (~150s), but it
	//     is a ceiling, not a grace period.
	//
	// Expire returns nil unconditionally in dockertest v3.12 (the stop happens
	// in a goroutine whose error is dropped upstream), so there is nothing to
	// check here.
	_ = res.Expire(1200)

	port := res.GetPort("50051/tcp")
	addr = "localhost:" + port

	// Wait for the gRPC service to come up. We dial with an arbitrary
	// token here — serve-testing accepts any bearer and gives that
	// token's datastore. We don't care about state on this token; we
	// only want to confirm the service is reachable.
	//
	// Critical: only return nil from the retry probe when we get a
	// REAL response from the service. ReadSchema on a fresh datastore
	// returns NotFound ("no schema has been defined") — that proves the
	// service is serving. Connection errors, Unavailable, etc. mean the
	// gRPC server hasn't bound the port yet; keep retrying. Returning
	// nil on connection errors races against the schema write that follows.
	probeToken := "probe-" + fmt.Sprint(time.Now().UnixNano())
	if retryErr := pool.Retry(func() error {
		conn, dialErr := grpc.NewClient(addr,
			grpc.WithTransportCredentials(insecure.NewCredentials()),
			grpcutil.WithInsecureBearerToken(probeToken),
		)
		if dialErr != nil {
			return dialErr
		}
		defer func() { _ = conn.Close() }()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_, readErr := v1.NewSchemaServiceClient(conn).ReadSchema(ctx, &v1.ReadSchemaRequest{})
		if readErr == nil {
			return nil
		}
		// NotFound = "no schema has been defined", which is what we
		// expect on a fresh serve-testing datastore. Service is up.
		if status.Code(readErr) == codes.NotFound {
			return nil
		}
		// Anything else (Unavailable, connection reset, etc.): retry.
		return readErr
	}); retryErr != nil {
		_ = pool.Purge(res) // failed start — purge immediately
		return "", nil, nil, fmt.Errorf("spicedb container did not become ready: %w", retryErr)
	}

	return addr, res, pool, nil
}

// reapAbandoned removes containers THIS fixture started whose creating process
// no longer exists. Best-effort: failures are reported, never fatal.
//
// It exists because a shared container's purge runs from a t.Cleanup or a
// TestMain, and a test binary can exit without either — the last test in a
// package arms the idle purge and the process exits before the timer fires, and
// a SIGKILLed run never runs cleanups at all. Without a sweep, a `mage test:e2e`
// run over 39 scenario binaries accumulates one idle container per finished
// package for the rest of the run. With it, each binary's first container start
// clears the ones whose owners have already exited.
//
// This is only half the story, and knowing which half matters: it runs when a
// NEW container starts, so nothing sweeps after the LAST binary of a suite
// exits. That residue is what the magefile's post-run sweep removes, using
// these same rules through ReapWith.
//
// Every safety rule lives in Classify — see the contract there.
func reapAbandoned(pool *dockertest.Pool) {
	res, err := ReapWith(pool.Client, os.Getpid(), "", LiveOwnerChecker())
	if err != nil {
		fmt.Fprintf(os.Stderr, "testspicedb: sweep abandoned fixture containers: %v\n", err)
		return
	}
	for _, d := range res.Kept {
		if d.Err != nil {
			fmt.Fprintf(os.Stderr, "testspicedb: reap abandoned container %s (%s): %v\n",
				shortID(d.ID), d.Reason, d.Err)
		}
	}
}

// shortID trims a docker ID to the 12 characters the CLI displays.
func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// start launches a spicedb container scoped to the calling test and registers a
// t.Cleanup that purges the container when the test finishes (pass or fail).
func start(t *testing.T) (string, error) {
	t.Helper()
	addr, res, pool, err := startContainer()
	if err != nil {
		return "", err
	}

	// The container is up. Purge it when the calling test finishes —
	// this is what keeps a `go test` run from leaking spicedb
	// containers. ory/dockertest has no "after all tests" hook, so each
	// test owns (and cleans up) its own container rather than sharing.
	t.Cleanup(func() {
		if purgeErr := pool.Purge(res); purgeErr != nil {
			t.Logf("testspicedb: purge spicedb container: %v", purgeErr)
		}
	})

	return addr, nil
}

// WriteSchema writes Schema using the given token's datastore. Tests
// call this once per fixture so each isolated datastore has the schema
// loaded before any relationship writes.
func WriteSchema(t *testing.T, endpoint, token string) {
	t.Helper()
	WriteSchemaText(t, endpoint, token, Schema)
}

// WriteSchemaWithExtra writes the canonical Schema plus additional
// definitions. Slot resources are composer-injected per AgentClass rather
// than part of the canonical schema, so a test that needs one has to supply
// it — this keeps that fixture from re-implementing the write.
func WriteSchemaWithExtra(t *testing.T, endpoint, token, extra string) {
	t.Helper()
	WriteSchemaText(t, endpoint, token, Schema+"\n"+extra)
}

// WriteSchemaText writes arbitrary schema text to the given token's datastore.
func WriteSchemaText(t *testing.T, endpoint, token, schema string) {
	t.Helper()
	conn, err := grpc.NewClient(endpoint,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpcutil.WithInsecureBearerToken(token),
	)
	if err != nil {
		t.Fatalf("dial spicedb: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	// 30s (not 5s): under the full -race integration suite the SpiceDB
	// container is CPU-starved, and a single-shot schema write (dozens of
	// object definitions) can exceed a tight deadline — flaking unrelated
	// tests at fixture setup. Generous headroom; in isolation the write is
	// sub-second. This call is not retried, so the deadline must absorb load.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := v1.NewSchemaServiceClient(conn).WriteSchema(ctx, &v1.WriteSchemaRequest{
		Schema: schema,
	}); err != nil {
		t.Fatalf("write schema: %v", err)
	}
}

// UniqueToken returns a per-test bearer token. serve-testing routes
// each token to its own in-memory datastore, so distinct tokens yield
// distinct test isolation domains.
func UniqueToken(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("test-%s-%d", sanitize(t.Name()), time.Now().UnixNano())
}

func sanitize(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
			out = append(out, c)
		default:
			out = append(out, '-')
		}
	}
	return string(out)
}
