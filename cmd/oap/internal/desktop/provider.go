package desktop

import (
	"context"
	"errors"
)

var ErrUnsupportedPlatform = errors.New("desktop: VM provider requires macOS on Apple Silicon")

type State string

const (
	StateUnknown  State = "unknown"
	StateStopped  State = "stopped"
	StateStarting State = "starting"
	StateRunning  State = "running"
	StateError    State = "error"
)

// VMConfig describes the VM to provision. Paths point into the .app's
// Resources at runtime.
type VMConfig struct {
	DiskImagePath string // raw rootfs.img (k3s baked in)
	EFIStorePath  string // EFI variable store (created if absent)
	CPUCount      uint
	MemoryBytes   uint64
	HostShareDir  string // virtiofs: host dir mounted into guest (persistent memory)
	HostShareTag  string // virtiofs mount tag (guest mounts -t virtiofs <tag>)
	// SSHKeyPath is the PRIVATE half of the ed25519 keypair baked at build
	// time (mage desktop:rootfs's desktopGenerateSSHKeypair bakes the
	// public half into the guest's /root/.ssh/authorized_keys; Desktop.App
	// stages this private half at Contents/Resources/ap-vm-key). Used by
	// Kubeconfig to fetch the k3s kubeconfig over SSH.
	SSHKeyPath string
}

// ClusterProvider abstracts the local cluster runtime. One impl today
// (vz / Virtualization.framework); Lima/cloud can slot in later.
type ClusterProvider interface {
	Provision(ctx context.Context) error
	Start(ctx context.Context) error
	Stop(ctx context.Context) error
	Status(ctx context.Context) (State, error)
	Destroy(ctx context.Context) error
	// GuestIP returns the host-reachable NAT IP of the running guest
	// (discovered via the vsock IP-report handshake).
	GuestIP(ctx context.Context) (string, error)
	// Kubeconfig fetches the guest's RAW k3s kubeconfig over SSH (server
	// still 127.0.0.1 — it is not rewritten here) using the already-
	// resolved guest ip (Engine.Up calls GuestIP before Kubeconfig and
	// threads the result through). The caller combines the result with
	// GuestIP and calls RewriteKubeconfigServer to point the server URL at
	// the guest's host-reachable NAT IP.
	Kubeconfig(ctx context.Context, ip string) ([]byte, error)
}
