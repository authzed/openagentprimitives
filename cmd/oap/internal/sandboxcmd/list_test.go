package sandboxcmd

import (
	"testing"
	"unicode/utf8"

	"github.com/charmbracelet/lipgloss"
	"github.com/stretchr/testify/assert"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

func TestSandboxCell(t *testing.T) {
	cases := []struct {
		name string
		s    spiceboxv1alpha1.SpiceboxSession
		want string
	}{
		{
			name: "status.sandbox set, cold -> kind:ref, no warm suffix",
			s: spiceboxv1alpha1.SpiceboxSession{Status: spiceboxv1alpha1.SpiceboxSessionStatus{
				Sandbox: &spiceboxv1alpha1.SandboxHandle{Kind: "pod", Ref: "default/sess-1"},
			}},
			want: "pod:default/sess-1",
		},
		{
			name: "status.sandbox set, prewarmed=true -> (warm) suffix",
			s: spiceboxv1alpha1.SpiceboxSession{Status: spiceboxv1alpha1.SpiceboxSessionStatus{
				Sandbox: &spiceboxv1alpha1.SandboxHandle{Kind: "agent-sandbox", Ref: "default/claim-1", Prewarmed: true},
			}},
			want: "agent-sandbox:default/claim-1 (warm)",
		},
		{
			name: "status.sandbox set, long ref -> truncated with ellipsis",
			s: spiceboxv1alpha1.SpiceboxSession{Status: spiceboxv1alpha1.SpiceboxSessionStatus{
				Sandbox: &spiceboxv1alpha1.SandboxHandle{Kind: "agent-sandbox", Ref: "default/this-is-a-very-long-sandbox-session-name-indeed"},
			}},
			want: "agent-sandbox:default/this-is-a-very-long…",
		},
		{
			name: "status.sandbox unset, PodName set -> PodName fallback (pre-seam session)",
			s: spiceboxv1alpha1.SpiceboxSession{Status: spiceboxv1alpha1.SpiceboxSessionStatus{
				PodName: "legacy-pod-1",
			}},
			want: "legacy-pod-1",
		},
		{
			name: "neither set -> placeholder",
			s:    spiceboxv1alpha1.SpiceboxSession{},
			want: "-",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, sandboxCell(tc.s))
		})
	}
}

func TestTruncateSandboxRef(t *testing.T) {
	cases := []struct {
		name string
		in   string
		max  int
		want string
	}{
		{"under max -> unchanged", "short", 10, "short"},
		{"exactly max -> unchanged", "1234567890", 10, "1234567890"},
		{"over max -> truncated to max-1 chars plus ellipsis", "12345678901", 10, "123456789…"},
		// A ref is backend-supplied, not a Kubernetes name, so it need not be
		// ASCII. Each of these is 2 display columns and 3 bytes wide, so a
		// byte-measured cut lands mid-rune and a byte-measured budget lets the
		// cell overflow its column — both caught by the invariants below.
		{"wide runes under max -> unchanged", "一二三四五", 10, "一二三四五"},
		{"wide runes over max -> cut on a rune boundary within the column budget", "一二三四五六", 10, "一二三四…"},
		{"mixed widths over max -> cut where the columns run out, not the bytes", "默认/一二三四五六七八九十一二三四五", 28, "默认/一二三四五六七八九十一…"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := truncateSandboxRef(tc.in, tc.max)
			assert.Equal(t, tc.want, got)
			// The two facts the SANDBOX column depends on, asserted for every
			// row rather than only the ones written with them in mind: a cell
			// wider than its budget pushes every column to its right, and an
			// invalid cut prints replacement characters at the user.
			assert.LessOrEqual(t, lipgloss.Width(got), tc.max, "cell must fit its display-column budget")
			assert.True(t, utf8.ValidString(got), "cut must land on a rune boundary")
		})
	}
}
