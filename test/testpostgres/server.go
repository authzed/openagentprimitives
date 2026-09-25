// Package testpostgres spins up a PostgreSQL container for tests that need a
// real database, mirroring test/testspicedb's shape: one container per test
// BINARY (lazily, on first call), an explicit Stop() the package's TestMain
// calls after m.Run(), and a clean skip when the container cannot be started.
//
// Why a container at all: pkg/memory/postgres is the durable memory backend
// every remote install runs, and its ~20 tests skipped unconditionally unless a
// developer happened to have POSTGRES_URI pointing at a database. A suite that
// silently proves nothing is worse than a suite that fails, because it reads as
// coverage on the dashboard.
//
// Why NOT unconditional: making `mage test:unit` require a Docker daemon would
// turn the fastest, most-run gate in the repo into one that fails on a laptop
// with Docker stopped, for every package. So the container is opt-in via
// AP_TEST_POSTGRES=docker, which `mage test:postgres` and `mage
// test:integration` both set — the integration tier already requires Docker for
// envtest and SpiceDB, so joining it there adds a database but no new
// prerequisite, and puts the backend into the ship gate.
package testpostgres

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/ory/dockertest/v3"
	"github.com/ory/dockertest/v3/docker"
)

// EnvMode names the opt-in that allows starting a container. Set it to
// ModeDocker to have URI start one; leave it unset and URI skips instead.
const (
	EnvMode    = "AP_TEST_POSTGRES"
	ModeDocker = "docker"

	// EnvURI is the operator-supplied override, checked first: an already-running
	// database (CI service, a local server) always wins over starting a container.
	EnvURI = "POSTGRES_URI"

	// MageTarget is named in the skip message so a developer who runs the unit
	// tier and sees "skipped" learns, from the skip itself, how to actually run
	// these tests. A skip that does not say what to do next is how twenty tests
	// stay dark for months.
	MageTarget = "mage test:postgres"
)

var (
	sharedOnce sync.Once
	sharedURI  string
	sharedRes  *dockertest.Resource
	sharedPool *dockertest.Pool
	sharedErr  error
)

// URI returns a PostgreSQL connection URI for the calling test, or skips the
// test when no database is available.
//
// Resolution order:
//  1. POSTGRES_URI — an operator-supplied database, used verbatim.
//  2. AP_TEST_POSTGRES=docker — start ONE container for this test binary
//     (subsequent calls return the same URI).
//  3. neither — t.Skip naming MageTarget.
//
// The container is shared by every test in the binary; it is NOT reset between
// tests. Callers own their isolation exactly as they do against an
// operator-supplied database — truncate in a t.Cleanup, or scope rows by a
// per-test key.
func URI(t *testing.T) string {
	t.Helper()
	if uri := os.Getenv(EnvURI); uri != "" {
		return uri
	}
	if os.Getenv(EnvMode) != ModeDocker {
		t.Skipf("no PostgreSQL available: set %s to an existing database, or run `%s` (which sets %s=%s to start a container)",
			EnvURI, MageTarget, EnvMode, ModeDocker)
	}
	sharedOnce.Do(func() {
		sharedURI, sharedRes, sharedPool, sharedErr = startContainer()
	})
	if sharedErr != nil {
		// Skip rather than fail: the same policy testspicedb uses. A developer
		// without a working Docker daemon gets a clear skip, not a red suite.
		// The error is carried into the message so the cause is not swallowed.
		t.Skipf("could not start postgres container (is docker running?): %v", sharedErr)
	}
	return sharedURI
}

// Stop purges the shared container. Safe to call when none was started (no-op).
// Call it from the package's TestMain after m.Run() returns, so the container is
// removed before the process exits.
func Stop() {
	if sharedRes != nil && sharedPool != nil {
		if err := sharedPool.Purge(sharedRes); err != nil {
			// We are in TestMain, outside any *testing.T — stderr is the only
			// surface. Never drop it silently (AGENTS.md): a purge failure is
			// exactly how a container graveyard accumulates.
			fmt.Fprintf(os.Stderr, "testpostgres: purge container: %v\n", err)
		}
		sharedRes, sharedPool, sharedURI = nil, nil, ""
	}
}

const (
	// postgresImage is pinned to a major version rather than `latest` so a
	// silently-bumped image cannot change SQL semantics under the suite. The
	// plain image (no pgvector) is deliberate: pkg/memory/postgres's migrateSQL
	// declares no vector column — the embedding column belongs to
	// pkg/memory/search/postgres, which has its own fixture needs.
	postgresImage = "postgres"
	postgresTag   = "16-alpine"

	testUser = "aptest"
	testPass = "aptest"
	testDB   = "aptest"

	// readyTimeout bounds the wait for the server to accept connections. The
	// image initializes a fresh data directory on first boot, which dominates.
	readyTimeout = 90 * time.Second
)

// startContainer launches a PostgreSQL container, waits until it accepts
// connections, and returns its URI along with the resource + pool handles so
// Stop can purge it.
func startContainer() (uri string, res *dockertest.Resource, pool *dockertest.Pool, err error) {
	pool, err = dockertest.NewPool("")
	if err != nil {
		return "", nil, nil, fmt.Errorf("connect to docker: %w", err)
	}
	pool.MaxWait = readyTimeout

	res, err = pool.RunWithOptions(&dockertest.RunOptions{
		Repository: postgresImage,
		Tag:        postgresTag,
		Env: []string{
			"POSTGRES_USER=" + testUser,
			"POSTGRES_PASSWORD=" + testPass,
			"POSTGRES_DB=" + testDB,
			// The data directory is thrown away with the container, so durability
			// buys nothing and costs a fsync on every commit — which the write-heavy
			// round-trip tests pay per statement.
			"PGDATA=/var/lib/postgresql/data/pgdata",
		},
		Cmd:          []string{"postgres", "-c", "fsync=off", "-c", "full_page_writes=off"},
		ExposedPorts: []string{"5432/tcp"},
	}, func(hc *docker.HostConfig) {
		// AutoRemove makes the daemon delete the container the moment it stops,
		// so a SIGKILLed test process leaves no stopped-container graveyard
		// behind — the failure mode `docker ps -a` on this machine shows for the
		// SpiceDB fixture, whose Expire() only stops and never removes.
		hc.AutoRemove = true
		hc.RestartPolicy = docker.RestartPolicy{Name: "no"}
	})
	if err != nil {
		return "", nil, nil, fmt.Errorf("run postgres container: %w", err)
	}

	uri = fmt.Sprintf("postgres://%s:%s@localhost:%s/%s?sslmode=disable",
		testUser, testPass, res.GetPort("5432/tcp"), testDB)

	// Only return nil from the probe when a real connection succeeds AND a
	// trivial query round-trips. The image briefly accepts TCP during initdb
	// while still refusing queries; returning early there races the caller's
	// Migrate.
	if retryErr := pool.Retry(func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		p, dialErr := pgxpool.New(ctx, uri)
		if dialErr != nil {
			return dialErr
		}
		defer p.Close()
		var one int
		return p.QueryRow(ctx, "SELECT 1").Scan(&one)
	}); retryErr != nil {
		if purgeErr := pool.Purge(res); purgeErr != nil {
			// Compound the errors rather than lose the purge failure: a half-started
			// container that also failed to purge is a leak worth naming.
			return "", nil, nil, fmt.Errorf("postgres container did not become ready: %w (purge also failed: %v)", retryErr, purgeErr)
		}
		return "", nil, nil, fmt.Errorf("postgres container did not become ready: %w", retryErr)
	}

	return uri, res, pool, nil
}
