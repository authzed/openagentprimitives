package oap

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBundle_SkillClones(t *testing.T) {
	manifests := `apiVersion: agentprimitives.authzed.com/v1alpha1
kind: AgentClass
metadata:
  name: a
---
apiVersion: agentprimitives.authzed.com/v1alpha1
kind: SkillSource
metadata:
  name: alpha
spec:
  repoURL: https://github.com/fakeorg/skills
  ref: main
  subpath: .claude/skills
---
apiVersion: agentprimitives.authzed.com/v1alpha1
kind: SkillSource
metadata:
  name: beta
spec:
  repoURL: https://github.com/fakeorg/playbooks
`
	b := &Bundle{
		Manifest:  &Manifest{OapFormatVersion: "1", Agent: Agent{Name: "a", Version: "1"}},
		Manifests: []byte(manifests),
	}

	clones, err := b.SkillClones()
	require.NoError(t, err)
	require.Len(t, clones, 2)
	// Sorted by SkillSource name: alpha, then beta.
	assert.Equal(t, SkillClone{SkillSource: "alpha", RepoURL: "https://github.com/fakeorg/skills", Ref: "main", Subpath: ".claude/skills"}, clones[0])
	assert.Equal(t, SkillClone{SkillSource: "beta", RepoURL: "https://github.com/fakeorg/playbooks"}, clones[1])
}

func TestBundle_SkillClones_NoneWhenNoSkillSource(t *testing.T) {
	b := &Bundle{
		Manifest:  &Manifest{OapFormatVersion: "1", Agent: Agent{Name: "a", Version: "1"}},
		Manifests: []byte("apiVersion: agentprimitives.authzed.com/v1alpha1\nkind: AgentClass\nmetadata:\n  name: a\n"),
	}
	clones, err := b.SkillClones()
	require.NoError(t, err)
	assert.Empty(t, clones)
}
