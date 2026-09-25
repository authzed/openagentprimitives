package install

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Adopting a Secret OVERWRITES its data — a bundled CR's spec merely converges,
// but an answered secret value replaces whatever the existing Secret held. That
// is why AdoptAll has always carved Secrets out: seizing one has to be an
// individual act.
//
// Over HTTP it was not one. admind takes `adopt` as a JSON array in the same
// POST body as everything else, so naming "Secret/x" there is the same single
// request as a blanket adopt — the distinction the carve-out rests on does not
// exist on that surface. The hard refusal the ownership guard was built to make
// became a field the caller fills in.
//
// So the permission is now a property of the CALLER, expressed in InstallOpts
// and defaulting to refused. A surface that can put a specific object in front
// of a specific human and get a specific answer sets it; one that cannot,
// doesn't, and forgetting means no.

func TestResolveConflicts_SecretAdoptionRefusedWhenTheCallerMayNotAdoptSecrets(t *testing.T) {
	conflicts := []Conflict{{Kind: "Secret", Name: "app-token", Namespace: "apps", Secret: true}}

	_, err := resolveConflicts(context.Background(), conflicts, InstallOpts{Adopt: []string{"Secret/app-token"}})

	require.Error(t, err, "a caller that may not adopt Secrets must not seize one by naming it")
	assert.Contains(t, err.Error(), "Secret/app-token",
		"the refusal names the object, so the operator knows what to resolve out of band")
}

func TestResolveConflicts_SecretAdoptionAllowedForACallerThatMay(t *testing.T) {
	conflicts := []Conflict{{Kind: "Secret", Name: "app-token", Namespace: "apps", Secret: true}}

	adopted, err := resolveConflicts(context.Background(), conflicts, InstallOpts{
		Adopt:               []string{"Secret/app-token"},
		AdoptSecretsAllowed: true,
	})

	require.NoError(t, err)
	assert.Equal(t, []string{"Secret/app-token"}, adopted,
		"an operator naming one Secret at a terminal is the case the carve-out always allowed")
}

// The flag governs Secrets ONLY. A non-Secret conflict is adoptable by any
// caller, exactly as before — the guard here is about data overwrite, not about
// adoption in general, and widening it would break every admind install that
// legitimately adopts a CR.
func TestResolveConflicts_NonSecretAdoptionIsUnaffected(t *testing.T) {
	conflicts := []Conflict{{Kind: "MCPServer", Name: "gh", Namespace: "apps"}}

	adopted, err := resolveConflicts(context.Background(), conflicts, InstallOpts{Adopt: []string{"MCPServer/gh"}})

	require.NoError(t, err)
	assert.Equal(t, []string{"MCPServer/gh"}, adopted)
}

// The AdoptDecision hook is the interactive seam — a CLI multi-select, a
// desktop dialog — so a Secret returned from it IS an individual act and is
// governed by the same flag as an explicit key rather than by which code path
// produced the answer.
func TestResolveConflicts_SecretFromTheAdoptDecisionHookObeysTheSameFlag(t *testing.T) {
	conflicts := []Conflict{{Kind: "Secret", Name: "app-token", Namespace: "apps", Secret: true}}
	decide := func(context.Context, []Conflict) ([]string, error) { return []string{"Secret/app-token"}, nil }

	_, err := resolveConflicts(context.Background(), conflicts, InstallOpts{AdoptDecision: decide})
	require.Error(t, err, "the hook does not launder a Secret past a caller that may not adopt one")

	adopted, err := resolveConflicts(context.Background(), conflicts, InstallOpts{
		AdoptDecision: decide, AdoptSecretsAllowed: true,
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"Secret/app-token"}, adopted)
}
