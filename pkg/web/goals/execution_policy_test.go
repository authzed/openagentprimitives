package goals

import (
	"testing"
	"time"

	domain "github.com/authzed/openagentprimitives/pkg/agent/goals"
	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestExecutionPolicyCapsDefaults(t *testing.T) {
	for _, tc := range []struct {
		name   string
		budget *v1.BudgetConfig
		want   domain.ExecutionBounds
	}{
		{"unconfigured", nil, domain.ExecutionBounds{180, 10, 10000, 90}},
		{"tight class", &v1.BudgetConfig{MaxTurns: 3, MaxTokens: 2000, MaxDuration: metav1.Duration{Duration: 60 * time.Second}}, domain.ExecutionBounds{60, 3, 2000, 60}},
		{"wall lifetime", &v1.BudgetConfig{MaxTurns: 50, MaxTokens: 40000, MaxDuration: metav1.Duration{Duration: 10 * time.Minute}, SessionExpiration: metav1.Duration{Duration: 30 * time.Second}}, domain.ExecutionBounds{30, 10, 10000, 30}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := executionPolicy(&v1.AgentClass{Spec: v1.AgentClassSpec{Budget: tc.budget}})
			assert.Equal(t, tc.want, p.DefaultBounds)
			assert.LessOrEqual(t, p.DefaultBounds.DurationSeconds, p.MaxBounds.DurationSeconds)
			assert.LessOrEqual(t, p.DefaultBounds.ApprovalSeconds, p.DefaultBounds.DurationSeconds)
		})
	}
}
