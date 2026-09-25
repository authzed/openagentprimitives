package permsurface_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/permsurface"
)

func permDescriptor(t *testing.T, perm, resource string, tools ...string) permsurface.Descriptor {
	t.Helper()
	h, err := permsurface.NewPermHandle(perm, resource)
	require.NoError(t, err)
	d := permsurface.Descriptor{
		Handle: h, Permission: perm, ResourceType: resource,
		StateImpact: authz.Readonly,
	}
	for _, tl := range tools {
		d.Via = append(d.Via, permsurface.Provenance{Tool: tl})
	}
	return d
}

// An approver deciding a ceiling reads handles, and `perm:list:crm_company` is
// a wire format, not a sentence. The description is what makes a card legible
// to somebody who does not already know the handle grammar.
func TestDescribe_readsAsASentenceNotAWireFormat(t *testing.T) {
	cases := []struct {
		name string
		desc permsurface.Descriptor
		want string
	}{
		{
			name: "a permission handle names the action and the resource",
			desc: permDescriptor(t, "list", "crm_company"),
			want: "List crm_company",
		},
		{
			name: "the tools that exercise it are named",
			desc: permDescriptor(t, "read", "crm_contact", "get_contact", "list_contacts"),
			want: "Read crm_contact (get_contact, list_contacts)",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, tc.desc.Describe())
		})
	}
}

// A tool handle has no permission or resource to name, so it describes as the
// tool itself rather than as an empty action.
func TestDescribe_aToolHandleDescribesAsItsTool(t *testing.T) {
	h, err := permsurface.NewToolHandle("send_email")
	require.NoError(t, err)
	d := permsurface.Descriptor{Handle: h, ToolName: "send_email", StateImpact: authz.External}

	assert.Equal(t, "send_email", d.Describe())
}

// A descriptor the surface never resolved carries no parts to describe. It
// falls back to the handle rather than rendering an empty line — a blank entry
// on an approval card is worse than a raw one.
func TestDescribe_anUnresolvedDescriptorFallsBackToTheHandle(t *testing.T) {
	h, err := permsurface.NewPermHandle("read", "doc")
	require.NoError(t, err)

	assert.Equal(t, "perm:read:doc", permsurface.Descriptor{Handle: h}.Describe())
}

// A declared title wins outright; with none, the fallback DETOKENIZES the
// handle rather than inventing English the way Describe() explicitly refuses
// to.
func TestDescriptor_DescribeWithTitle(t *testing.T) {
	d := permsurface.Descriptor{Permission: "push", ResourceType: "git_repo"}

	assert.Equal(t, "Push commits to the repository",
		d.DescribeWithTitle("Push commits to the repository"),
		"a declared title wins outright")

	assert.Equal(t, "Push git repo", d.DescribeWithTitle(""),
		"no title: detokenize with no article — a/an guesses wrong on vowel-initial "+
			"resource types (\"a email\"), and dropping the article removes the whole "+
			"class rather than trading one bug for a smaller one")
}

// A tool handle has no resource type to detokenize, so its honest fallback is
// the tool name itself — same as Describe().
func TestDescriptor_DescribeWithTitle_ToolHandle(t *testing.T) {
	d := permsurface.Descriptor{ToolName: "gitlike_git"}
	assert.Equal(t, "gitlike_git", d.DescribeWithTitle(""),
		"a tool handle has no resource type to detokenize; its name is the honest answer")
}
