package skillcmd

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/aptest"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// fakeCloneToken is credential-shaped but not a credential: `oap skill source`
// prints spec.repoURL, and the tests below need a value whose survival into the
// output is unambiguous.
const fakeCloneToken = "ghp_notarealtokenatall0000"

func TestRedactRepoURL(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "no userinfo: unchanged",
			raw:  "https://git.invalid/org/repo",
			want: "https://git.invalid/org/repo",
		},
		{
			name: "token as the user half: replaced, host and path kept",
			raw:  "https://" + fakeCloneToken + "@git.invalid/org/repo",
			want: "https://****@git.invalid/org/repo",
		},
		{
			name: "user:token: the whole userinfo goes, not just the password",
			raw:  "https://alice:" + fakeCloneToken + "@git.invalid/org/repo.git",
			want: "https://****@git.invalid/org/repo.git",
		},
		{
			name: "userinfo net/url refuses to parse: still redacted",
			raw:  "https://alice:tok en@git.invalid/org/repo",
			want: "https://****@git.invalid/org/repo",
		},
		{
			name: "scp-style git@host: no authority, so nothing to redact",
			raw:  "git@git.invalid:org/repo.git",
			want: "git@git.invalid:org/repo.git",
		},
		{
			name: "ssh://git@host: redacted too, which costs only a constant",
			raw:  "ssh://git@git.invalid/org/repo.git",
			want: "ssh://****@git.invalid/org/repo.git",
		},
		{
			name: "@ in the path, not the authority: kept",
			raw:  "https://git.invalid/org/repo@v2",
			want: "https://git.invalid/org/repo@v2",
		},
		{
			name: "userinfo with a query and no path: authority still bounded correctly",
			raw:  "https://alice:" + fakeCloneToken + "@git.invalid?ref=main",
			want: "https://****@git.invalid?ref=main",
		},
		{
			name: "empty: unchanged",
			raw:  "",
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := redactRepoURL(tc.raw)
			assert.Equal(t, tc.want, got)
			assert.NotContains(t, got, fakeCloneToken, "no credential may survive redaction")
		})
	}
}

// TestSkillSourceListDoesNotPrintACredentialEmbeddedInTheRepoURL is the
// end-to-end half: redactRepoURL being correct is worth nothing if the REPO
// column is built from the raw spec field. It runs the real command over a
// SkillSource whose repoURL carries a token and requires the token to be absent
// from everything the command wrote.
func TestSkillSourceListDoesNotPrintACredentialEmbeddedInTheRepoURL(t *testing.T) {
	objs := []client.Object{&spiceboxv1alpha1.SkillSource{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-source", Namespace: "default"},
		Spec: spiceboxv1alpha1.SkillSourceSpec{
			RepoURL: "https://alice:" + fakeCloneToken + "@git.invalid/org/repo",
			Ref:     "main",
		},
	}}

	for _, args := range [][]string{
		{"source", "list"},
		{"source", "show", "demo-source"},
	} {
		t.Run("oap skill "+strings.Join(args, " ")+": token absent, repo still identifiable", func(t *testing.T) {
			g := aptest.GlobalsFor(aptest.NewBundle(t, objs...))
			out := aptest.Run(t, NewCmd(g), args...)
			assert.NotContains(t, out, fakeCloneToken, "a credential in spec.repoURL must not reach the terminal:\n%s", out)
			assert.NotContains(t, out, "alice", "the whole userinfo goes, not just the secret half")
			assert.Contains(t, out, "git.invalid/org/repo", "the repo must stay identifiable")
		})
	}
}
