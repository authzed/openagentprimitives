package install

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/platform/oap"
)

// validBundle returns a minimal-but-Validate()-clean bundle: format version 1,
// a compat floor, and one required secret satisfied by a matching secret
// question. It carries exactly one AgentClass CR because Bundle.Validate
// enforces the one-class rule; the sole question is type=secret, so no binding
// target needs to resolve against that CR.
func validBundle() *oap.Bundle {
	return &oap.Bundle{
		Manifest: &oap.Manifest{
			OapFormatVersion: "1",
			Agent:            oap.Agent{Name: "fixture-agent", Version: "1.0.0"},
			Compat:           oap.Compat{MinApVersion: "1.2.0"},
			Requires: oap.Requires{
				Secrets: []oap.RequiredSecret{
					{Name: "widget-token", Keys: []string{"token"}, Question: "widgetToken"},
				},
			},
			Questions: []oap.Question{
				{
					Name:   "widgetToken",
					Type:   oap.QSecret,
					Prompt: "Widget API token",
					Secret: &oap.SecretQuestion{
						CreateSecret: &oap.SecretTarget{Name: "widget-token", Key: "token"},
					},
				},
			},
		},
		Manifests: []byte("apiVersion: agentprimitives.authzed.com/v1alpha1\nkind: AgentClass\nmetadata:\n  name: fixture-agent\nspec:\n  systemPrompt:\n    inline: hi\n"),
	}
}

func TestPreflight(t *testing.T) {
	cases := []struct {
		name             string
		mutate           func(b *oap.Bundle)
		clusterApVersion string
		wantErr          string // substring expected in the error; "" means Preflight must return nil
	}{
		{
			name:             "valid bundle, matching versions, secret has question: nil",
			clusterApVersion: "1.3.0",
		},
		{
			name:             "minApVersion above cluster version: error naming both versions",
			clusterApVersion: "1.1.0",
			wantErr:          "agent requires oap >= 1.2.0 but cluster is 1.1.0",
		},
		{
			name: "unknown oapFormatVersion major: error via Validate",
			mutate: func(b *oap.Bundle) {
				b.Manifest.OapFormatVersion = "2"
			},
			clusterApVersion: "1.3.0",
			wantErr:          "newer than this oap supports",
		},
		{
			name: "keyed required secret whose question cannot materialize it: error naming the question",
			mutate: func(b *oap.Bundle) {
				// orExisting alone: the question collects a value install has
				// nowhere to put, which Resolve refuses rather than drops.
				b.Manifest.Questions[0].Secret = &oap.SecretQuestion{OrExisting: true}
			},
			clusterApVersion: "1.3.0",
			wantErr:          "createSecret",
		},
		{
			name: "keyless required secret with no question: nil, its setup flow mints it",
			mutate: func(b *oap.Bundle) {
				// The whole-Secret shape an OAuth credential exports as: no key
				// to name, so no typed answer could reconstruct it.
				b.Manifest.Requires.Secrets = append(b.Manifest.Requires.Secrets, oap.RequiredSecret{
					Name: "oauth-creds",
				})
			},
			clusterApVersion: "1.3.0",
		},
		{
			name: "keyed required secret with no question: nil, install synthesizes one per key",
			mutate: func(b *oap.Bundle) {
				// The shape a bundle author writes when they just want the
				// credential asked for: name the Secret and its keys, wire
				// nothing. RequiredSecretQuestions turns each key into the
				// question this used to demand the author write by hand.
				b.Manifest.Requires.Secrets = append(b.Manifest.Requires.Secrets, oap.RequiredSecret{
					Name: "orphan-secret",
					Keys: []string{"token"},
				})
			},
			clusterApVersion: "1.3.0",
		},
		{
			name: "required secret with no matching question: error",
			mutate: func(b *oap.Bundle) {
				b.Manifest.Requires.Secrets = append(b.Manifest.Requires.Secrets, oap.RequiredSecret{
					Name:     "orphan-secret",
					Question: "missingQuestion",
				})
			},
			clusterApVersion: "1.3.0",
			wantErr:          "missingQuestion",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := validBundle()
			if tc.mutate != nil {
				tc.mutate(b)
			}

			err := Preflight(context.Background(), b, tc.clusterApVersion)

			if tc.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

func TestPreflight_CompatSkippedWhenEitherSideUnset(t *testing.T) {
	b := validBundle()
	b.Manifest.Compat.MinApVersion = ""
	require.NoError(t, Preflight(context.Background(), b, ""))

	b2 := validBundle()
	require.NoError(t, Preflight(context.Background(), b2, ""))
}

func TestPreflight_CompatInvalidSemverIsHardError(t *testing.T) {
	b := validBundle()
	b.Manifest.Compat.MinApVersion = "not-a-version"

	err := Preflight(context.Background(), b, "1.0.0")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not-a-version")
}
