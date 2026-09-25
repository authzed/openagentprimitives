package desktop

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGuestIPFromKubeconfig(t *testing.T) {
	kc := []byte(`
apiVersion: v1
clusters:
- name: oap-desktop
  cluster: { server: https://192.168.66.2:6443 }
contexts:
- name: oap-desktop
  context: { cluster: oap-desktop, user: oap-desktop }
current-context: oap-desktop
users:
- name: oap-desktop
  user: {}
`)
	ip, err := GuestIPFromKubeconfig(kc, "oap-desktop")
	require.NoError(t, err)
	assert.Equal(t, "192.168.66.2", ip)

	_, err = GuestIPFromKubeconfig(kc, "missing")
	assert.Error(t, err)
}

type fakeRunner struct {
	lastCmd   string
	stdinSeen string
	out       string
}

func (f *fakeRunner) Run(_ context.Context, _, _, _, cmd string, stdin io.Reader) ([]byte, error) {
	f.lastCmd = cmd
	if stdin != nil {
		b, _ := io.ReadAll(stdin)
		f.stdinSeen = string(b)
	}
	return []byte(f.out), nil
}

type fakeSaver struct{ data string }

func (f fakeSaver) Save(context.Context, string) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader(f.data)), nil
}

func TestLoadImageStreamsSaveIntoImport(t *testing.T) {
	r := &fakeRunner{}
	require.NoError(t, LoadImage(context.Background(), r, fakeSaver{data: "TARBYTES"}, "/k", "1.2.3.4", "sre-sandbox:dev"))
	assert.Contains(t, r.lastCmd, "k3s ctr -n k8s.io images import")
	assert.Equal(t, "TARBYTES", r.stdinSeen, "the docker save stream is piped to the guest import")
}

func TestHasImageMatchesNormalizedRef(t *testing.T) {
	r := &fakeRunner{out: "docker.io/library/sre-sandbox:dev\nghcr.io/x/y:1\n"}
	got, err := HasImage(context.Background(), r, "/k", "1.2.3.4", "sre-sandbox:dev")
	require.NoError(t, err)
	assert.True(t, got, "bare tag must match its containerd-normalized form")

	r.out = "ghcr.io/x/y:1\n"
	got, err = HasImage(context.Background(), r, "/k", "1.2.3.4", "sre-sandbox:dev")
	require.NoError(t, err)
	assert.False(t, got)
}

func TestVMKeyPathEnvOverride(t *testing.T) {
	key := filepath.Join(t.TempDir(), "ap-vm-key")
	require.NoError(t, os.WriteFile(key, []byte("x"), 0o600))
	t.Setenv("OAP_DESKTOP_SSH_KEY", key)
	got, err := VMKeyPath()
	require.NoError(t, err)
	assert.Equal(t, key, got)
}

func TestVMKeyPathDeprecatedEnvOverride(t *testing.T) {
	key := filepath.Join(t.TempDir(), "ap-vm-key")
	require.NoError(t, os.WriteFile(key, []byte("x"), 0o600))
	t.Setenv("AP_DESKTOP_SSH_KEY", key)
	got, err := VMKeyPath()
	require.NoError(t, err)
	assert.Equal(t, key, got, "the deprecated AP_DESKTOP_SSH_KEY name must still work")
}

type errCloser struct{ closeErr error }

func (errCloser) Read(p []byte) (int, error) { return 0, io.EOF }
func (e errCloser) Close() error             { return e.closeErr }

type errCloseSaver struct{ closeErr error }

func (e errCloseSaver) Save(context.Context, string) (io.ReadCloser, error) {
	return errCloser{closeErr: e.closeErr}, nil
}

func TestLoadImageSurfacesSaveError(t *testing.T) {
	r := &fakeRunner{} // its Run returns nil error, so closeErr is what surfaces
	err := LoadImage(context.Background(), r, errCloseSaver{closeErr: errors.New("save exploded")}, "/k", "1.2.3.4", "sre-sandbox:dev")
	require.Error(t, err)
	assert.ErrorContains(t, err, "save exploded")
}
