package skill

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/tools/skills/validate"
)

// spec.repoInstructions.Content lands VERBATIM in the system prompt of every
// AgentClass linking this skill, on every turn, under "Treat them as
// authoritative for work that touches those repos". It is the highest-trust text
// surface the product has.
//
// The SkillSource controller caps it at 64 KiB at materialization — its own
// comment gives the reason, "a hostile or oversized AGENTS.md" — and the webhook
// never looked at the field at all: validate.Skill takes no repoInstructions
// parameter, and the CRD declares content as a plain string with no maxLength.
// So a hand-authored Skill set it directly and it reached the prompt uncapped.
//
// The guard-on-one-of-two-paths shape is sharp here: the DESCRIPTION beside it
// in the same prompt IS validated — length, no XML/HTML tags, no reserved words
// — precisely because it reaches the prompt.
func TestSkillWebhook_RepoInstructionsAreCappedAtAdmission(t *testing.T) {
	w := newWebhook(t)
	w.OperatorUsername = "system:serviceaccount:agentprimitives-system:spicebox-operator"

	skill := &v1.Skill{
		ObjectMeta: metav1.ObjectMeta{Name: "mine", Namespace: "tenant-ns"},
		Spec: v1.SkillSpec{
			CanonicalName: "local//mine",
			Frontmatter:   v1.SkillFrontmatter{Name: "mine"},
			Description:   "something I wrote",
			Body:          "my own instructions",
			RepoInstructions: &v1.SkillRepoInstructions{
				SourceFile: "AGENTS.md",
				Content:    strings.Repeat("x", validate.MaxRepoInstructionsBytes+1),
			},
		},
	}

	over := w.Handle(context.Background(),
		writerReq(t, skill, "system:serviceaccount:tenant-ns:some-tenant"))
	assert.False(t, over.Allowed,
		"unbounded always-on prompt text must be refused at the door, not only at materialization")

	// Exactly at the cap must pass: the controller stores 64 KiB itself, so a
	// materialized skill must not be rejected by its own platform.
	skill.Spec.RepoInstructions.Content = strings.Repeat("x", validate.MaxRepoInstructionsBytes)
	at := w.Handle(context.Background(),
		writerReq(t, skill, "system:serviceaccount:tenant-ns:some-tenant"))
	assert.True(t, at.Allowed, "%s", at.Result)
}
