//go:build integration

package extract_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	llmanthropic "github.com/authzed/openagentprimitives/pkg/agent/llm/anthropic"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/extract"
	"github.com/authzed/openagentprimitives/pkg/authz/extract/anthropic"
)

func TestIntegration_Extract_RealLLM(t *testing.T) {
	key := os.Getenv("ANTHROPIC_API_KEY")
	if key == "" {
		t.Skip("ANTHROPIC_API_KEY not set; skipping live LLM test")
	}
	llmProv := llmanthropic.New(key)
	prov := anthropic.New(llmProv, "")

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	types := []spiceboxv1alpha1.BoundEntityType{
		{ResourceType: "github_repo", Description: "GitHub repository in owner/name form.", Permission: "read"},
	}
	res, err := prov.Extract(ctx, extract.ExtractInput{
		UserMessage: "Summarize last week's work on authzed/spicedb",
		EntityTypes: types,
	})
	require.NoError(t, err)
	t.Logf("extracted: %+v", res)
}
