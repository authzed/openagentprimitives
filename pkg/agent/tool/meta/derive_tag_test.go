package meta

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/authz"
)

func TestDeriveTag_mintsDerivedAndReturnsRegion(t *testing.T) {
	var gotInputs []string
	var gotContent string
	cfg := DeriveTagConfig{Mint: func(_ context.Context, in []string, content string) (string, error) {
		gotInputs = in
		gotContent = content
		return "pt_new", nil
	}}
	tl := NewDeriveTag(cfg)

	assert.Equal(t, "derive_tag", tl.Name())
	assert.Equal(t, tool.KindMeta, tl.Kind())
	assert.Equal(t, authz.Stateless, tl.Permission().StateImpact)

	res, err := tl.Execute(context.Background(),
		json.RawMessage(`{"derived_from":["pt_1","pt_2"],"content":"the fused conclusion"}`), &tool.SessionContext{})
	require.NoError(t, err)
	assert.False(t, res.IsError)
	assert.Equal(t, []string{"pt_1", "pt_2"}, gotInputs)
	assert.Equal(t, "the fused conclusion", gotContent, "the content is handed to the validator+mint")
	assert.Contains(t, res.Content, `<pt-untrusted nonce=`, "returns a ready-to-paste region")
	assert.Contains(t, res.Content, `id="pt_new"`, "the region carries the new id")
	assert.Contains(t, res.Content, "the fused conclusion", "the region wraps the exact content")
}

func TestDeriveTag_refusals(t *testing.T) {
	okMint := func(context.Context, []string, string) (string, error) { return "pt_x", nil }

	// nil Mint → refuse.
	refuse := NewDeriveTag(DeriveTagConfig{})
	r, err := refuse.Execute(context.Background(), json.RawMessage(`{"derived_from":["pt_1"],"content":"x"}`), &tool.SessionContext{})
	require.NoError(t, err)
	assert.True(t, r.IsError)

	// empty derived_from → error, not a leaf.
	empty, err := NewDeriveTag(DeriveTagConfig{Mint: okMint}).Execute(
		context.Background(), json.RawMessage(`{"derived_from":[],"content":"x"}`), &tool.SessionContext{})
	require.NoError(t, err)
	assert.True(t, empty.IsError, "no inputs is an error, not a leaf")

	// missing content → error (nothing to validate or paste).
	noContent, err := NewDeriveTag(DeriveTagConfig{Mint: okMint}).Execute(
		context.Background(), json.RawMessage(`{"derived_from":["pt_1"]}`), &tool.SessionContext{})
	require.NoError(t, err)
	assert.True(t, noContent.IsError, "content is required")

	// a Mint error (validation rejected / access denied) → surfaced IsError.
	denyTL := NewDeriveTag(DeriveTagConfig{Mint: func(context.Context, []string, string) (string, error) {
		return "", fmt.Errorf("the derivation was rejected — it introduces information not in pt_9")
	}})
	denied, err := denyTL.Execute(context.Background(), json.RawMessage(`{"derived_from":["pt_9"],"content":"smuggled secret"}`), &tool.SessionContext{})
	require.NoError(t, err)
	assert.True(t, denied.IsError)
	assert.Contains(t, denied.Content, "rejected", "the refusal surfaces the validator's reason")
}
