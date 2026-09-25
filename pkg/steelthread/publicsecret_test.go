package steelthread_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/steelthread"
)

// publicIDValue is a value the shape of a GitHub App installation id: a bare
// integer that is not a credential, that GitHub itself puts into the body of
// every webhook it delivers, and that therefore cannot be kept out of a
// verbatim record of a delivery.
//
// Deliberately NOT credential-shaped. A value that both matched a structural
// pattern and was declared public would confuse the two claims this file has to
// keep apart.
const publicIDValue = "157401862"

// TestSelfCheck_APublicSecretKeyIsNotALeak is the fix, stated as a pair.
//
// The capture reads its live values out of Secrets, and it used to conclude
// from that alone that every one of them was a credential. A credential bundle
// is not shaped that way: a GitHub App's Secret holds a private key and a
// webhook secret beside an app id and an installation id, and GitHub publishes
// both of the latter. Treating provenance as sensitivity refused every triggered
// GitHub capture permanently — not fixable by redaction either, since the value
// belongs to the payload.
//
// Both rows run the SAME planted value through the SAME check, differing only
// in the declaration, so the pair proves the declaration is what moved the
// answer rather than something about the value.
func TestSelfCheck_APublicSecretKeyIsNotALeak(t *testing.T) {
	cases := []struct {
		name     string
		public   bool
		wantLeak bool
	}{
		{name: "declared public: emitted, no finding", public: true, wantLeak: false},
		{name: "not declared: hard secret-leak, exactly as before", public: false, wantLeak: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := cleanCapture(t)
			in.LiveSecrets = append(in.LiveSecrets, steelthread.LiveSecret{
				Name:   "demo-agent-creds/installation-id",
				Value:  publicIDValue,
				Public: tc.public,
			})
			plant(t, &in, "bundle.json", []byte("\n// "+publicIDValue+"\n"))

			got := steelthread.SelfCheck(in)
			if !tc.wantLeak {
				assert.Empty(t, got,
					"a published identifier the payload cannot omit must not refuse the capture")
				return
			}
			f := findByCode(t, got, steelthread.CodeSecretLeak)
			assert.Equal(t, steelthread.SeverityHard, f.Severity)
			assert.Contains(t, f.Message, "demo-agent-creds/installation-id")
		})
	}
}

// TestSelfCheck_TheZeroValueScans pins the fail-closed default in the only place
// it can be pinned: an entry a caller built without thinking about this field.
//
// The declaration is an opt-out for named public keys, never an opt-in for
// protection. A caller that does not know, or that forgets to ask the credential
// type, must get the whole Secret scanned — so adding a credential type cannot
// silently open a hole in the leak scan.
func TestSelfCheck_TheZeroValueScans(t *testing.T) {
	in := cleanCapture(t)
	// No Public field set at all. This is the shape of every call site that
	// predates the field, and of any new one that never learned about it.
	in.LiveSecrets = append(in.LiveSecrets, steelthread.LiveSecret{
		Name: "demo-agent-creds/some-new-key", Value: publicIDValue,
	})
	plant(t, &in, "bundle.json", []byte("\n// "+publicIDValue+"\n"))

	f := findByCode(t, steelthread.SelfCheck(in), steelthread.CodeSecretLeak)
	assert.Equal(t, steelthread.SeverityHard, f.Severity)
}

// TestSelfCheck_ARealCredentialInTheSamePublicSecretStillRefuses is the
// cross-check the fix is worth nothing without.
//
// One Secret, two keys: one declared public and one not. The narrowing must
// apply per KEY, not per Secret — a rule that let a Secret off once it had any
// public key in it would take a GitHub App's private key with it.
func TestSelfCheck_ARealCredentialInTheSamePublicSecretStillRefuses(t *testing.T) {
	const realSecretValue = "fixture-only-webhook-secret-0011223344"

	in := cleanCapture(t)
	in.LiveSecrets = append(in.LiveSecrets,
		steelthread.LiveSecret{Name: "demo-agent-creds/installation-id", Value: publicIDValue, Public: true},
		steelthread.LiveSecret{Name: "demo-agent-creds/webhook-secret", Value: realSecretValue},
	)
	plant(t, &in, "bundle.json", []byte("\n// "+publicIDValue+"\n// "+realSecretValue+"\n"))

	got := steelthread.SelfCheck(in)
	f := findByCode(t, got, steelthread.CodeSecretLeak)
	assert.Equal(t, steelthread.SeverityHard, f.Severity)
	assert.Contains(t, f.Message, "demo-agent-creds/webhook-secret",
		"the key that was NOT declared public is the one that must refuse")
	assert.NotContains(t, f.Message, "installation-id",
		"the declared-public key must not be reported alongside it")
	assert.NotContains(t, f.Message, realSecretValue, "a leak finding never reprints what it caught")

	for _, other := range got {
		if other.Code == steelthread.CodeSecretLeak {
			assert.NotContains(t, other.Message, "installation-id")
		}
	}
}

// TestSelfCheck_APublicDeclarationCannotLaunderCredentialShapedMaterial is the
// second layer, and the reason the narrowing above is safe to make at all.
//
// checkSecrets is the only check the declaration touches. The STRUCTURAL scan
// reads the same emitted bytes with no gate, no input to be given and no
// declaration to consult, so a key wrongly declared public whose value carries a
// recognizable credential shape is still refused — by a different check, with a
// different code. A declaration is not a suppression, and there is no ordering
// of these two checks in which it could become one.
func TestSelfCheck_APublicDeclarationCannotLaunderCredentialShapedMaterial(t *testing.T) {
	// A PEM private-key header: the one shape with no innocent reading.
	const pemHeader = "-----BEGIN RSA PRIVATE KEY-----"

	in := cleanCapture(t)
	in.LiveSecrets = append(in.LiveSecrets, steelthread.LiveSecret{
		Name: "demo-agent-creds/private-key", Value: pemHeader, Public: true,
	})
	plant(t, &in, "bundle.json", []byte("\n// "+pemHeader+"\n"))

	got := steelthread.SelfCheck(in)
	f := findByCode(t, got, steelthread.CodeStructuralSecret)
	assert.Equal(t, steelthread.SeverityHard, f.Severity)
	assert.Contains(t, f.Message, "bundle.json")
	assert.True(t, steelthread.HasHardFinding(got),
		"a declaration must never be able to make a capture with key material in it emit")
}

// TestSelfCheck_APublicOnlySecretStillCountsAsRead pins the gate's reading of a
// Secret every one of whose keys is a published identifier.
//
// The gate asks "did anybody READ this Secret", because an empty LiveSecrets and
// a Secret nobody looked at are otherwise the same value. A public entry answers
// that question — the read happened, and what it found is not secret — so it
// must not raise secret-check-skipped. Refusing here would put the false refusal
// back one layer down, which is exactly what the EmptyRead entry beside it
// exists to avoid.
func TestSelfCheck_APublicOnlySecretStillCountsAsRead(t *testing.T) {
	in := cleanCapture(t)
	in.ShadowedSecrets = append(in.ShadowedSecrets, "demo-gh-creds")
	in.LiveSecrets = append(in.LiveSecrets, steelthread.LiveSecret{
		Name: "demo-gh-creds/app-id", Value: publicIDValue, Public: true,
	})

	for _, f := range steelthread.SelfCheck(in) {
		require.NotEqual(t, steelthread.CodeSecretCheckSkipped, f.Code,
			"a Secret that was read and held only published identifiers was still READ: %s", f.Message)
	}
}
