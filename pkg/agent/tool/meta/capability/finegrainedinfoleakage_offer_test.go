package capability

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type stubDeriveValidator struct{ valid bool }

func (s stubDeriveValidator) ValidateDerivation(context.Context, []DeriveSource, string) (bool, string, error) {
	return s.valid, "", nil
}

func TestFineGrainedOffer_returnsDeriveTagWhenWired(t *testing.T) {
	c := &fineGrainedInfoLeakageCapability{}

	none, skip := c.Offer(OfferContext{}) // no Env backing
	assert.Nil(t, skip)
	assert.Empty(t, none, "no MintDerivedTag → no tool offered")

	// Mint but NO validator → still no tool (fail-closed: no unvalidated derivation).
	mintOnly := RunnerEnv{MintDerivedTag: func(context.Context, []string, string) (string, error) { return "pt_x", nil }}
	none, _ = c.Offer(OfferContext{Env: mintOnly})
	assert.Empty(t, none, "no DeriveValidator → derive_tag is not offered")

	env := RunnerEnv{
		MintDerivedTag:     func(context.Context, []string, string) (string, error) { return "pt_x", nil },
		ResolveTagContents: func(context.Context, []string) ([]DeriveSource, error) { return nil, nil },
		DeriveValidator:    stubDeriveValidator{valid: true},
	}
	tools, skip := c.Offer(OfferContext{Env: env})
	assert.Nil(t, skip)
	require.Len(t, tools, 1)
	assert.Equal(t, "derive_tag", tools[0].Name())
}

func TestFineGrainedOffer_accessCheckGatesInputs(t *testing.T) {
	var mintedInputs []string
	env := RunnerEnv{
		MintDerivedTag: func(_ context.Context, in []string, _ string) (string, error) {
			mintedInputs = in
			return "pt_new", nil
		},
		TagAccessCheck:     func(_ context.Context, id string) (bool, error) { return id != "pt_forbidden", nil },
		ResolveTagContents: func(context.Context, []string) ([]DeriveSource, error) { return nil, nil },
		DeriveValidator:    stubDeriveValidator{valid: true},
	}
	tools, _ := (&fineGrainedInfoLeakageCapability{}).Offer(OfferContext{Env: env})
	require.Len(t, tools, 1)

	// A permitted input mints.
	ok, err := tools[0].Execute(context.Background(), []byte(`{"derived_from":["pt_ok"],"content":"synthesis"}`), nil)
	require.NoError(t, err)
	assert.False(t, ok.IsError)
	assert.Equal(t, []string{"pt_ok"}, mintedInputs)

	// A forbidden input is refused before the mint, naming the tag.
	mintedInputs = nil
	denied, err := tools[0].Execute(context.Background(), []byte(`{"derived_from":["pt_forbidden"],"content":"synthesis"}`), nil)
	require.NoError(t, err)
	assert.True(t, denied.IsError)
	assert.Contains(t, denied.Content, "pt_forbidden")
	assert.Nil(t, mintedInputs, "the mint is never reached for an input the session cannot access")
}

func TestFineGrainedOffer_validatorRejectionBlocksMint(t *testing.T) {
	var minted bool
	env := RunnerEnv{
		MintDerivedTag: func(context.Context, []string, string) (string, error) { minted = true; return "pt_new", nil },
		ResolveTagContents: func(context.Context, []string) ([]DeriveSource, error) {
			return []DeriveSource{{TagID: "pt_pub", Content: "public"}}, nil
		},
		DeriveValidator: stubDeriveValidator{valid: false}, // judge rejects
	}
	tools, _ := (&fineGrainedInfoLeakageCapability{}).Offer(OfferContext{Env: env})
	require.Len(t, tools, 1)

	res, err := tools[0].Execute(context.Background(), []byte(`{"derived_from":["pt_pub"],"content":"smuggled secret"}`), nil)
	require.NoError(t, err)
	assert.True(t, res.IsError, "a rejected derivation must not mint")
	assert.False(t, minted, "the mint is never reached when validation fails")
}
