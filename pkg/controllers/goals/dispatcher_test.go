package goals

import (
	"strings"
	"testing"

	domain "github.com/authzed/openagentprimitives/pkg/agent/goals"
	"github.com/stretchr/testify/require"
)

func TestOpeningSummary(t *testing.T) {
	require.Equal(t, "Session created to meet goal Stretch: Stand up and stretch.", openingSummary(domain.Goal{Title: "Stretch", Outcome: "Stand up\n and stretch."}))
	long := openingSummary(domain.Goal{Title: "Stretch", Outcome: strings.Repeat("🦊", 3000)})
	require.Len(t, []rune(long), 2000)
	require.True(t, strings.HasSuffix(long, "…"))
}
