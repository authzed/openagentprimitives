package installcmd

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/authzed/openagentprimitives/pkg/platform/cloud"
)

// zap2-shaped class set: the bundled node-pinned default plus the Filestore
// classes an operator added later.
func pickerClasses() []cloud.RWXClassInfo {
	return []cloud.RWXClassInfo{
		{Name: "ap-workspace-rwx", Provisioner: "cluster.local/ap-workspace-provisioner", Bundled: true},
		{Name: "enterprise-multishare-rwx", Provisioner: "filestore.csi.storage.gke.io", Filestore: true, Multishare: true},
		{Name: "enterprise-rwx", Provisioner: "filestore.csi.storage.gke.io", Filestore: true},
	}
}

const (
	pickerCurrent     = "ap-workspace-rwx"
	pickerRecommended = "enterprise-multishare-rwx"
)

// selectWorkspaceClass drives the interactive picker: it renders the menu with
// the current class pre-selected and the recommendation annotated, warns that
// switching rolls the operator, reads one line, and maps the choice to a
// workspaceChoice. These cases pin every branch of that mapping plus the
// mandated roll-operator warning.
func TestSelectWorkspaceClass(t *testing.T) {
	t.Run("empty input keeps the current class (no repoint)", func(t *testing.T) {
		var out bytes.Buffer
		got := selectWorkspaceClass(strings.NewReader("\n"), &out, pickerClasses(), pickerCurrent, pickerRecommended)
		assert.Equal(t, pickerCurrent, got.ClassName)
		assert.Empty(t, got.Message)
	})

	t.Run("menu warns that switching rolls the operator", func(t *testing.T) {
		var out bytes.Buffer
		_ = selectWorkspaceClass(strings.NewReader("\n"), &out, pickerClasses(), pickerCurrent, pickerRecommended)
		s := strings.ToLower(out.String())
		assert.Contains(t, s, "operator", "warning must name the operator")
		assert.True(t, strings.Contains(s, "roll") || strings.Contains(s, "restart"),
			"warning must say the operator is rolled/restarted; got:\n%s", out.String())
	})

	t.Run("menu marks current and recommended", func(t *testing.T) {
		var out bytes.Buffer
		_ = selectWorkspaceClass(strings.NewReader("\n"), &out, pickerClasses(), pickerCurrent, pickerRecommended)
		s := out.String()
		assert.Contains(t, s, "current")
		assert.Contains(t, s, "recommended")
	})

	t.Run("selecting the recommended Filestore class returns it", func(t *testing.T) {
		var out bytes.Buffer
		// Menu order: 1) ap-workspace-rwx  2) enterprise-multishare-rwx  ...
		got := selectWorkspaceClass(strings.NewReader("2\n"), &out, pickerClasses(), pickerCurrent, pickerRecommended)
		assert.Equal(t, pickerRecommended, got.ClassName)
		assert.Empty(t, got.Message)
	})

	t.Run("keeping the current class needs no probe (Probe=false, trusted)", func(t *testing.T) {
		var out bytes.Buffer
		got := selectWorkspaceClass(strings.NewReader("\n"), &out, pickerClasses(), pickerCurrent, pickerRecommended)
		assert.Equal(t, pickerCurrent, got.ClassName)
		assert.False(t, got.Probe, "the already-installed current class is trusted; no re-probe, no unverified up-front stamp")
	})

	t.Run("switching to a different class must be probed before it is trusted (Probe=true)", func(t *testing.T) {
		var out bytes.Buffer
		got := selectWorkspaceClass(strings.NewReader("2\n"), &out, pickerClasses(), pickerCurrent, pickerRecommended)
		assert.Equal(t, pickerRecommended, got.ClassName)
		assert.True(t, got.Probe,
			"a newly-picked class is unverified; the WORK block must probe it before writing the marker or stamping the operator")
	})

	t.Run("selecting isolated returns an empty class with guidance", func(t *testing.T) {
		var out bytes.Buffer
		// Isolated is always the last option: 3 classes + isolated = index 4.
		got := selectWorkspaceClass(strings.NewReader("4\n"), &out, pickerClasses(), pickerCurrent, pickerRecommended)
		assert.Empty(t, got.ClassName)
		assert.NotEmpty(t, got.Message, "isolated choice must carry guidance for the WORK block")
	})

	t.Run("out-of-range input keeps the current class", func(t *testing.T) {
		var out bytes.Buffer
		got := selectWorkspaceClass(strings.NewReader("99\n"), &out, pickerClasses(), pickerCurrent, pickerRecommended)
		assert.Equal(t, pickerCurrent, got.ClassName)
	})

	t.Run("current class missing from the cluster is still selectable as default", func(t *testing.T) {
		var out bytes.Buffer
		got := selectWorkspaceClass(strings.NewReader("\n"), &out, pickerClasses(), "deleted-class", pickerRecommended)
		assert.Equal(t, "deleted-class", got.ClassName)
		assert.Contains(t, strings.ToLower(out.String()), "not found")
	})
}
