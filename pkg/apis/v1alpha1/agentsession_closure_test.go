package v1alpha1_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// labelled stamps s with LabelDelegationRoot=root, mirroring what buildChild
// does at creation time, and returns s for chaining into a table of fixtures.
func labelled(s *v1.AgentSession, root string) *v1.AgentSession {
	s.Labels = map[string]string{v1.LabelDelegationRoot: root}
	return s
}

func namesOf(sessions []v1.AgentSession) []string {
	names := make([]string, len(sessions))
	for i, s := range sessions {
		names[i] = s.Name
	}
	return names
}

func TestRootNameFor_ParentWithNoLabel_ReturnsParentsOwnName(t *testing.T) {
	parent := sess(t, "root-a", "", "")
	assert.Equal(t, "root-a", v1.RootNameFor(parent), "a parent without the label IS a root")
}

func TestRootNameFor_ParentWithLabel_ReturnsTheLabelValueNotTheParentsName(t *testing.T) {
	// mid-b is itself a delegated child of root-a; its label carries the
	// tree's root, which is NOT its own name.
	parent := labelled(sess(t, "mid-b", "root-a", ""), "root-a")

	got := v1.RootNameFor(parent)

	assert.Equal(t, "root-a", got, "a labelled parent's tree root is the label value")
	assert.NotEqual(t, parent.Name, got,
		"this is the induction step: 'simplifying' RootNameFor to always return "+
			"the parent's name would make every child its own root")
}

func TestListClosure_ExcludesOtherTreesAndOtherNamespaces(t *testing.T) {
	root := sess(t, "root-a", "", "")
	child1 := labelled(sess(t, "child-a1", "root-a", ""), "root-a")
	child2 := labelled(sess(t, "child-a2", "root-a", ""), "root-a")

	// A wholly separate tree in the same namespace.
	otherRoot := sess(t, "root-x", "", "")
	otherChild := labelled(sess(t, "child-x1", "root-x", ""), "root-x")

	// Same root name, same child name, but a different namespace.
	otherNSChild := labelled(sess(t, "child-a1", "root-a", ""), "root-a")
	otherNSChild.Namespace = "other-ns"

	c := newReader(t, root, child1, child2, otherRoot, otherChild, otherNSChild).Build()

	got, err := v1.ListClosure(context.Background(), c, "default", "root-a")
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"child-a1", "child-a2"}, namesOf(got),
		"only default/root-a's own labelled descendants, never another tree or another namespace")
}

func TestDescendantsOf_MidTreeNode_ReturnsItsSubtreeOnly(t *testing.T) {
	root := sess(t, "root-a", "", "")
	mid := labelled(sess(t, "mid-a", "root-a", ""), "root-a")
	leaf := labelled(sess(t, "leaf-a", "mid-a", ""), "root-a")

	// A sibling branch under the same root: must not appear in mid's subtree.
	sibling := labelled(sess(t, "mid-b", "root-a", ""), "root-a")
	siblingLeaf := labelled(sess(t, "leaf-b", "mid-b", ""), "root-a")

	c := newReader(t, root, mid, leaf, sibling, siblingLeaf).Build()

	got, err := v1.DescendantsOf(context.Background(), c, mid)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"leaf-a"}, namesOf(got),
		"mid's own subtree only -- not its ancestor (root), not a sibling branch, not itself")
}

func TestDescendantsOf_Root_ReturnsWholeTreeMinusRoot(t *testing.T) {
	root := sess(t, "root-a", "", "")
	mid := labelled(sess(t, "mid-a", "root-a", ""), "root-a")
	leaf := labelled(sess(t, "leaf-a", "mid-a", ""), "root-a")

	c := newReader(t, root, mid, leaf).Build()

	got, err := v1.DescendantsOf(context.Background(), c, root)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"mid-a", "leaf-a"}, namesOf(got),
		"the whole tree minus the root itself -- ListClosure never labels the root with its own name")
}

func TestDescendantsOf_MemberWithBrokenAncestorLink_ErrorsRatherThanBeingDropped(t *testing.T) {
	root := sess(t, "root-a", "", "")
	mid := labelled(sess(t, "mid-a", "root-a", ""), "root-a")
	// ghost-a carries root-a's label (so ListClosure returns it) but its
	// parent reference is broken. Per no-silent-errors, DescendantsOf must
	// surface this rather than silently excluding ghost-a from the result --
	// a hold cascade that drops a member because its walk happened to fail
	// would freeze everything except the one session it most needed to catch.
	ghost := labelled(sess(t, "ghost-a", "missing-parent", ""), "root-a")

	c := newReader(t, root, mid, ghost).Build()

	_, err := v1.DescendantsOf(context.Background(), c, root)
	require.Error(t, err, "a broken ancestor link on a closure member must propagate, not be dropped")
	assert.Contains(t, err.Error(), "ghost-a")
}
