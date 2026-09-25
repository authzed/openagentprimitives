package desktop

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"golang.org/x/crypto/ssh"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/authzed/openagentprimitives/pkg/x/envfallback"
)

// GuestIPFromKubeconfig reads the guest's host-reachable NAT IP from the
// named context's cluster server URL — the same URL MergeVMContext rewrote to
// point at the guest (see kubectx.go). A standalone CLI process thus reaches
// the VM without the running app's Provider handle.
func GuestIPFromKubeconfig(raw []byte, contextName string) (string, error) {
	cfg, err := clientcmd.Load(raw)
	if err != nil {
		return "", fmt.Errorf("desktop: parse kubeconfig: %w", err)
	}
	kctx, ok := cfg.Contexts[contextName]
	if !ok {
		return "", fmt.Errorf("desktop: kubeconfig has no context %q", contextName)
	}
	cl, ok := cfg.Clusters[kctx.Cluster]
	if !ok {
		return "", fmt.Errorf("desktop: context %q references missing cluster %q", contextName, kctx.Cluster)
	}
	u, err := url.Parse(cl.Server)
	if err != nil {
		return "", fmt.Errorf("desktop: parse server URL %q: %w", cl.Server, err)
	}
	host := u.Hostname()
	if host == "" {
		return "", fmt.Errorf("desktop: server URL %q has no host", cl.Server)
	}
	return host, nil
}

// SSHRunner runs one remote command in the guest over SSH, optionally feeding
// stdin, returning combined stdout.
type SSHRunner interface {
	Run(ctx context.Context, keyPath, addr, user, remoteCmd string, stdin io.Reader) ([]byte, error)
}

// ImageSaver streams `docker save <tag>` from the host daemon.
type ImageSaver interface {
	Save(ctx context.Context, tag string) (io.ReadCloser, error)
}

const guestSSHPort = "22"
const guestSSHUser = "root"

// KeyFileName is the on-disk filename of the oap-desktop VM SSH private key,
// shared by the app's stager (cmd/oap ensureVMKey) and VMKeyPath's discovery so
// the two ends can never drift.
const KeyFileName = "ap-vm-key"

// LoadImage streams `docker save <tag>` on the host into
// `k3s ctr -n k8s.io images import -` in the guest, so the kubelet sees the
// image (the k8s.io containerd namespace) without any registry.
func LoadImage(ctx context.Context, r SSHRunner, s ImageSaver, keyPath, guestIP, tag string) error {
	rc, err := s.Save(ctx, tag)
	if err != nil {
		return fmt.Errorf("desktop: docker save %s: %w", tag, err)
	}
	addr := net.JoinHostPort(guestIP, guestSSHPort)
	_, runErr := r.Run(ctx, keyPath, addr, guestSSHUser, "k3s ctr -n k8s.io images import -", rc)
	closeErr := rc.Close()
	if runErr != nil {
		return fmt.Errorf("desktop: import %s into VM: %w", tag, runErr)
	}
	if closeErr != nil {
		return fmt.Errorf("desktop: docker save %s failed: %w", tag, closeErr)
	}
	return nil
}

// HasImage reports whether ref is already present in the guest's k8s.io
// containerd namespace, matching a bare tag against its containerd-normalized
// form (docker.io/library/<tag>).
func HasImage(ctx context.Context, r SSHRunner, keyPath, guestIP, ref string) (bool, error) {
	addr := net.JoinHostPort(guestIP, guestSSHPort)
	out, err := r.Run(ctx, keyPath, addr, guestSSHUser, "k3s ctr -n k8s.io images ls -q", nil)
	if err != nil {
		return false, fmt.Errorf("desktop: list VM images: %w", err)
	}
	want := map[string]bool{ref: true}
	if !strings.Contains(ref, "/") {
		want["docker.io/library/"+ref] = true
	}
	for _, line := range strings.Split(string(out), "\n") {
		if want[strings.TrimSpace(line)] {
			return true, nil
		}
	}
	return false, nil
}

// dockerSaver is the production ImageSaver: `docker save <tag>` to a pipe.
type dockerSaver struct{}

func (dockerSaver) Save(ctx context.Context, tag string) (io.ReadCloser, error) {
	cmd := exec.CommandContext(ctx, "docker", "save", tag)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("desktop: docker save %s: stdout pipe: %w", tag, err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("desktop: docker save %s: start: %w", tag, err)
	}
	return &cmdReadCloser{cmd: cmd, r: stdout}, nil
}

type cmdReadCloser struct {
	cmd *exec.Cmd
	r   io.ReadCloser
}

func (c *cmdReadCloser) Read(p []byte) (int, error) { return c.r.Read(p) }
func (c *cmdReadCloser) Close() error {
	_ = c.r.Close()
	return c.cmd.Wait()
}

// NewDockerSaver returns the production ImageSaver.
func NewDockerSaver() ImageSaver { return dockerSaver{} }

// sshRunner is the production SSHRunner over golang.org/x/crypto/ssh.
type sshRunner struct{}

// NewSSHRunner returns the production SSHRunner.
func NewSSHRunner() SSHRunner { return sshRunner{} }

func (sshRunner) Run(ctx context.Context, keyPath, addr, user, remoteCmd string, stdin io.Reader) ([]byte, error) {
	key, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, fmt.Errorf("desktop: read VM ssh key %s: %w", keyPath, err)
	}
	signer, err := ssh.ParsePrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("desktop: parse VM ssh key: %w", err)
	}
	// The guest is a locally-provisioned VM whose host key we don't persist;
	// the trust root is the private key + the loopback-only NAT address, so
	// InsecureIgnoreHostKey is acceptable here (matches Provider.Kubeconfig's
	// existing SSH to the same guest).
	cfg := &ssh.ClientConfig{
		User:            user,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
	}
	client, err := ssh.Dial("tcp", addr, cfg)
	if err != nil {
		return nil, fmt.Errorf("desktop: ssh dial %s: %w", addr, err)
	}
	defer client.Close()
	sess, err := client.NewSession()
	if err != nil {
		return nil, fmt.Errorf("desktop: ssh session: %w", err)
	}
	defer sess.Close()
	if stdin != nil {
		sess.Stdin = stdin
	}
	out, err := sess.CombinedOutput(remoteCmd)
	if err != nil {
		return out, fmt.Errorf("desktop: ssh run %q: %w (%s)", remoteCmd, err, strings.TrimSpace(string(out)))
	}
	return out, nil
}

// VMKeyPath locates the ed25519 private key used to SSH into the oap-desktop
// guest. A standalone `oap` binary isn't inside the .app bundle, so an explicit
// OAP_DESKTOP_SSH_KEY override (also surfaced as `--vm-ssh-key` on the install
// command) is the reliable path; the other two are best-effort auto-discovery.
// AP_DESKTOP_SSH_KEY is accepted as a deprecated fallback name.
func VMKeyPath() (string, error) {
	var candidates []string
	if env, usedDeprecated := envfallback.Get("OAP_DESKTOP_SSH_KEY", "AP_DESKTOP_SSH_KEY"); env != "" {
		if usedDeprecated {
			fmt.Fprintln(os.Stderr, "AP_DESKTOP_SSH_KEY is deprecated; use OAP_DESKTOP_SSH_KEY")
		}
		candidates = append(candidates, env)
	}
	if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates, filepath.Join(home, "Library", "Application Support", "oap", KeyFileName))
	}
	if exe, err := os.Executable(); err == nil {
		// Bundled app: <App>.app/Contents/MacOS/ap -> ../Resources/ap-vm-key
		candidates = append(candidates, filepath.Join(filepath.Dir(filepath.Dir(exe)), "Resources", KeyFileName))
	}
	for _, c := range candidates {
		if fi, err := os.Stat(c); err == nil && !fi.IsDir() {
			return c, nil
		}
	}
	return "", fmt.Errorf("desktop: VM ssh key not found; set OAP_DESKTOP_SSH_KEY or pass --vm-ssh-key (looked in: %s)", strings.Join(candidates, ", "))
}
