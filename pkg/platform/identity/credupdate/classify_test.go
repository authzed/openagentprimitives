package credupdate_test

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/authzed/openagentprimitives/pkg/platform/identity/credupdate"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/provider"
)

// int32Ptr is a small helper so table rows can express "an exit code was
// observed" (a non-nil pointer) without repeating `func() *int32 { ... }()`
// inline everywhere.
func int32Ptr(v int32) *int32 { return &v }

func TestIsAuthShaped(t *testing.T) {
	cases := []struct {
		name string
		obs  credupdate.Observation
		af   *provider.AuthFailure
		want bool
	}{
		{
			name: "nil AuthFailure: no corroboration available, never a default-yes",
			obs:  credupdate.Observation{HTTPStatus: 401},
			af:   nil,
			want: false,
		},
		{
			name: "block present, HTTPStatuses empty, observed 401: the documented 401-only default",
			obs:  credupdate.Observation{HTTPStatus: 401},
			af:   &provider.AuthFailure{},
			want: true,
		},
		{
			// LOAD-BEARING SECURITY TEST. See defaultHTTPStatuses' doc comment in
			// classify.go: an LLM cannot forge a 401, but it CAN provoke a 403 by
			// deliberately requesting a resource it isn't entitled to. If a 403
			// counted by default, a prompt-injected agent pairing a manufactured
			// 403 with an unreachable-provider probe could talk a human into
			// re-entering a WORKING credential. 403 must never be in the default.
			name: "SECURITY: block present, HTTPStatuses empty, observed 403 -> false (403 is never a default)",
			obs:  credupdate.Observation{HTTPStatus: 403},
			af:   &provider.AuthFailure{},
			want: false,
		},
		{
			name: "httpStatuses: [403] declared explicitly, observed 403: opt-in works",
			obs:  credupdate.Observation{HTTPStatus: 403},
			af:   &provider.AuthFailure{HTTPStatuses: []int{403}},
			want: true,
		},
		{
			name: "observed 500: not an auth-shaped status under the default or under any declared block",
			obs:  credupdate.Observation{HTTPStatus: 500},
			af:   &provider.AuthFailure{},
			want: false,
		},
		{
			name: "exit code matches a declared exitCodes entry",
			obs:  credupdate.Observation{ExitCode: int32Ptr(1)},
			af:   &provider.AuthFailure{ExitCodes: []int{1}},
			want: true,
		},
		{
			name: "exit code observed but the block declares only HTTP statuses: exitCodes never defaults",
			obs:  credupdate.Observation{ExitCode: int32Ptr(1)},
			af:   &provider.AuthFailure{HTTPStatuses: []int{401}},
			want: false,
		},
		{
			name: "stderr matches a declared pattern",
			obs:  credupdate.Observation{Stderr: "Error: Unauthorized access to repository"},
			af:   &provider.AuthFailure{StderrPatterns: []string{`(?i)unauthorized`}},
			want: true,
		},
		{
			name: "stderr observed but matches nothing declared",
			obs:  credupdate.Observation{Stderr: "Error: file not found"},
			af:   &provider.AuthFailure{StderrPatterns: []string{`(?i)unauthorized`}},
			want: false,
		},
		{
			name: "zero-value Observation: no status, nil exit code, empty stderr -> nothing was observed",
			obs:  credupdate.Observation{},
			af:   &provider.AuthFailure{HTTPStatuses: []int{401}, ExitCodes: []int{1}, StderrPatterns: []string{`(?i)unauthorized`}},
			want: false,
		},
		{
			// The provider AUTHENTICATED this call and then answered with a
			// tool-level error. Whatever else the observation carries, the
			// credential demonstrably works, so nothing here is an
			// authentication failure.
			name: "SECURITY: origin authenticated, so a matching stderr pattern must NOT corroborate",
			obs:  credupdate.Observation{Stderr: "Error: Unauthorized access to repository", OriginAuthenticated: true},
			af:   &provider.AuthFailure{StderrPatterns: []string{`(?i)unauthorized`}},
			want: false,
		},
		{
			name: "SECURITY: origin authenticated, so a matching exit code must NOT corroborate",
			obs:  credupdate.Observation{ExitCode: int32Ptr(1), OriginAuthenticated: true},
			af:   &provider.AuthFailure{ExitCodes: []int{1}},
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := credupdate.IsAuthShaped(tc.obs, tc.af)
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestIsAuthShaped_MultipleSignalsAnyMatch pins that HTTP, exit code, and
// stderr are independent OR'd signals: a declared block that covers all
// three matches on any one of them being observed, not all of them at once.
func TestIsAuthShaped_MultipleSignalsAnyMatch(t *testing.T) {
	af := &provider.AuthFailure{
		HTTPStatuses:   []int{401},
		ExitCodes:      []int{1},
		StderrPatterns: []string{`(?i)unauthorized`},
	}

	assert.True(t, credupdate.IsAuthShaped(credupdate.Observation{HTTPStatus: 401}, af), "HTTP signal alone must match")
	assert.True(t, credupdate.IsAuthShaped(credupdate.Observation{ExitCode: int32Ptr(1)}, af), "exit code signal alone must match")
	assert.True(t, credupdate.IsAuthShaped(credupdate.Observation{Stderr: "unauthorized"}, af), "stderr signal alone must match")
	assert.False(t, credupdate.IsAuthShaped(credupdate.Observation{HTTPStatus: 200, ExitCode: int32Ptr(0), Stderr: "ok"}, af), "no signal matching must refuse")
}

// TestIsAuthShaped_ExitCodeZeroIsARealObservation pins that a non-nil pointer
// to 0 is a real "the process exited 0" observation, distinct from "no exit
// code observed" (nil).
//
// The distinction is what makes `exitCodes: [0]` a REJECTABLE declaration
// rather than an invisible one: collapsing both onto a bare zero would make
// every unobserved call look like an exit-0 call, and no amount of validation
// downstream could tell them apart. Load-time validation does reject that
// declaration (validateAuthFailureConfig — 0 means success, so declaring it
// inverts the signal), so a block like this one cannot reach the classifier
// from the shipped catalog; the pure function is exercised here directly.
func TestIsAuthShaped_ExitCodeZeroIsARealObservation(t *testing.T) {
	af := &provider.AuthFailure{ExitCodes: []int{0}}

	assert.True(t, credupdate.IsAuthShaped(credupdate.Observation{ExitCode: int32Ptr(0)}, af),
		"a non-nil pointer to 0 is an observed exit code and must match a declared 0")
	assert.False(t, credupdate.IsAuthShaped(credupdate.Observation{ExitCode: nil}, af),
		"a nil exit code means nothing was observed, even though 0 is declared")
}

// TestIsAuthShaped_AuthenticationBeatsEveryDeclaredShape pins the override:
// OriginAuthenticated is not one more signal to weigh, it is a veto.
//
// It matters because the two weakest shapes are the ones an agent can steer.
// argv is model-authored, so a CLI that exits 1 on a bad flag, or echoes an
// argument into its own stderr, hands the classifier a "match" that the agent
// manufactured. When the same call also proves the credential was accepted,
// the proof has to win — otherwise a prompt-injected agent can keep a stale
// observation alive and, for a provider with no verify: probe, walk a human
// all the way to a credential-entry form for a working credential.
func TestIsAuthShaped_AuthenticationBeatsEveryDeclaredShape(t *testing.T) {
	af := &provider.AuthFailure{
		HTTPStatuses:   []int{401},
		ExitCodes:      []int{1},
		StderrPatterns: []string{`(?i)unauthorized`},
	}

	authenticated := credupdate.Observation{
		HTTPStatus:          401,
		ExitCode:            int32Ptr(1),
		Stderr:              "unauthorized",
		OriginAuthenticated: true,
	}
	assert.False(t, credupdate.IsAuthShaped(authenticated, af),
		"every declared shape matches, and the call still authenticated: proof beats pattern")

	// The same observation without the proof is the genuine failure, and must
	// still corroborate — the veto is the only thing that changed.
	unauthenticated := authenticated
	unauthenticated.OriginAuthenticated = false
	assert.True(t, credupdate.IsAuthShaped(unauthenticated, af),
		"the veto must not swallow a real auth failure")
}

// TestIsAuthShaped_ConcurrentCallsAreRaceFree exercises IsAuthShaped from
// many goroutines at once. classify.go deliberately compiles StderrPatterns
// per call rather than through a package-level cache (see the doc comment
// on matchesAnyPattern) — this test is the proof that choice carries no
// shared-mutable-state race, so it must be run with `go test -race`.
func TestIsAuthShaped_ConcurrentCallsAreRaceFree(t *testing.T) {
	af := &provider.AuthFailure{
		HTTPStatuses:   []int{401},
		ExitCodes:      []int{1},
		StderrPatterns: []string{`(?i)unauthorized`, `(?i)forbidden`},
	}

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			obs := credupdate.Observation{Stderr: "request unauthorized"}
			if n%2 == 0 {
				obs = credupdate.Observation{HTTPStatus: 401}
			}
			assert.True(t, credupdate.IsAuthShaped(obs, af))
		}(i)
	}
	wg.Wait()
}
