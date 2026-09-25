//go:build darwin && arm64

// Package vz is the darwin/arm64 Virtualization.framework-backed
// desktop.ClusterProvider. It boots a Linux guest (k3s baked into the
// disk image by the Phase-2 build) via Apple's Virtualization.framework
// through the github.com/Code-Hex/vz/v3 bindings.
//
// # Guest <-> host handoff
//
// Two pieces of guest state have to cross into the host process, and
// they use two different mechanisms on purpose:
//
//   - GuestIP: PRIMARY mechanism is lease-based discovery — this
//     provider pins a random locally-administered MAC on the NAT
//     network device at boot (buildConfig) and remembers it, then
//     polls macOS's own NAT DHCP server's lease table
//     (/var/db/dhcpd_leases, world-readable, see lease.go) for the
//     block whose hw_address matches that MAC. This works even when
//     the guest agent's vsock handshake never completes (observed in
//     practice: k3s fully up and NAT-reachable, vsock handshake
//     stuck). The vsock handshake described below is kept as a
//     CONCURRENT fallback — whichever resolves first wins — because
//     it needs no assumption about the host's NAT/DHCP implementation
//     and remains the only mechanism on host OSes without an
//     inspectable lease file.
//   - Historically (and still as fallback): the guest agent actively
//     DIALS the host over vsock on guestIPVsockPort as soon as its NAT
//     interface is up, writing a single newline-terminated IP string
//     and closing the connection. This direction (guest -> host) is
//     required because there's no other way for the host to learn an
//     externally-meaningless "guest CID" maps to a particular NAT IP;
//     the guest is the only side that knows its own address.
//   - Kubeconfig: the host fetches /etc/rancher/k3s/k3s.yaml (server
//     still "127.0.0.1") over SSH, as root, using the ed25519 keypair
//     `mage desktop:rootfs` bakes fresh into every build — the PUBLIC
//     half lands in the guest's /root/.ssh/authorized_keys
//     (build/desktop/rootfs-customize.sh), the PRIVATE half ships in
//     the .app at Contents/Resources/ap-vm-key (VMConfig.SSHKeyPath).
//     This replaced an earlier virtio-fs-share handoff (a guest-side
//     script copied the kubeconfig into the same host share
//     VMConfig.HostShareDir backs persistent memory with): that share
//     was observed coming up empty in practice (sshd, by contrast, is
//     a well-understood, independently pollable service — see
//     Kubeconfig's retry loop). Kubeconfig() returns the RAW guest
//     kubeconfig (server still 127.0.0.1) — orchestrate.go's Engine.Up
//     rewrites the server URL to GuestIP via RewriteKubeconfigServer;
//     this provider does not duplicate that step.
package vz

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/Code-Hex/vz/v3"
	"golang.org/x/crypto/ssh"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/desktop"
)

const (
	// guestIPVsockPort is the vsock port the guest agent connects OUT to
	// (guest -> host) to report its NAT IP once networking is up. Chosen
	// arbitrarily from the unreserved range; must match the guest agent.
	guestIPVsockPort uint32 = 5051

	// serialLogFileName is the guest serial console capture, written
	// alongside the EFI variable store.
	serialLogFileName = "console.log"

	// guestIPWaitTimeout bounds how long GuestIP blocks waiting for a
	// REACHABLE guest address — from lease-based discovery whose candidate
	// answered a TCP probe, or from the vsock handshake — before giving up
	// (absent a caller-supplied deadline that fires first).
	//
	// This constant changed meaning when pollLeaseForMAC started probing.
	// Before, a lease left over from a previous boot satisfied GuestIP
	// instantly, so this window never actually bounded anything and kernel
	// boot, the NIC, and sshd were all absorbed — invisibly — by the much
	// longer kubeconfigSSHWaitTimeout downstream. Now this window genuinely
	// owns "guest boots far enough to accept a connection", which is what it
	// always claimed to own.
	//
	// Four minutes, not the two it held while the constant was inert: sshd has
	// been observed answering at ~2m30s on a busy host, so keeping two would
	// convert a boot the old code survived by accident into a hard failure at
	// the newly-enforced ceiling. The ceiling costs nothing on a normal boot —
	// leasePollInterval re-probes continuously and a guest that comes up in
	// thirty seconds is picked up then.
	guestIPWaitTimeout = 4 * time.Minute

	// localNetworkSuspicionAfter is how long the two conditions below must
	// BOTH hold before bring-up stops waiting and names the cause.
	//
	// Generous on purpose. It is not a boot budget — a guest that is merely
	// slow keeps refusing the port, which clears the suspicion outright — so
	// this only has to outlast the window between the guest's DHCP lease
	// appearing and its interface carrying traffic. Erring long costs a slow
	// host nothing; erring short would fail a legitimate boot on a heuristic.
	localNetworkSuspicionAfter = 45 * time.Second

	// leasePollInterval is how often GuestIP re-reads dhcpLeaseFilePath
	// while waiting for the pinned MAC to show up in the NAT DHCP lease
	// table.
	leasePollInterval = 1 * time.Second

	// vmStopTimeout bounds how long Stop waits for a clean guest
	// shutdown (RequestStop) before falling back to a hard Stop.
	vmStopTimeout = 30 * time.Second

	// sshUser is the only account the guest permits SSH login as — see
	// build/desktop/sshd-ap.conf (root, key-only).
	sshUser = "root"

	// sshPort is the guest's sshd listen port (build/desktop's baked
	// ssh.service; standard port, never remapped).
	sshPort = 22

	// sshDialTimeout bounds one whole attempt within Kubeconfig's retry
	// loop — TCP connect, SSH handshake, AND the exec+read-stdout
	// conversation (see fetchKubeconfigOverSSH's explicit conn.SetDeadline;
	// golang.org/x/crypto/ssh applies no timeout of its own once past the
	// initial dial) — short, because a slow/unreachable/stuck attempt
	// should fail fast and let the loop retry rather than eat into the
	// overall kubeconfigSSHWaitTimeout budget.
	sshDialTimeout = 5 * time.Second

	// guestReachableProbeTimeout bounds ONE liveness probe of a candidate
	// address from the DHCP lease table (see pollLeaseForMAC). Short on
	// purpose: the probe runs on every poll tick, and its only job is to tell
	// a guest that is listening now from a lease left behind by a previous
	// boot. A guest that needs longer than this to accept a TCP connection is
	// simply not ready yet, which the next tick re-asks.
	guestReachableProbeTimeout = 2 * time.Second

	// kubeconfigSSHWaitTimeout bounds how long Kubeconfig retries fetching
	// the guest's kubeconfig over SSH.
	//
	// This budget starts once the guest is REACHABLE, not at VM boot:
	// pollLeaseForMAC probes a candidate lease address before returning it,
	// so GuestIP no longer answers from a lease that predates the guest and
	// this window no longer silently absorbs kernel boot and the NIC coming
	// up. What is left for it to cover is sshd finishing its startup and k3s
	// reaching a serving apiserver — the two things its name implies.
	//
	// Five minutes is kept even though the window now covers less than it
	// did. k3s on a cold, busy host is the slow part and was never the part
	// that was over-budgeted, and the ceiling costs nothing on a fast boot:
	// kubeconfigPollInterval is 2s, so a ready guest is picked up on the next
	// tick. It is the ceiling for a slow one that matters.
	kubeconfigSSHWaitTimeout = 5 * time.Minute

	// kubeconfigPollInterval is how often Kubeconfig retries the SSH fetch
	// while waiting for sshd and then k3s to come up.
	kubeconfigPollInterval = 2 * time.Second

	// k3sKubeconfigGuestPath is the file k3s writes its kubeconfig to
	// inside the guest (server still "127.0.0.1" — see the package doc's
	// guest<->host handoff section).
	k3sKubeconfigGuestPath = "/etc/rancher/k3s/k3s.yaml"
)

// fixedGuestMAC is the stable locally-administered MAC every desktop guest
// boots with (see buildConfig for the rationale: a fixed MAC + the guest's
// ClientIdentifier=mac DHCP setting yields a stable lease/IP/ARP every boot,
// instead of the churn a random MAC + DUID client-id caused). The 0x0a first
// octet is locally-administered + unicast, and is distinct from Docker
// Desktop's vmnet MAC.
var fixedGuestMAC = net.HardwareAddr{0x0a, 0x01, 0x0a, 0x70, 0x00, 0x01}

// Provider is the darwin/arm64 Virtualization.framework-backed
// ClusterProvider.
type Provider struct {
	cfg desktop.VMConfig

	mu       sync.Mutex
	vm       *vz.VirtualMachine
	listener *vz.VirtioSocketListener
	mac      string // pinned NAT MAC for the current boot, set by buildConfig; matched against dhcpLeaseFilePath

	guestIPMu sync.RWMutex
	guestIP   string
	guestErr  error
	guestIPCh chan struct{} // closed once guestIP/guestErr are set for the current boot

	// guestProbePort overrides the port a candidate guest address is probed
	// on (see probePort). Zero — always, in production — means sshPort; it
	// exists so tests can point the probe at an ephemeral listener rather
	// than needing to bind privileged :22.
	guestProbePort int
}

// New constructs a Provider for cfg. It does not touch the filesystem or
// the Virtualization.framework; see Provision and Start.
func New(cfg desktop.VMConfig) *Provider { return &Provider{cfg: cfg} }

var _ desktop.ClusterProvider = (*Provider)(nil)

func (p *Provider) serialLogPath() string {
	return filepath.Join(filepath.Dir(p.cfg.EFIStorePath), serialLogFileName)
}

// Provision ensures the on-disk state Start needs exists: the disk image
// (baked/imported elsewhere — Provision only verifies it's present), the
// EFI variable store (created fresh if absent), and the virtio-fs host
// share directory (created if configured and absent).
func (p *Provider) Provision(ctx context.Context) error {
	if _, err := os.Stat(p.cfg.DiskImagePath); err != nil {
		return fmt.Errorf("vz: disk image %q: %w", p.cfg.DiskImagePath, err)
	}

	if err := os.MkdirAll(filepath.Dir(p.cfg.EFIStorePath), 0o700); err != nil {
		return fmt.Errorf("vz: create EFI store directory: %w", err)
	}
	if _, err := os.Stat(p.cfg.EFIStorePath); errors.Is(err, os.ErrNotExist) {
		store, err := vz.NewEFIVariableStore(p.cfg.EFIStorePath, vz.WithCreatingEFIVariableStore())
		if err != nil {
			return fmt.Errorf("vz: create EFI variable store: %w", err)
		}
		_ = store
	} else if err != nil {
		return fmt.Errorf("vz: stat EFI variable store: %w", err)
	}

	if p.cfg.HostShareDir != "" {
		if err := os.MkdirAll(p.cfg.HostShareDir, 0o700); err != nil {
			return fmt.Errorf("vz: create host share directory: %w", err)
		}
	}
	return nil
}

// buildConfig assembles the VirtualMachineConfiguration for cfg: EFI
// boot, virtio-blk root disk, NAT networking with a pinned random MAC,
// a vsock device, an optional virtio-fs host share, an entropy device,
// and a serial console to a log file.
func (p *Provider) buildConfig() (*vz.VirtualMachineConfiguration, error) {
	// The EFI store and the serial log (see serialLogPath) both live in
	// filepath.Dir(p.cfg.EFIStorePath). Provision() normally creates this
	// directory ahead of time, but buildConfig must not assume Provision
	// ran first — ensure the parent directory exists before anything
	// below tries to create a file in it, starting with the EFI store.
	if err := os.MkdirAll(filepath.Dir(p.cfg.EFIStorePath), 0o700); err != nil {
		return nil, fmt.Errorf("vz: create EFI store directory: %w", err)
	}

	var (
		store *vz.EFIVariableStore
		err   error
	)
	if _, statErr := os.Stat(p.cfg.EFIStorePath); errors.Is(statErr, os.ErrNotExist) {
		store, err = vz.NewEFIVariableStore(p.cfg.EFIStorePath, vz.WithCreatingEFIVariableStore())
	} else {
		store, err = vz.NewEFIVariableStore(p.cfg.EFIStorePath)
	}
	if err != nil {
		return nil, fmt.Errorf("vz: EFI variable store: %w", err)
	}
	loader, err := vz.NewEFIBootLoader(vz.WithEFIVariableStore(store))
	if err != nil {
		return nil, fmt.Errorf("vz: EFI boot loader: %w", err)
	}

	cfg, err := vz.NewVirtualMachineConfiguration(loader, p.cfg.CPUCount, p.cfg.MemoryBytes)
	if err != nil {
		return nil, fmt.Errorf("vz: virtual machine configuration: %w", err)
	}

	// Root disk (raw image only; k3s is baked in by the Phase-2 build).
	diskAtt, err := vz.NewDiskImageStorageDeviceAttachment(p.cfg.DiskImagePath, false)
	if err != nil {
		return nil, fmt.Errorf("vz: disk image attachment: %w", err)
	}
	blk, err := vz.NewVirtioBlockDeviceConfiguration(diskAtt)
	if err != nil {
		return nil, fmt.Errorf("vz: virtio-blk configuration: %w", err)
	}
	cfg.SetStorageDevicesVirtualMachineConfiguration([]vz.StorageDeviceConfiguration{blk})

	// NAT networking (host-reachable 192.168.64.x-style IP). Pin a MAC so
	// the guest sees a stable address across boots.
	natAtt, err := vz.NewNATNetworkDeviceAttachment()
	if err != nil {
		return nil, fmt.Errorf("vz: NAT attachment: %w", err)
	}
	netCfg, err := vz.NewVirtioNetworkDeviceConfiguration(natAtt)
	if err != nil {
		return nil, fmt.Errorf("vz: network device configuration: %w", err)
	}
	// Use a FIXED (not random-per-boot) locally-administered MAC. Combined
	// with the guest's `ClientIdentifier=mac` DHCP setting (see
	// build/desktop/10-ap-dhcp.network), this makes the guest present a
	// stable DHCP client identity every boot, so macOS's vmnet hands it the
	// SAME 192.168.64.x lease each time. A random MAC + the guest's default
	// DUID-based client id produced a NEW lease (and often a new IP) every
	// boot, which (a) piled up stale dead leases in dhcpLeaseFilePath and
	// (b) left the host's ARP cache pointing a reused IP at a dead guest's
	// MAC — GuestIP would then hand SSH a stale, unreachable address. A
	// fixed MAC keeps the IP + ARP stable and makes the lease match reliable.
	// One VM runs at a time (killOrphanVMs enforces this at startup), so a
	// fixed MAC can't collide with a sibling; it's locally-administered and
	// distinct from Docker Desktop's vmnet MAC.
	mac, err := vz.NewMACAddress(fixedGuestMAC)
	if err != nil {
		return nil, fmt.Errorf("vz: MAC address: %w", err)
	}
	netCfg.SetMACAddress(mac)
	cfg.SetNetworkDevicesVirtualMachineConfiguration([]*vz.VirtioNetworkDeviceConfiguration{netCfg})
	// Remember the pinned MAC (caller holds p.mu — see Start) so GuestIP
	// can match it against the host's NAT DHCP lease table.
	p.mac = mac.String()

	// vsock — see the package doc for the guest<->host handoff protocol.
	sock, err := vz.NewVirtioSocketDeviceConfiguration()
	if err != nil {
		return nil, fmt.Errorf("vz: vsock device configuration: %w", err)
	}
	cfg.SetSocketDevicesVirtualMachineConfiguration([]vz.SocketDeviceConfiguration{sock})

	// virtio-fs host share (Phase 4 backs persistent memory through
	// this; Kubeconfig also reads from it — see package doc).
	if p.cfg.HostShareDir != "" {
		shared, err := vz.NewSharedDirectory(p.cfg.HostShareDir, false)
		if err != nil {
			return nil, fmt.Errorf("vz: shared directory: %w", err)
		}
		single, err := vz.NewSingleDirectoryShare(shared)
		if err != nil {
			return nil, fmt.Errorf("vz: single directory share: %w", err)
		}
		fsCfg, err := vz.NewVirtioFileSystemDeviceConfiguration(p.cfg.HostShareTag)
		if err != nil {
			return nil, fmt.Errorf("vz: virtio-fs device configuration: %w", err)
		}
		fsCfg.SetDirectoryShare(single)
		cfg.SetDirectorySharingDevicesVirtualMachineConfiguration([]vz.DirectorySharingDeviceConfiguration{fsCfg})
	}

	// Entropy (avoids early-boot RNG stall).
	ent, err := vz.NewVirtioEntropyDeviceConfiguration()
	if err != nil {
		return nil, fmt.Errorf("vz: entropy device configuration: %w", err)
	}
	cfg.SetEntropyDevicesVirtualMachineConfiguration([]*vz.VirtioEntropyDeviceConfiguration{ent})

	// Serial console -> log file, for boot diagnostics.
	if err := os.MkdirAll(filepath.Dir(p.serialLogPath()), 0o700); err != nil {
		return nil, fmt.Errorf("vz: create serial log directory: %w", err)
	}
	serialAtt, err := vz.NewFileSerialPortAttachment(p.serialLogPath(), false)
	if err != nil {
		return nil, fmt.Errorf("vz: serial log attachment: %w", err)
	}
	serialCfg, err := vz.NewVirtioConsoleDeviceSerialPortConfiguration(serialAtt)
	if err != nil {
		return nil, fmt.Errorf("vz: serial console configuration: %w", err)
	}
	cfg.SetSerialPortsVirtualMachineConfiguration([]*vz.VirtioConsoleDeviceSerialPortConfiguration{serialCfg})

	if ok, err := cfg.Validate(); !ok || err != nil {
		return nil, fmt.Errorf("vz: invalid configuration: %w", err)
	}
	return cfg, nil
}

// Start builds the configuration, boots the VM, and kicks off the vsock
// listener that will receive the guest's IP (see receiveGuestIP). It is
// idempotent: calling Start while the current VM is still alive (any
// state other than Stopped/Error — Running, Starting, Pausing, Paused,
// Resuming, Stopping, Saving, Restoring) is a no-op. This isn't just an
// optimization: creating a second *vz.VirtualMachine while the first is
// still alive would also spawn a second receiveGuestIP goroutine for no
// reason, so the guard doubles as the "at most one receiver per live VM
// instance" invariant receiveGuestIP's doc comment relies on.
func (p *Provider) Start(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.vm != nil && vmIsAlive(p.vm) {
		return nil
	}

	cfg, err := p.buildConfig()
	if err != nil {
		return err
	}
	vm, err := vz.NewVirtualMachine(cfg)
	if err != nil {
		return fmt.Errorf("vz: create virtual machine: %w", err)
	}
	if err := vm.Start(); err != nil {
		return fmt.Errorf("vz: start virtual machine: %w", err)
	}

	// If a previous VM instance's vsock receiver never completed its
	// handshake (guest never dialed in), it's about to be permanently
	// orphaned — see the receiveGuestIP doc comment for why that
	// goroutine can't be canceled. Surface it now, at the point the leak
	// actually happens, rather than leaving it silent.
	if p.vm != nil {
		p.guestIPMu.RLock()
		prevCh := p.guestIPCh
		p.guestIPMu.RUnlock()
		select {
		case <-prevCh:
			// Previous receiver already resolved (success or error) —
			// nothing leaked.
		default:
			slog.Warn("vz: starting a new VM instance while the previous instance's vsock IP receiver is still blocked waiting for the guest to dial in; that goroutine (and its *vz.VirtualMachine reference) will remain until process exit — see receiveGuestIP doc comment",
				"diskImagePath", p.cfg.DiskImagePath)
		}
	}

	p.vm = vm

	p.guestIPMu.Lock()
	p.guestIP = ""
	p.guestErr = nil
	ch := make(chan struct{})
	p.guestIPCh = ch
	p.guestIPMu.Unlock()

	go p.receiveGuestIP(vm, ch)

	return nil
}

// vmIsAlive reports whether vm still holds host resources — i.e. it
// hasn't reached a terminal state — and so must not be silently
// replaced by a fresh Start(). Stopped and Error are the only terminal
// states; everything else (including the pause/save/restore states this
// provider never itself enters, but which macOS can still report) counts
// as alive.
func vmIsAlive(vm *vz.VirtualMachine) bool {
	switch vm.State() {
	case vz.VirtualMachineStateStopped, vz.VirtualMachineStateError:
		return false
	default:
		return true
	}
}

// receiveGuestIP is the HOST side of the vsock IP handshake: it listens
// on guestIPVsockPort, accepts the guest agent's connection, reads a
// single newline-terminated IP string, and records the result on ch (the
// guestIPCh captured by Start for this specific boot — see the note on
// setGuestIPResult about why it's passed explicitly rather than read
// live off p.guestIPCh).
//
// # This goroutine can block forever, and that is a known library limit
//
// vz.VirtioSocketListener exposes no deadline and no cancelable accept:
// AcceptVirtioSocketConnection blocks until a connection arrives, full
// stop, and listener.Close() does NOT unblock a pending Accept. That's a
// limitation of the github.com/Code-Hex/vz/v3 binding (ultimately of
// Virtualization.framework's ObjC API), not a bug in this file. So if
// the guest never dials in — boot failure, guest agent crash before it
// reaches the NAT-up point, or the VM is torn down before the handshake
// — this goroutine sits in Accept forever, pinning a reference to vm
// (the native *vz.VirtualMachine) until the oap process exits. Do not
// "fix" this with a context/timeout wrapper around Accept itself: there
// is nothing in the library to cancel, so such a wrapper would abandon
// the blocked goroutine exactly as it does today while adding the
// illusion of cancellation.
//
// What IS done to bound the damage, given that constraint:
//   - Start() will not spawn a second receiver while the current VM
//     instance is still alive (see vmIsAlive) — at most one receiver
//     goroutine exists per live VM instance, so the leak is bounded to
//     one goroutine per Start-that-actually-creates-a-VM, not one per
//     Start() call.
//   - Each boot's receiver reports through the ch passed in here, not
//     through a live read of p.guestIPCh, so a late/stale result from an
//     orphaned goroutine (from a since-replaced VM instance) can never
//     clobber a later boot's in-progress or resolved GuestIP() result —
//     see setGuestIPResult.
//   - GuestIP() never blocks on this goroutine finishing: it selects
//     over ctx/guestIPWaitTimeout, so a hung receiver costs one leaked
//     goroutine + VM reference, never a hung caller.
//   - Start() logs a warning when it's about to orphan the previous
//     instance's receiver, so repeated Start-after-Stop cycles (each one
//     leaking a goroutine when the guest never completed its handshake)
//     are visible in logs instead of silently accumulating.
func (p *Provider) receiveGuestIP(vm *vz.VirtualMachine, ch chan struct{}) {
	socks := vm.SocketDevices()
	if len(socks) == 0 {
		p.setGuestIPResult(ch, "", fmt.Errorf("vz: no vsock device configured on this virtual machine"))
		return
	}

	listener, err := socks[0].Listen(guestIPVsockPort)
	if err != nil {
		p.setGuestIPResult(ch, "", fmt.Errorf("vz: listen on vsock port %d: %w", guestIPVsockPort, err))
		return
	}
	p.mu.Lock()
	p.listener = listener
	p.mu.Unlock()
	defer listener.Close()

	conn, err := listener.AcceptVirtioSocketConnection()
	if err != nil {
		p.setGuestIPResult(ch, "", fmt.Errorf("vz: accept vsock connection: %w", err))
		return
	}
	defer conn.Close()

	scanner := bufio.NewScanner(conn)
	if !scanner.Scan() {
		err := scanner.Err()
		if err == nil {
			err = fmt.Errorf("guest closed the vsock connection before sending an IP")
		}
		p.setGuestIPResult(ch, "", fmt.Errorf("vz: read guest IP over vsock: %w", err))
		return
	}
	ip := strings.TrimSpace(scanner.Text())
	if ip == "" {
		p.setGuestIPResult(ch, "", fmt.Errorf("vz: guest sent an empty IP over vsock"))
		return
	}
	p.setGuestIPResult(ch, ip, nil)
}

// setGuestIPResult records the outcome of the vsock handshake for one
// specific boot, identified by ch (the channel Start created and handed
// to receiveGuestIP for that boot) rather than whatever p.guestIPCh
// currently holds. That distinction matters once a receiver can outlive
// its VM instance (see receiveGuestIP's doc comment): without it, a
// stale result arriving after Start() has already moved on to a new
// boot would overwrite that new boot's p.guestIP/p.guestErr using the
// old boot's shared-field write, corrupting a result nothing asked for.
// Guarding the field write on ch == p.guestIPCh confines every write to
// its own generation; the close(ch) always happens so any caller
// blocked on this specific ch (there can be at most one live GuestIP
// caller per boot in practice) still unblocks.
func (p *Provider) setGuestIPResult(ch chan struct{}, ip string, err error) {
	p.guestIPMu.Lock()
	defer p.guestIPMu.Unlock()
	select {
	case <-ch:
		// Already resolved for this boot (shouldn't happen — the
		// handshake goroutine only ever calls this once — but guard
		// against a double-fire rather than panic on a closed channel).
	default:
		if ch == p.guestIPCh {
			p.guestIP = ip
			p.guestErr = err
		}
		close(ch)
	}
}

// GuestIP returns an address at which the guest for THIS boot is reachable.
// It races two discovery mechanisms and returns whichever resolves first:
//
//   - PRIMARY: polling the host's own NAT DHCP lease table
//     (dhcpLeaseFilePath) for the MAC pinned in buildConfig, and probing
//     each matching address before accepting it — see pollLeaseForMAC. It
//     depends only on macOS's NAT/DHCP implementation plus a TCP connect,
//     not on guest-side agent code reaching a particular boot stage.
//   - FALLBACK: the vsock handshake (see receiveGuestIP), kept
//     concurrently for hosts/configurations where the lease file isn't
//     usable.
//
// "Reachable" is load-bearing, not decoration, and it applies to BOTH
// sources. fixedGuestMAC deliberately keeps this guest's lease across boots,
// so on every boot after the first a matching lease exists BEFORE the guest
// does. Returning that address unprobed made this function answer instantly
// and always from the lease — the vsock fallback could never win — and handed
// callers an address that might belong to no running guest at all. The
// caller's own much longer wait then absorbed the entire guest boot and
// blamed whatever it was waiting for.
//
// The vsock source was then left exempt from the same rule, which reopened
// the identical hole from the other side. vsock is NOT IP networking — it is
// a virtio device — so the handshake completes perfectly on a host that
// cannot route a single packet to the guest. In production that exempt path
// won the race in six seconds on a host where macOS's Local Network privacy
// gate was dropping this process's LAN traffic; GuestIP returned an address
// this process could never reach, bring-up advanced past "Waiting for guest
// network", and the failure surfaced five minutes later one step downstream,
// blaming k3s. A reported address proves the guest knows its own IP; only a
// probe proves THIS process can get to it, and that is what the caller is
// about to depend on. So the vsock address is a candidate too, re-probed
// until it answers — see pollVsockGuestIP.
//
// Whichever source produces a REACHABLE address first wins; GuestIP does not
// wait for the other. It blocks until one of them succeeds, both are known to
// be unable to (see the vsock-failure case), ctx is done, or
// guestIPWaitTimeout elapses — whichever comes first. A timeout here does
// NOT clean up the receiveGuestIP goroutine still waiting on the accept
// behind it; see that function's doc comment.
func (p *Provider) GuestIP(ctx context.Context) (string, error) {
	p.guestIPMu.Lock()
	ch := p.guestIPCh
	p.guestIPMu.Unlock()
	if ch == nil {
		return "", fmt.Errorf("vz: virtual machine not started")
	}

	p.mu.Lock()
	mac := p.mac
	p.mu.Unlock()

	waitCtx, cancel := context.WithTimeout(ctx, guestIPWaitTimeout)
	defer cancel()

	// Buffered so a source that resolves just as this function returns on
	// another path never blocks its goroutine forever on an unread send.
	reachable := make(chan string, 2)
	witness := &reachabilityWitness{}
	if mac != "" {
		go p.pollLeaseForMAC(waitCtx, mac, reachable, witness)
	}

	// Fail FAST on the one signature that is diagnosable rather than waiting
	// out the full budget and handing the operator three maybes. A lease
	// proves the guest booted and its interface came up; a refusal would prove
	// our packets reach it. Lease and no refusal, sustained, means the guest is
	// up and this process cannot talk to it — which on macOS is overwhelmingly
	// the Local Network privacy prompt having been dismissed or denied.
	suspicion := time.NewTimer(localNetworkSuspicionAfter)
	defer suspicion.Stop()
	vsockFailed := make(chan error, 1)
	go p.pollVsockGuestIP(waitCtx, ch, reachable, vsockFailed)

	var vsockErr error
	for {
		select {
		case ip := <-reachable:
			return ip, nil
		case err := <-vsockFailed:
			// One source is out. That ENDS the race only when it was the
			// only source: the lease poller has its own, independent path to
			// an address and may still be seconds away from finding one.
			// Returning here regardless is how a vsock listen error used to
			// abort a bring-up the lease table would have rescued.
			vsockErr = err
			vsockFailed = nil // nil channel: never selected again
			if mac == "" {
				return "", fmt.Errorf("vz: %w", err)
			}
		case <-suspicion.C:
			if !witness.sawLease.Load() || witness.sawRefusal.Load() {
				continue // not this failure; let the full budget run
			}
			slog.Warn("vz: guest has a DHCP lease but nothing has ever answered on it; treating as a host-side network block rather than waiting out the remaining budget",
				"leaseFile", dhcpLeaseFilePath, "mac", mac, "sshPort", p.probePort(),
				"after", localNetworkSuspicionAfter.String(), "budget", guestIPWaitTimeout.String())
			return "", fmt.Errorf(
				"vz: the virtual machine booted and took a DHCP lease, but nothing answered on port %d for %s and no connection was ever refused either — "+
					"a refusal would mean the guest is merely still starting, so this is almost certainly this process being blocked from the local network. "+
					"On macOS: System Settings > Privacy & Security > Local Network, and enable the entry for this app. "+
					"Then run it again. (If Local Network is already enabled, the guest may have booted without bringing its interface up — check the VM console.)",
				p.probePort(), localNetworkSuspicionAfter)
		case <-waitCtx.Done():
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			slog.Warn("vz: timed out waiting for a REACHABLE guest address; neither a DHCP lease whose address accepted a connection on the ssh port nor an address reported over vsock answered on that port in time — the guest may not have booted far enough to bring its interface up, the guest agent may never have connected, or this process may be unable to reach the guest at all (on macOS 15, check System Settings > Privacy & Security > Local Network). A lease alone is not enough: fixedGuestMAC keeps this guest's lease across boots, so one can be present while nothing is listening. The vsock receiver goroutine for this boot remains blocked in Accept and will not be cleaned up until process exit (known vz-library limitation; see receiveGuestIP doc comment)",
				"timeout", guestIPWaitTimeout.String(), "diskImagePath", p.cfg.DiskImagePath, "leaseFile", dhcpLeaseFilePath, "mac", mac, "sshPort", p.probePort(), "vsockErr", vsockErr)
			return "", fmt.Errorf("vz: timed out after %s waiting for the guest to become reachable — nothing answered on port %d, neither an address from a DHCP lease in %s nor one reported by the guest over vsock; if the guest is in fact up, this process may be blocked from reaching it (macOS 15: System Settings > Privacy & Security > Local Network)", guestIPWaitTimeout, p.probePort(), dhcpLeaseFilePath)
		}
	}
}

// probePort is the port both discovery sources probe a candidate address on.
// Always sshPort in production; overridable only so tests can stand up a
// listener on an ephemeral port instead of requiring privileged :22.
func (p *Provider) probePort() int {
	if p.guestProbePort != 0 {
		return p.guestProbePort
	}
	return sshPort
}

// pollVsockGuestIP waits for this boot's vsock handshake to resolve, then
// subjects the address the guest reported to the same probe pollLeaseForMAC
// applies to a lease entry: a candidate, re-probed every leasePollInterval
// until it actually answers, never an answer on its own. A handshake that
// FAILED is reported on failed instead — it means this source can never
// produce a candidate, which the caller needs in order to stop waiting on it.
func (p *Provider) pollVsockGuestIP(ctx context.Context, ch chan struct{}, found chan<- string, failed chan<- error) {
	select {
	case <-ctx.Done():
		return
	case <-ch:
	}

	p.guestIPMu.RLock()
	ip, err := p.guestIP, p.guestErr
	p.guestIPMu.RUnlock()

	switch {
	case err != nil:
		select {
		case failed <- err:
		default:
		}
		return
	case ip == "":
		select {
		case failed <- fmt.Errorf("vsock handshake resolved without an address"):
		default:
		}
		return
	}

	probeUntilReachable(ctx, ip, p.probePort(), leasePollInterval, found)
}

// probeUntilReachable sends ip on found once ip:port accepts a TCP
// connection, re-probing every interval until it does or ctx is done. It is
// the shared "candidate -> answer" step both discovery sources go through:
// an address is only an answer once THIS process has proven it can reach it.
func probeUntilReachable(ctx context.Context, ip string, port int, interval time.Duration, found chan<- string) {
	addr := net.JoinHostPort(ip, strconv.Itoa(port))

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if tcpReachable(ctx, addr, guestReachableProbeTimeout) {
			select {
			case found <- ip:
			default:
			}
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// pollLeaseForMAC polls dhcpLeaseFilePath every leasePollInterval,
// looking for a lease entry whose hardware address matches mac (see
// leaseIPForMAC), and sends the discovered IP on found (buffered, size
// >=1) as soon as one appears. It returns without sending once ctx is
// done.
//
// A missing or unreadable lease file — the guest hasn't requested a
// lease yet, macOS hasn't created the file, a permissions surprise — is
// deliberately NOT treated as fatal here: it just means "no candidate
// this tick," so polling continues. The concurrent vsock handshake in
// receiveGuestIP exists precisely to cover the case where lease-based
// discovery never pans out (including on a hypothetical future host
// where this file doesn't exist at all); GuestIP's own timeout path logs
// and errors if BOTH mechanisms fail, so a persistently unreadable lease
// file is still surfaced, just not on every poll tick.
// reachabilityWitness records what the probes actually saw, so GuestIP can tell
// a slow boot from a blocked one instead of listing both as possibilities after
// four minutes.
type reachabilityWitness struct {
	sawLease   atomic.Bool // a lease matched our MAC: the guest booted and got an address
	sawRefusal atomic.Bool // something answered at least once: packets are getting through
}

func (p *Provider) pollLeaseForMAC(ctx context.Context, mac string, found chan<- string, witness *reachabilityWitness) {
	check := func() bool {
		raw, err := os.ReadFile(dhcpLeaseFilePath)
		if err != nil {
			return false
		}
		ip, ok := leaseIPForMAC(string(raw), mac)
		if !ok {
			return false
		}
		// A matching lease is a CANDIDATE, never an answer. fixedGuestMAC
		// deliberately keeps this guest's lease in the host's table across
		// boots, so on every boot after the first this entry is already
		// present before the guest exists — reading it proves only that this
		// VM has booted here before, not that anything is listening now.
		//
		// Returning it unproven is what made the vsock handshake dead code:
		// the lease won the race in GuestIP every single time, instantly, and
		// the address it produced was then handed to a caller that assumed a
		// live guest. When the address was stale the whole SSH budget was
		// spent dialing something that was never going to answer, and the
		// failure named ssh and k3s rather than the address.
		//
		// Probing the port the caller is about to use is what converts the
		// candidate into an answer, and it is what makes GuestIP's own
		// timeout mean "no guest became reachable" rather than "no lease
		// existed".
		// The lease is what proves the guest's interface came up and DHCP
		// completed, so from here on silence is about US reaching IT.
		witness.sawLease.Store(true)
		switch tcpProbe(ctx, net.JoinHostPort(ip, strconv.Itoa(p.probePort())), guestReachableProbeTimeout) {
		case probeReachable:
		case probeRefused:
			// Someone is home. Whatever is wrong, it is not that our packets
			// are being dropped — record that and keep waiting for sshd.
			witness.sawRefusal.Store(true)
			return false
		default:
			return false
		}
		select {
		case found <- ip:
		default:
		}
		return true
	}

	if check() {
		return
	}

	ticker := time.NewTicker(leasePollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if check() {
				return
			}
		}
	}
}

// tcpReachable reports whether addr accepts a TCP connection within timeout.
//
// Deliberately only a connect: the question it answers is "is there a guest
// listening here for THIS boot", not "is sshd ready to authenticate me". A
// half-started sshd that accepts and never speaks is still proof the guest is
// alive and the address is current, which is all the caller needs before
// starting a longer, better-named wait against it.
// probeOutcome distinguishes the two failure shapes a bool cannot, and which
// mean opposite things about what to do next.
//
// A REFUSAL is good news: something at that address answered, so packets reach
// the guest and only sshd is missing — the guest is still booting and waiting
// is exactly right. Silence is the bad kind: nothing came back at all, which is
// what both a dead address and a BLOCKED one look like.
type probeOutcome int

const (
	probeReachable probeOutcome = iota
	probeRefused                // answered: host up, port not listening yet
	probeSilent                 // timed out / no route: nothing came back
)

// tcpProbe dials addr and classifies the result. The error is the whole point:
// tcpReachable used to discard it, which is why a four-minute wait could not
// tell "the guest is still starting sshd" from "this process cannot reach the
// guest at all" and had to offer the operator both as guesses.
func tcpProbe(ctx context.Context, addr string, timeout time.Duration) probeOutcome {
	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var dialer net.Dialer
	conn, err := dialer.DialContext(probeCtx, "tcp", addr)
	if err == nil {
		_ = conn.Close()
		return probeReachable
	}
	if errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNREFUSED) {
		return probeRefused
	}
	return probeSilent
}

func tcpReachable(ctx context.Context, addr string, timeout time.Duration) bool {
	return tcpProbe(ctx, addr, timeout) == probeReachable
}

// Kubeconfig fetches the guest's k3s kubeconfig over SSH as root, using
// VMConfig.SSHKeyPath (the private half of the keypair baked into this
// build — see the package doc's guest<->host handoff section). It retries
// for up to kubeconfigSSHWaitTimeout: sshd may not have finished starting
// the moment ip becomes reachable, and k3s only writes
// k3sKubeconfigGuestPath once its apiserver is actually serving. The
// server URL is NOT rewritten here; callers (orchestrate.go's Engine.Up)
// combine the result with GuestIP via RewriteKubeconfigServer.
func (p *Provider) Kubeconfig(ctx context.Context, ip string) ([]byte, error) {
	if ip == "" {
		return nil, fmt.Errorf("vz: kubeconfig requires a guest ip")
	}
	if p.cfg.SSHKeyPath == "" {
		return nil, fmt.Errorf("vz: kubeconfig requires VMConfig.SSHKeyPath (baked build-time key)")
	}
	signer, err := loadSSHSigner(p.cfg.SSHKeyPath)
	if err != nil {
		return nil, fmt.Errorf("vz: %w", err)
	}

	clientCfg := &ssh.ClientConfig{
		User: sshUser,
		Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)},
		// The guest is a fresh disk image THIS build produced (see
		// build/desktop/rootfs-customize.sh) and is only ever reachable on
		// the host-only NAT network buildConfig sets up — there is no
		// third party positioned to present a spoofed host key on that
		// link, and the guest's host keys (build/desktop/ssh-hostkeys.service)
		// are regenerated every boot, so there is no stable fingerprint to
		// pin across boots even if this provider wanted to. TOFU/pinning
		// here would add friction without adding security.
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), //nolint:gosec // see comment above
		// No Timeout here: it would only bound ssh.Dial's own net.Dial step,
		// which this code doesn't use (see fetchKubeconfigOverSSH's doc
		// comment) — the real per-attempt bound is the conn.SetDeadline that
		// function sets from sshDialTimeout.
	}
	addr := net.JoinHostPort(ip, strconv.Itoa(sshPort))

	raw, err := pollKubeconfigOverSSH(ctx, addr, clientCfg, k3sKubeconfigGuestPath, kubeconfigSSHWaitTimeout, kubeconfigPollInterval, sshDialTimeout)
	if err != nil {
		return nil, fmt.Errorf("vz: %w", err)
	}
	return raw, nil
}

// loadSSHSigner reads and parses the unencrypted ed25519 private key at
// path (as generated by mage desktop:rootfs's desktopGenerateSSHKeypair).
func loadSSHSigner(path string) (ssh.Signer, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read ssh private key %q: %w", path, err)
	}
	signer, err := ssh.ParsePrivateKey(raw)
	if err != nil {
		return nil, fmt.Errorf("parse ssh private key %q: %w", path, err)
	}
	return signer, nil
}

// pollKubeconfigOverSSH retries fetchKubeconfigOverSSH every interval
// until it succeeds, ctx is done, or timeout elapses — whichever comes
// first. attemptTimeout bounds each individual fetchKubeconfigOverSSH
// call (see that function's doc comment). Factored out of Kubeconfig so
// tests can drive it against an in-process SSH server on an arbitrary
// loopback address with short interval/timeout/attemptTimeout values,
// instead of waiting out the real production ones.
func pollKubeconfigOverSSH(ctx context.Context, addr string, cfg *ssh.ClientConfig, guestPath string, timeout, interval, attemptTimeout time.Duration) ([]byte, error) {
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// This loop covers THREE different waits with one budget, and they fail
	// for unrelated reasons: nothing answers at addr at all; sshd answers but
	// will not let us through; sshd let us through and k3s has not written the
	// kubeconfig. The two flags record how far the loop ever actually got, so
	// the timeout can name the phase that ran out instead of reporting
	// whatever the last attempt happened to say.
	//
	// Both are STICKY on purpose. Progress once made is a fact about the guest
	// that a later failure does not retract: a guest that answered and then
	// went quiet is not the wrong-address failure, and reporting the last
	// attempt's dial error would put us straight back to the misleading
	// message.
	//
	// The message must also not out-claim its evidence. It used to assert that
	// a guest which never completed an SSH session "is not running or is not
	// at this address" — a conclusion the loop never checked and cannot check,
	// since it can only observe that ITS packets got no answer. That assertion
	// was flatly false in the failure that prompted this: the guest was
	// running, at that address, sshd listening and the kubeconfig already
	// written, while macOS silently dropped this process's packets. Whoever
	// reads this error is being sent to look somewhere, so it names what was
	// observed and leaves the conclusion open — see unreachableHint.
	var lastErr error
	reachedSSH := false
	connectedTCP := false
	for {
		raw, err := fetchKubeconfigOverSSH(waitCtx, addr, cfg, guestPath, attemptTimeout)
		if err == nil {
			return raw, nil
		}
		var notReady kubeconfigNotReadyError
		if errors.As(err, &notReady) {
			reachedSSH = true
		}
		var dialFailed dialFailedError
		if !errors.As(err, &dialFailed) {
			// Anything that is not a dial failure happened AFTER a connection
			// was established, so the address is right and something is
			// listening on it.
			connectedTCP = true
		}
		lastErr = err
		select {
		case <-waitCtx.Done():
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			switch {
			case reachedSSH:
				return nil, fmt.Errorf("timed out after %s waiting for k3s to write %s in the guest at %s "+
					"(ssh was reachable, so this is k3s taking too long to start, not a network problem): %w",
					timeout, guestPath, addr, lastErr)
			case connectedTCP:
				return nil, fmt.Errorf("timed out after %s waiting for an ssh session at %s — the address "+
					"accepted TCP connections, so the guest is running and correctly addressed, but sshd "+
					"never let us through (a mismatch between this build's key and the guest's "+
					"authorized_keys looks exactly like this): %w", timeout, addr, lastErr)
			default:
				return nil, fmt.Errorf("timed out after %s waiting for ssh at %s to answer — not one TCP "+
					"connection was ever established, so this is connectivity rather than k3s: the guest may "+
					"not be running, may be at a different address, or may be unreachable from this "+
					"process%s: %w", timeout, addr, unreachableHint(lastErr), lastErr)
			}
		case <-ticker.C:
		}
	}
}

// kubeconfigNotReadyError marks the failures that prove the guest IS
// reachable and authenticated — the SSH session ran, and only the kubeconfig
// itself was missing or empty. It is what lets the retry loop distinguish
// "waiting for k3s" from "waiting for a guest", which are the two phases this
// one loop covers and which need opposite things from whoever reads the error.
type kubeconfigNotReadyError struct{ err error }

func (e kubeconfigNotReadyError) Error() string { return e.err.Error() }
func (e kubeconfigNotReadyError) Unwrap() error { return e.err }

// dialFailedError marks the opposite end: the TCP connection never came up
// at all, so this host and that address never exchanged a byte. Together with
// kubeconfigNotReadyError it splits the retry loop's single budget into the
// three genuinely different phases it spans — nothing answered, sshd answered
// but would not let us through, sshd let us through but k3s has not written
// the file — so the timeout can report the one that actually ran out.
type dialFailedError struct{ err error }

func (e dialFailedError) Error() string { return e.err.Error() }
func (e dialFailedError) Unwrap() error { return e.err }

// unreachableHint returns the macOS-specific note that belongs on a dial
// which TIMED OUT, and "" for every other failure.
//
// The distinction carries the whole diagnosis. A guest that is absent, or an
// address that is wrong, refuses the connection or reports no route — fast,
// and unambiguously. SYNs that simply vanish mean something between this
// process and the wire is swallowing them, and on macOS 15 the common cause
// is the Local Network privacy gate: it drops an app's LAN traffic (the
// kernel logs "reason: NECP") with no prompt, no error the app can read, and
// nothing to distinguish it from a dead guest except that the dial times out
// instead of being refused. It is invisible from inside this process, so this
// message is the only place it can be named. A rebuilt .app is the usual
// trigger — ad-hoc signing gives every build a new code identity, so a grant
// the user made to the previous build no longer applies to this one.
func unreachableHint(err error) string {
	var ne net.Error
	if !errors.As(err, &ne) || !ne.Timeout() {
		return ""
	}
	return " — note the dial TIMED OUT rather than being refused, which is what a host-side block " +
		"looks like rather than a missing guest: if the guest is in fact up (try ssh'ing to it from " +
		"a terminal), enable this app under System Settings > Privacy & Security > Local Network"
}

// fetchKubeconfigOverSSH dials addr, opens one SSH session as cfg.User,
// and runs `cat guestPath`, returning stdout. A single attempt — the
// retry policy lives in pollKubeconfigOverSSH.
//
// attemptTimeout bounds the ENTIRE attempt: TCP connect, SSH handshake, and
// the exec+read-stdout conversation. ONE deadline governs all three, derived
// once from a per-attempt context and then reused on the raw connection.
// Both halves of that are load-bearing:
//
//   - Dialing under the per-attempt context, not the caller's, is what bounds
//     the CONNECT. Setting the connection deadline cannot do it — that call
//     can only run once the dial has already returned. A connect left to the
//     caller's context falls back to the OS ceiling (darwin's
//     net.inet.tcp.keepinit, 75s), which is what a host silently dropping
//     this process's SYNs actually costs: one attempt outlived thirty-seven
//     ticks of a 2s retry loop that believed it was retrying, so a guest that
//     came up during that window went unnoticed until the in-flight attempt
//     finally expired.
//   - Reusing the context's ABSOLUTE deadline on the connection, rather than
//     starting a fresh attemptTimeout after the dial, is what keeps the total
//     bounded. Restarting it let a slow connect and a slow handshake each
//     spend the full budget.
//
// cfg.Timeout is irrelevant here: it only bounds ssh.Dial's own net.Dial
// step, which this function does not use, and golang.org/x/crypto/ssh applies
// no timeout of its own to the handshake. Without the connection deadline a
// peer that accepts TCP but never speaks SSH — exactly what a not-yet-ready
// sshd can look like — would hang this attempt forever.
func fetchKubeconfigOverSSH(ctx context.Context, addr string, cfg *ssh.ClientConfig, guestPath string, attemptTimeout time.Duration) ([]byte, error) {
	attemptCtx, cancel := context.WithTimeout(ctx, attemptTimeout)
	defer cancel()
	// Always ok — WithTimeout just set one. It may be EARLIER than
	// attemptTimeout when the caller's own deadline is nearer, which is the
	// behaviour we want: the tighter of the two wins.
	deadline, _ := attemptCtx.Deadline()

	var dialer net.Dialer
	conn, err := dialer.DialContext(attemptCtx, "tcp", addr)
	if err != nil {
		return nil, dialFailedError{fmt.Errorf("dial %s: %w", addr, err)}
	}

	if err := conn.SetDeadline(deadline); err != nil {
		conn.Close()
		return nil, fmt.Errorf("set deadline on %s: %w", addr, err)
	}

	sshConn, chans, reqs, err := ssh.NewClientConn(conn, addr, cfg)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("ssh handshake with %s: %w", addr, err)
	}
	client := ssh.NewClient(sshConn, chans, reqs)
	defer client.Close()

	session, err := client.NewSession()
	if err != nil {
		return nil, fmt.Errorf("open ssh session: %w", err)
	}
	defer session.Close()

	var stdout, stderr bytes.Buffer
	session.Stdout = &stdout
	session.Stderr = &stderr
	// Past this point the guest is reachable AND authenticated us, so every
	// remaining failure is about k3s, not about the network or the address.
	// Marking them is what lets the retry loop's timeout say so.
	if err := session.Run("cat " + guestPath); err != nil {
		return nil, kubeconfigNotReadyError{fmt.Errorf("cat %s over ssh: %w (stderr: %q)",
			guestPath, err, strings.TrimSpace(stderr.String()))}
	}
	if stdout.Len() == 0 {
		return nil, kubeconfigNotReadyError{fmt.Errorf("cat %s over ssh: empty output (not yet written?)", guestPath)}
	}
	return stdout.Bytes(), nil
}

// Stop asks the guest to shut down cleanly (RequestStop), waits up to
// vmStopTimeout for VirtualMachineStateStopped, and falls back to a hard
// Stop if the guest doesn't get there in time.
func (p *Provider) Stop(ctx context.Context) error {
	p.mu.Lock()
	vm := p.vm
	p.mu.Unlock()
	if vm == nil {
		return nil
	}
	if vm.State() == vz.VirtualMachineStateStopped {
		return nil
	}

	if vm.CanRequestStop() {
		notify := vm.StateChangedNotify()
		if _, err := vm.RequestStop(); err == nil {
			stopCtx, cancel := context.WithTimeout(ctx, vmStopTimeout)
			defer cancel()
		waitForStop:
			for {
				select {
				case st := <-notify:
					if st == vz.VirtualMachineStateStopped {
						return nil
					}
				case <-stopCtx.Done():
					break waitForStop
				}
			}
		}
	}

	if vm.CanStop() {
		if err := vm.Stop(); err != nil {
			return fmt.Errorf("vz: stop virtual machine: %w", err)
		}
		return nil
	}

	return fmt.Errorf("vz: virtual machine cannot be stopped from state %s", vm.State())
}

// Status maps the vz execution state onto desktop.State. Pause/resume/
// save/restore states are never entered by this provider (it only ever
// calls Start/RequestStop/Stop), so they collapse conservatively into
// Running (still "up") rather than getting dedicated desktop.State
// values.
func (p *Provider) Status(ctx context.Context) (desktop.State, error) {
	p.mu.Lock()
	vm := p.vm
	p.mu.Unlock()
	if vm == nil {
		return desktop.StateStopped, nil
	}

	switch vm.State() {
	case vz.VirtualMachineStateStopped:
		return desktop.StateStopped, nil
	case vz.VirtualMachineStateStarting, vz.VirtualMachineStateResuming, vz.VirtualMachineStateRestoring:
		return desktop.StateStarting, nil
	case vz.VirtualMachineStateRunning, vz.VirtualMachineStatePausing, vz.VirtualMachineStatePaused,
		vz.VirtualMachineStateStopping, vz.VirtualMachineStateSaving:
		return desktop.StateRunning, nil
	case vz.VirtualMachineStateError:
		return desktop.StateError, fmt.Errorf("vz: virtual machine is in an error state")
	default:
		return desktop.StateUnknown, nil
	}
}

// Destroy stops the VM (if running) and removes the state this provider
// created: the disk image, the EFI variable store, and the serial log.
// It deliberately leaves VMConfig.HostShareDir alone — that directory
// holds the virtio-fs-backed persistent memory (Phase 4), which outlives
// VM destroy/recreate cycles by design (see the "remove what we added"
// convention).
func (p *Provider) Destroy(ctx context.Context) error {
	if err := p.Stop(ctx); err != nil {
		return fmt.Errorf("vz: stop before destroy: %w", err)
	}

	p.mu.Lock()
	if p.listener != nil {
		_ = p.listener.Close()
		p.listener = nil
	}
	p.vm = nil
	p.mu.Unlock()

	var errs []error
	for _, path := range []string{p.cfg.DiskImagePath, p.cfg.EFIStorePath, p.serialLogPath()} {
		if path == "" {
			continue
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, fmt.Errorf("remove %q: %w", path, err))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("vz: destroy: %w", errors.Join(errs...))
	}
	return nil
}
