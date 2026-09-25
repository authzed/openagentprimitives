//go:build mage
// +build mage

package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/magefile/mage/mg"
	"github.com/magefile/mage/sh"

	"github.com/authzed/openagentprimitives/pkg/platform/apimage"
	"github.com/authzed/openagentprimitives/pkg/x/envfallback"
)

// Desktop groups the mage targets that build the double-clickable macOS
// bundle (sub-project A of the macOS-bundle-sqlite-memory feature): an
// offline k3s rootfs (Rootfs), the air-gapped image tarball baked into it
// (Images), and the final signed oap.app (App).
//
// Images and Rootfs need only Docker (+curl, go, zstd) and run fine
// wherever Docker is available, including this development environment —
// Rootfs does its qemu-img convert/resize and offline customization
// (formerly virt-customize/libguestfs, which needs a real or
// nested-capable hypervisor for its supermin appliance) inside a small
// Debian container via loop-mount, not on the host; see
// desktopBuildRootfsInDocker's doc comment and
// build/desktop/rootfs-customize.sh. Only App is host-gated: it cross-signs
// and cgo-builds a darwin/arm64 binary, so it must run on a real Apple
// Silicon Mac with Xcode Command Line Tools (`codesign`).
type Desktop mg.Namespace

// All runs the full desktop build chain in order — Images -> Rootfs -> App —
// producing the ad-hoc-signed build/desktop/out/oap.app. Single from-scratch
// entry point: App already transitively depends on Rootfs, which depends on
// Images (via mg.Deps), so this drives that chain and reports where the
// bundle landed. Requires the union of all three targets' tools
// (docker+buildx, curl, go, zstd, codesign) on a macOS host.
func (Desktop) All() error {
	mg.Deps(Desktop.App)
	appPath := filepath.Join(desktopOutDir, "oap.app")
	fmt.Printf("\ndesktop:all: complete -> %s\n", appPath)
	fmt.Printf("desktop:all: launch with:  open %s   (first time: right-click -> Open, ad-hoc signed)\n", appPath)
	return nil
}

// Icons regenerates the menu-bar status icon PNG frames
// (cmd/oap/internal/desktop/menubaricons/assets) from the vector geometry in
// the render subpackage. Run after changing icon geometry or motion; commit
// the regenerated PNGs. The menubaricons/assets golden test fails CI if the
// committed assets drift from the generator.
func (Desktop) Icons() error {
	return sh.RunV("go", "run", "./cmd/oap/internal/desktop/menubaricons/gen",
		"-out", "cmd/oap/internal/desktop/menubaricons/assets")
}

// ---------------------------------------------------------------------------
// Shared paths, versions, and image lists
// ---------------------------------------------------------------------------

const (
	desktopDir      = "build/desktop"
	desktopCacheDir = desktopDir + "/cache" // downloaded upstream artifacts, reused across runs
	desktopOutDir   = desktopDir + "/out"   // build outputs: rootfs.img, images tarball, oap.app

	// desktopPlatform is the ONLY platform this bundle ever targets: Apple
	// Silicon's Virtualization.framework boots an arm64 guest, so every
	// container image and every downloaded upstream artifact below is arm64.
	desktopPlatform = "linux/arm64"

	// hostShareVirtiofsTag MUST match desktopState's hostShareTag constant in
	// cmd/oap/internal/desktopcmd/run_darwin.go (the vz.VMConfig.HostShareTag the running VM
	// is booted with) — it is the fstab virtiofs source tag baked into the
	// guest here, matched at boot time by the host-side VM config there. Keep
	// the two in sync by hand: there's no shared Go constant because this
	// file and desktop_darwin.go compile under different build tags (mage vs
	// darwin&&arm64) and mage targets never link against cmd/oap.
	hostShareVirtiofsTag = "apmem"

	// hostShareMountPoint is the guest-side mountpoint for the virtiofs host
	// share, matched at boot time by the vz.VMConfig.HostShareDir the host
	// mounts into the guest at this path. Backs Phase-4 persistent memory
	// (SQLite under /var/lib/ap) — kubeconfig delivery no longer goes
	// through this share; see desktopSSHKeyName's doc comment.
	hostShareMountPoint = "/var/lib/ap"

	// ubuntuRelease/ubuntuCloudImageURL pin the base rootfs. "noble" (24.04
	// LTS) has long support and a maintained arm64 cloud image.
	ubuntuRelease        = "noble"
	ubuntuCloudImageName = "noble-server-cloudimg-arm64.img"
	ubuntuCloudImageURL  = "https://cloud-images.ubuntu.com/" + ubuntuRelease + "/current/" + ubuntuCloudImageName

	// k3sVersion pins the exact k3s release the bundle ships (both the
	// server binary and its matching air-gap system-image set — these two
	// MUST come from the same release; k3s's image manifest is
	// version-specific). Bump deliberately; re-run desktop:rootfs after
	// (delete the stale cached artifacts under build/desktop/cache/ first,
	// or they'll be reused — see desktopDownload's doc comment).
	k3sVersion              = "v1.30.6+k3s1"
	k3sReleaseBaseURL       = "https://github.com/k3s-io/k3s/releases/download/" + k3sVersion
	k3sBinaryURL            = k3sReleaseBaseURL + "/k3s-arm64"
	k3sAirgapImagesURL      = k3sReleaseBaseURL + "/k3s-airgap-images-arm64.tar.zst"
	k3sAirgapImagesFileName = "k3s-airgap-images-arm64.tar.zst"

	// rootfsDiskSizeDefault is the raw disk image's resized capacity when
	// OAP_DESKTOP_DISK_SIZE is unset. The image is sparse, so this is a
	// ceiling, not an upfront allocation: only what the guest actually
	// writes (base OS + baked images + pulled workload images + k3s state)
	// consumes host space. 10G proved too tight once a user layered custom
	// agent images (e.g. a sandbox + MCP sidecar) on top of the full memory
	// profile (neo4j + graphiti + postgres) — the guest hit disk-pressure
	// and evicted control-plane pods. 20G leaves comfortable headroom.
	// Override per build with OAP_DESKTOP_DISK_SIZE (the deprecated
	// AP_DESKTOP_DISK_SIZE name still works; e.g. "30G"); see
	// rootfsDiskSize().
	rootfsDiskSizeDefault = "20G"

	// desktopSSHKeyName is the basename of the ed25519 keypair
	// desktopGenerateSSHKeypair generates fresh on every Desktop.Rootfs run:
	// the PRIVATE half lands at build/desktop/out/ap-vm-key (Desktop.App
	// stages it into Contents/Resources/ap-vm-key, mode 0600) and the PUBLIC
	// half (ap-vm-key.pub) is baked into the guest rootfs's
	// /root/.ssh/authorized_keys — see desktopBuildRootfsInDocker and
	// build/desktop/rootfs-customize.sh. This replaces the old
	// virtiofs-share kubeconfig handoff: the host now reads
	// /etc/rancher/k3s/k3s.yaml over SSH as root using this key (see
	// cmd/oap/internal/desktop/vz/provider_darwin.go's Kubeconfig).
	//
	// A single per-build key baked into every install of this build is
	// appropriate for what this is — a loopback-only NAT dev VM with no
	// listener reachable outside the host — but is intentionally NOT a
	// per-installation runtime-generated key. That hardening (mint a fresh
	// keypair per `oap desktop` install rather than per rootfs build) is a
	// follow-up.
	desktopSSHKeyName = "ap-vm-key"

	// desktopRootfsBuilderImageTag is the local (never pushed) tag for the
	// container image desktopBuildRootfsInDocker builds from
	// build/desktop/rootfs-builder.Dockerfile and runs to do the
	// convert+customize step. Rebuilt (cheaply — Docker layer-caches the
	// apt-get install) on every Desktop.Rootfs run so a rootfs-customize.sh
	// edit always takes effect.
	desktopRootfsBuilderImageTag = "oap-desktop-rootfs-builder:latest"
)

// The first-party image set the desktop bake compiles is computed at Images()
// call time from apimage.DesktopBakeImages(devMode): the always-on control
// plane + slim base sandbox, plus (only when OAP_DESKTOP_DEV=1) the toolchain
// overlays. AlwaysOnImages is the single source of truth for the images a
// running install needs regardless of opt-in features — so this bake can never
// silently omit one (it once omitted Sandbox, which left every agent chat hung
// with its sandbox pod stuck in ErrImagePull). The prompt-injection detector is
// excluded (opt-in). Toolchain overlays are baked only for local development
// because their `:dev` tags exist on no registry; the regular app pulls them
// from a registry, so a plain `mage desktop:app` leaves them out.

// desktopDependencyImages is the minimal profile's public dependency image
// set: the spicedb-operator + the SpiceDBCluster it reconciles (authorization
// — even the desktop profile's SpiceDB is operator-managed, not a hand-rolled
// Deployment) and NATS (the signal bus), the images the base install cannot
// run without. The remaining entries in apimage.DependencyImages (pgvector,
// neo4j, graphiti, the RWX workspace provisioner, busybox, the snapshot
// helper) back OPTIONAL features (postgres/graphiti memory backends, RWX
// workspace snapshotting) the desktop bundle's SQLite-backed, single-node
// profile doesn't enable — see cmd/oap/internal/desktop.FullProfileImages for
// the analogous on-demand opt-in list at the k3s-image-import layer.
var desktopDependencyImages = []string{
	"ghcr.io/authzed/spicedb:v1.56.2",
	"ghcr.io/authzed/spicedb-operator:v1.27.0",
	"nats:2.10-alpine",
}

// ---------------------------------------------------------------------------
// Desktop:Images — the air-gapped image tarball
// ---------------------------------------------------------------------------

// Images builds the minimal-profile first-party images for linux/arm64,
// pulls the matching public dependency images, and docker-saves all of them
// into one zstd-compressed tarball at
// build/desktop/out/images-minimal.tar.zst — tagged EXACTLY as the
// manifests/operator reference them (":dev" for the first-party images, the
// upstream tag verbatim for spicedb/nats), so k3s's containerd never
// attempts an egress pull for them (imagePullPolicy: IfNotPresent resolves
// locally). Desktop.Rootfs copies this tarball into the guest's
// /var/lib/rancher/k3s/agent/images/, where k3s auto-imports every archive
// it finds there at boot.
//
// Requires: docker (with buildx + a builder that supports --load), zstd.
func (Desktop) Images() error {
	if err := desktopRequireTools("docker", "zstd"); err != nil {
		return err
	}
	if err := os.MkdirAll(desktopOutDir, 0o755); err != nil {
		return fmt.Errorf("desktop:images: create %s: %w", desktopOutDir, err)
	}

	devModeVal, usedDeprecatedDevMode := envfallback.Get("OAP_DESKTOP_DEV", "AP_DESKTOP_DEV")
	if usedDeprecatedDevMode {
		fmt.Println("==> desktop:images: AP_DESKTOP_DEV is deprecated; use OAP_DESKTOP_DEV")
	}
	devMode := strings.TrimSpace(devModeVal) == "1"
	firstParty := apimage.DesktopBakeImages(devMode)
	if devMode {
		fmt.Println("==> desktop:images: OAP_DESKTOP_DEV=1 — baking toolchain overlays (go, node, claude) for offline local dev")
	}

	refs := make([]string, 0, len(firstParty)+len(desktopDependencyImages))

	// The first-party images that are thin final stages over one shared Go
	// compile (apimage.UsesGoBuilder) get that compile materialized once,
	// first, as its own step. Unlike cmd/oap's build fan-out (which runs jobs
	// concurrently and so needs the prebuild to avoid several simultaneous
	// cold-cache compiles racing for the same builder vertex), this loop is
	// sequential: without the prebuild, the FIRST Go-service build still
	// produces the builder vertex once, and every later one in this loop hits
	// it as an ordinary layer-cache hit — none of them recompile from cold.
	// The prebuild's value here is visibility, not correctness: it surfaces
	// the multi-minute compile as its own named step instead of burying it
	// inside whichever service happens to build first.
	goSvcs := 0
	for _, im := range firstParty {
		if apimage.UsesGoBuilder(im.Dockerfile, im.Stage) {
			goSvcs++
		}
	}
	if goSvcs >= apimage.GoBuilderPrebuildMin {
		b := apimage.GoBuilder
		fmt.Printf("==> desktop:images: docker buildx build --target %s %s (shared Go compile for %d images)\n",
			b.Stage, b.Dockerfile, goSvcs)
		// No -t and no --load: --output=type=cacheonly tells BuildKit to skip
		// materializing an image entirely, so this only warms BuildKit's cache
		// instead of leaving a dangling image in the daemon's store (the
		// default "docker" driver otherwise still lands an untagged --target
		// build there).
		if err := sh.RunV("docker", "buildx", "build",
			"--platform", desktopPlatform,
			"-f", b.Dockerfile,
			"--target", b.Stage,
			"--output=type=cacheonly",
			b.Context,
		); err != nil {
			return fmt.Errorf("desktop:images: prebuild shared Go builder: %w", err)
		}
	}

	for _, im := range firstParty {
		ref := im.LocalRef()
		fmt.Printf("==> desktop:images: docker buildx build %s (%s, dockerfile=%q, stage=%q, context=%q)\n",
			ref, desktopPlatform, im.Dockerfile, im.Stage, im.Context)
		args := []string{"buildx", "build", "--platform", desktopPlatform, "--load", "-t", ref}
		if im.Dockerfile != "" {
			args = append(args, "-f", im.Dockerfile)
		}
		if im.Stage != "" {
			args = append(args, "--target", im.Stage)
		}
		args = append(args, im.Context)
		if err := sh.RunV("docker", args...); err != nil {
			return fmt.Errorf("desktop:images: build %s: %w", ref, err)
		}
		refs = append(refs, ref)
	}

	for _, ref := range desktopDependencyImages {
		fmt.Printf("==> desktop:images: docker pull %s (%s)\n", ref, desktopPlatform)
		if err := sh.RunV("docker", "pull", "--platform", desktopPlatform, ref); err != nil {
			return fmt.Errorf("desktop:images: pull %s: %w", ref, err)
		}
		refs = append(refs, ref)
	}

	tarPath := filepath.Join(desktopOutDir, "images-minimal.tar")
	fmt.Printf("==> desktop:images: docker save %d image(s) -> %s\n", len(refs), tarPath)
	saveArgs := append([]string{"save", "-o", tarPath}, refs...)
	if err := sh.RunV("docker", saveArgs...); err != nil {
		return fmt.Errorf("desktop:images: docker save: %w", err)
	}
	// The .tar is a large intermediate; only the compressed .tar.zst ships.
	// Best-effort cleanup — a failure here doesn't invalidate the tarball
	// that matters, but IS logged (no silent errors) so leftover multi-GB
	// files under build/desktop/out/ aren't a mystery later.
	defer func() {
		if err := os.Remove(tarPath); err != nil && !os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "desktop:images: warning: could not remove intermediate %s: %v\n", tarPath, err)
		}
	}()

	zstPath := tarPath + ".zst"
	// -T0 uses every core: single-threaded -19 on the ~1 GiB image tarball was
	// the slowest step of the build. The level is env-tunable
	// (OAP_DESKTOP_ZSTD_LEVEL, default 19; the deprecated AP_DESKTOP_ZSTD_LEVEL
	// name still works) to trade ratio for speed on a slow machine — e.g.
	// OAP_DESKTOP_ZSTD_LEVEL=12 is far faster and only modestly larger. -T0
	// output is a standard zstd stream k3s imports unchanged.
	level := "19"
	if v, usedDeprecated := envfallback.Get("OAP_DESKTOP_ZSTD_LEVEL", "AP_DESKTOP_ZSTD_LEVEL"); strings.TrimSpace(v) != "" {
		if usedDeprecated {
			fmt.Println("==> desktop:images: AP_DESKTOP_ZSTD_LEVEL is deprecated; use OAP_DESKTOP_ZSTD_LEVEL")
		}
		level = strings.TrimSpace(v)
	}
	fmt.Printf("==> desktop:images: zstd -%s -T0 %s -> %s\n", level, tarPath, zstPath)
	if err := sh.RunV("zstd", "-"+level, "-T0", "-f", tarPath, "-o", zstPath); err != nil {
		return fmt.Errorf("desktop:images: zstd compress: %w", err)
	}

	fmt.Printf("desktop:images: done -> %s\n", zstPath)
	return nil
}

// ---------------------------------------------------------------------------
// Desktop:Rootfs — the offline k3s guest disk image
// ---------------------------------------------------------------------------

// Rootfs builds the offline single-node k3s guest disk image at
// build/desktop/out/rootfs.img: an Ubuntu arm64 cloud image resized to
// rootfsDiskSize, customized WITHOUT booting it (inside Docker — see
// desktopBuildRootfsInDocker) with:
//
//   - the k3s server binary + build/desktop/k3s.service (enabled) +
//     build/desktop/k3s-config.yaml (offline single-node defaults)
//   - k3s's own air-gap system-image set (coredns, metrics-server, etc.)
//     AND the Desktop.Images minimal-profile app image tarball, both dropped
//     into /var/lib/rancher/k3s/agent/images/ where k3s auto-imports any
//     archive it finds there at boot — so first boot needs zero egress
//   - the internal/cmd/apguest guest agent + build/desktop/apguest.service (enabled),
//     which reports the guest's NAT IP to the host over vsock (see
//     cmd/oap/internal/desktop/vz/provider_darwin.go's package doc)
//   - sshd enabled (build/desktop/ssh-hostkeys.service generates host keys
//     unconditionally at boot, sidestepping Ubuntu's cloud-init-gated
//     default; build/desktop/sshd-ap.conf pins root/key-only login) with a
//     freshly generated ed25519 PUBLIC key baked into
//     /root/.ssh/authorized_keys — see desktopGenerateSSHKeypair and
//     desktopSSHKeyName. The host fetches the k3s kubeconfig over this SSH
//     link (cmd/oap/internal/desktop/vz/provider_darwin.go's Kubeconfig);
//     the PRIVATE half never enters the guest, only build/desktop/out/ and
//     (via Desktop.App) the .app's Contents/Resources.
//   - the /var/lib/ap virtiofs fstab entry (tag hostShareVirtiofsTag) the
//     vz.Provider's HostShareDir mounts persistent memory through
//   - guest DHCP networking (build/desktop/10-ap-dhcp.network, matching
//     en*/eth*) so systemd-networkd brings up the virtio NIC and gets an IP
//     with NO cloud-init/netplan involved — the stock Ubuntu cloud image
//     ships neither, so without this the NIC never DHCPs and
//     cmd/oap/internal/desktop/vz/provider_darwin.go's GuestIP always times
//     out; build/desktop/wait-online-timeout.conf bounds
//     systemd-networkd-wait-online.service (already enabled) to --any
//     --timeout=30 so a DHCP hiccup delays boot instead of hanging it
//     forever now that it has an interface to actually wait for
//
// The EFI bootloader and kernel/initrd stay exactly as Ubuntu's cloud image
// ships them (no kernel extraction) — vz.Provider boots via
// vz.NewEFIBootLoader against this disk's own ESP, per provider_darwin.go.
//
// Requires: docker, curl, go (to cross-compile internal/cmd/apguest for linux/arm64),
// ssh-keygen (part of the base macOS toolchain — generates the keypair
// above). Depends on Desktop.Images. See desktopBuildRootfsInDocker for why
// the convert+customize step (formerly qemu-img + virt-customize run
// directly on the host) now happens inside a container instead.
// rootfsDiskSize returns the DISK_SIZE handed to rootfs-customize.sh:
// OAP_DESKTOP_DISK_SIZE when set (e.g. "30G", the deprecated
// AP_DESKTOP_DISK_SIZE name still works), else rootfsDiskSizeDefault.
// The value is passed verbatim to `qemu-img resize`, so it must be a size
// qemu accepts (e.g. "20G", "20480M").
func rootfsDiskSize() string {
	if v, usedDeprecated := envfallback.Get("OAP_DESKTOP_DISK_SIZE", "AP_DESKTOP_DISK_SIZE"); strings.TrimSpace(v) != "" {
		if usedDeprecated {
			fmt.Println("==> desktop: AP_DESKTOP_DISK_SIZE is deprecated; use OAP_DESKTOP_DISK_SIZE")
		}
		return strings.TrimSpace(v)
	}
	return rootfsDiskSizeDefault
}

func (Desktop) Rootfs() error {
	mg.Deps(Desktop.Images)

	if err := desktopRequireTools("docker", "curl", "go", "ssh-keygen"); err != nil {
		return err
	}
	if err := os.MkdirAll(desktopCacheDir, 0o755); err != nil {
		return fmt.Errorf("desktop:rootfs: create %s: %w", desktopCacheDir, err)
	}
	if err := os.MkdirAll(desktopOutDir, 0o755); err != nil {
		return fmt.Errorf("desktop:rootfs: create %s: %w", desktopOutDir, err)
	}

	// --- Step 1: fetch upstream artifacts (cached; see desktopDownload) ---

	cloudImgPath := filepath.Join(desktopCacheDir, ubuntuCloudImageName)
	if err := desktopDownload(ubuntuCloudImageURL, cloudImgPath); err != nil {
		return fmt.Errorf("desktop:rootfs: download Ubuntu cloud image: %w", err)
	}

	k3sBinCachePath := filepath.Join(desktopCacheDir, "k3s-arm64-"+k3sVersion)
	if err := desktopDownload(k3sBinaryURL, k3sBinCachePath); err != nil {
		return fmt.Errorf("desktop:rootfs: download k3s binary: %w", err)
	}
	// Staged (renamed to the bare "k3s" the systemd unit + config.yaml
	// expect) into desktopOutDir rather than left under the version-suffixed
	// cache name, so the virt-customize --copy-in below lands it at
	// /usr/local/bin/k3s (copy-in preserves the LOCAL basename).
	k3sBinStagedPath := filepath.Join(desktopOutDir, "k3s")
	if err := desktopCopyFile(k3sBinCachePath, k3sBinStagedPath); err != nil {
		return fmt.Errorf("desktop:rootfs: stage k3s binary: %w", err)
	}
	if err := os.Chmod(k3sBinStagedPath, 0o755); err != nil {
		return fmt.Errorf("desktop:rootfs: chmod k3s binary: %w", err)
	}

	k3sImagesPath := filepath.Join(desktopCacheDir, k3sAirgapImagesFileName)
	if err := desktopDownload(k3sAirgapImagesURL, k3sImagesPath); err != nil {
		return fmt.Errorf("desktop:rootfs: download k3s air-gap images: %w", err)
	}

	// --- Step 2: cross-build the guest binaries ---

	fmt.Println("==> desktop:rootfs: go build ./internal/cmd/apguest (linux/arm64)")
	apguestPath := filepath.Join(desktopOutDir, "apguest")
	if err := sh.RunWithV(map[string]string{"GOOS": "linux", "GOARCH": "arm64", "CGO_ENABLED": "0"},
		"go", "build", "-o", apguestPath, "./internal/cmd/apguest"); err != nil {
		return fmt.Errorf("desktop:rootfs: build apguest: %w", err)
	}
	if err := os.Chmod(apguestPath, 0o755); err != nil {
		return fmt.Errorf("desktop:rootfs: chmod apguest: %w", err)
	}

	imagesTarballPath := filepath.Join(desktopOutDir, "images-minimal.tar.zst")
	if _, err := os.Stat(imagesTarballPath); err != nil {
		return fmt.Errorf("desktop:rootfs: %s missing (desktop:images should have produced it): %w", imagesTarballPath, err)
	}

	// --- Step 2b: generate the SSH keypair baked into this build ---

	fmt.Println("==> desktop:rootfs: ssh-keygen -t ed25519 (fresh guest access key)")
	sshKeyPath := filepath.Join(desktopOutDir, desktopSSHKeyName)
	authorizedKey, err := desktopGenerateSSHKeypair(sshKeyPath)
	if err != nil {
		return fmt.Errorf("desktop:rootfs: generate SSH keypair: %w", err)
	}

	// --- Steps 3-4: convert + resize + customize, all inside Docker ---
	//
	// This used to shell out to host qemu-img (convert/resize) and then
	// virt-customize/libguestfs (the --mkdir/--copy-in/--chmod/--run-command
	// list previously here) to do the actual customization. libguestfs has no
	// working native macOS build, so both steps now happen inside a small
	// Debian container instead — see desktopBuildRootfsInDocker.

	rootfsPath := filepath.Join(desktopOutDir, "rootfs.img")
	fstabLine := fmt.Sprintf("%s %s virtiofs defaults 0 0", hostShareVirtiofsTag, hostShareMountPoint)
	if err := desktopBuildRootfsInDocker(cloudImgPath, k3sImagesPath, fstabLine, authorizedKey); err != nil {
		return fmt.Errorf("desktop:rootfs: %w", err)
	}

	fmt.Printf("desktop:rootfs: done -> %s\n", rootfsPath)
	return nil
}

// desktopGenerateSSHKeypair generates a fresh, unencrypted ed25519 keypair
// at privPath (public half at privPath+".pub") via the system `ssh-keygen`,
// overwriting any previous keypair from an earlier build — see
// desktopSSHKeyName's doc comment for why this bundle intentionally mints a
// new key on every Desktop.Rootfs run rather than persisting one across
// builds. Returns the single-line public key (as ssh-keygen writes it:
// "<type> <base64> <comment>"), ready to drop verbatim into the guest's
// authorized_keys.
func desktopGenerateSSHKeypair(privPath string) (string, error) {
	pubPath := privPath + ".pub"
	for _, p := range []string{privPath, pubPath} {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return "", fmt.Errorf("remove stale %s: %w", p, err)
		}
	}
	if err := sh.RunV("ssh-keygen", "-t", "ed25519", "-N", "", "-C", "oap-desktop-vm", "-f", privPath); err != nil {
		return "", fmt.Errorf("ssh-keygen: %w", err)
	}
	if err := os.Chmod(privPath, 0o600); err != nil {
		return "", fmt.Errorf("chmod private key: %w", err)
	}
	pub, err := os.ReadFile(pubPath)
	if err != nil {
		return "", fmt.Errorf("read generated public key: %w", err)
	}
	return strings.TrimSpace(string(pub)), nil
}

// desktopBuildRootfsInDocker does the qemu-img convert/resize + offline
// customization Desktop.Rootfs used to do via host qemu-img + virt-customize
// (libguestfs). Both now run inside a container instead: libguestfs has no
// working native macOS build (virt-customize's supermin appliance needs a
// real or nested-capable hypervisor, which Docker Desktop for Mac's
// nested-virtualization story doesn't reliably provide), whereas qemu-img
// plus a plain loop-mounted ext4 partition need nothing but --privileged and
// a live /dev — both of which a container gets.
//
// The container image (build/desktop/rootfs-builder.Dockerfile) carries
// qemu-img + losetup/mount/e2fsprogs; the actual work
// (convert -> resize -> losetup -P -> mount -> copy files in -> enable
// services -> append fstab -> unmount) is build/desktop/rootfs-customize.sh,
// run as that image's ENTRYPOINT. Cross-referencing the two doc comments
// gives the full picture of what lands in the guest and why.
//
// cloudImgPath and k3sImagesPath are absolute host paths under
// build/desktop/cache/ (see desktopDownload); everything else the script
// needs — the staged k3s + apguest binaries, images-minimal.tar.zst, the
// static units/config/script under build/desktop/ — is found by bind-
// mounting desktopOutDir and desktopDir wholesale, so this function doesn't
// need to know their individual filenames. authorizedKey is the single-line
// public half of desktopGenerateSSHKeypair's freshly generated keypair,
// baked verbatim into the guest's /root/.ssh/authorized_keys.
func desktopBuildRootfsInDocker(cloudImgPath, k3sImagesPath, fstabLine, authorizedKey string) error {
	absCloudImg, err := filepath.Abs(cloudImgPath)
	if err != nil {
		return fmt.Errorf("resolve absolute path for %s: %w", cloudImgPath, err)
	}
	absK3sImages, err := filepath.Abs(k3sImagesPath)
	if err != nil {
		return fmt.Errorf("resolve absolute path for %s: %w", k3sImagesPath, err)
	}
	absStaticDir, err := filepath.Abs(desktopDir)
	if err != nil {
		return fmt.Errorf("resolve absolute path for %s: %w", desktopDir, err)
	}
	absOutDir, err := filepath.Abs(desktopOutDir)
	if err != nil {
		return fmt.Errorf("resolve absolute path for %s: %w", desktopOutDir, err)
	}

	dockerfilePath := filepath.Join(desktopDir, "rootfs-builder.Dockerfile")
	fmt.Printf("==> desktop:rootfs: docker build -f %s %s\n", dockerfilePath, desktopDir)
	if err := sh.RunV("docker", "build", "--platform", desktopPlatform,
		"-t", desktopRootfsBuilderImageTag, "-f", dockerfilePath, desktopDir); err != nil {
		return fmt.Errorf("docker build rootfs-builder image: %w", err)
	}

	fmt.Println("==> desktop:rootfs: docker run rootfs-customize.sh (convert + resize + customize, offline)")
	if err := sh.RunWithV(map[string]string{
		"DISK_SIZE":          rootfsDiskSize(),
		"FSTAB_LINE":         fstabLine,
		"SSH_AUTHORIZED_KEY": authorizedKey,
	}, "docker", "run", "--rm",
		"--privileged", "--platform", desktopPlatform,
		// Bind-mounting the HOST's real /dev (itself devtmpfs), not the
		// container's private one, is required for the loop-partition device
		// nodes losetup -P creates to actually show up — see
		// rootfs-customize.sh's doc comment for the failure mode this avoids.
		"-v", "/dev:/dev",
		"-v", absCloudImg+":/work/cloudimg.img:ro",
		"-v", absK3sImages+":/work/k3s-airgap-images.tar.zst:ro",
		"-v", absStaticDir+":/work/static:ro",
		"-v", absOutDir+":/work/out",
		"-e", "DISK_SIZE",
		"-e", "FSTAB_LINE",
		"-e", "SSH_AUTHORIZED_KEY",
		desktopRootfsBuilderImageTag,
	); err != nil {
		return fmt.Errorf("docker run rootfs-builder: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Desktop:App — .app assembly + ad-hoc dev signing
// ---------------------------------------------------------------------------

// Devapp builds the LOCAL-DEVELOPMENT desktop app: identical to App, but the
// image bake also includes the language/agent toolchain overlays (go, node,
// claude) whose `:dev` tags exist on no registry. Use this to run codebot (or
// any toolchain-using agent) on an offline desktop; the plain App target omits
// them, targeting the registry-hosted-toolchain future. It sets OAP_DESKTOP_DEV=1
// before the App chain so Rootfs -> Images inherit it.
func (Desktop) Devapp() error {
	os.Setenv("OAP_DESKTOP_DEV", "1")
	mg.Deps(Desktop.App)
	return nil
}

// App assembles build/desktop/out/oap.app:
//
//	Contents/
//	  Info.plist               (build/desktop/Info.plist, copied verbatim)
//	  MacOS/oap                (this repo's cmd/oap, built darwin/arm64+cgo —
//	                             the bundle's ONLY executable; see
//	                             cmd/oap/main_bundle_launch.go for how a
//	                             Finder double-click becomes `oap desktop`)
//	  Resources/rootfs.img     (build/desktop/out/rootfs.img, from Desktop.Rootfs)
//	  Resources/ap-vm-key      (build/desktop/out/ap-vm-key, mode 0600 — the
//	                             PRIVATE half of the keypair Desktop.Rootfs
//	                             generated and baked the public half of into
//	                             the guest; see desktopSSHKeyName)
//
// then ad-hoc signs it for local development (NOT for distribution — see the
// TODO on notarization in the report this target's task produced).
//
// Deliberately NOT bundled into Resources, despite earlier drafts of this
// bundle's design listing them: an EFI variable store seed (vz.Provider's
// Provision() — cmd/oap/internal/desktop/vz/provider_darwin.go — creates a
// fresh one automatically the first time EFIStorePath doesn't exist; no code
// path ever reads a seed from Resources) and a second copy of
// images-minimal.tar.zst (Desktop.Rootfs already bakes it INTO rootfs.img;
// shipping it again in Resources would ~double the bundle's size for a file
// ensureDiskImage in cmd/oap/internal/desktopcmd/run_darwin.go never reads).
//
// Requires: go (darwin/arm64 cgo build), codesign. Depends on Desktop.Rootfs.
// Only runs on darwin — cross-compiling a cgo darwin/arm64 binary that links
// Cocoa/Virtualization.framework headers needs a real macOS + Xcode SDK.
func (Desktop) App() error {
	mg.Deps(Desktop.Rootfs)

	if runtime.GOOS != "darwin" {
		return fmt.Errorf("desktop:app: must run on macOS (builds a cgo darwin/arm64 binary and ad-hoc signs a .app bundle); this host is %s", runtime.GOOS)
	}
	if err := desktopRequireTools("codesign"); err != nil {
		return err
	}

	appPath := filepath.Join(desktopOutDir, "oap.app")
	contentsPath := filepath.Join(appPath, "Contents")
	macOSPath := filepath.Join(contentsPath, "MacOS")
	resourcesPath := filepath.Join(contentsPath, "Resources")
	for _, dir := range []string{macOSPath, resourcesPath} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("desktop:app: create %s: %w", dir, err)
		}
	}

	apBinPath := filepath.Join(macOSPath, "oap")
	fmt.Printf("==> desktop:app: go build -o %s ./cmd/oap (darwin/arm64, cgo)\n", apBinPath)
	if err := sh.RunWithV(map[string]string{"GOOS": "darwin", "GOARCH": "arm64", "CGO_ENABLED": "1"},
		"go", "build", "-o", apBinPath, "./cmd/oap"); err != nil {
		return fmt.Errorf("desktop:app: build oap: %w", err)
	}

	fmt.Println("==> desktop:app: stage Contents/Resources/rootfs.img")
	if err := desktopCopyFile(filepath.Join(desktopOutDir, "rootfs.img"), filepath.Join(resourcesPath, "rootfs.img")); err != nil {
		return fmt.Errorf("desktop:app: stage rootfs.img: %w", err)
	}

	fmt.Println("==> desktop:app: stage Contents/Resources/ap-vm-key")
	stagedKeyPath := filepath.Join(resourcesPath, desktopSSHKeyName)
	if err := desktopCopyFile(filepath.Join(desktopOutDir, desktopSSHKeyName), stagedKeyPath); err != nil {
		return fmt.Errorf("desktop:app: stage %s: %w", desktopSSHKeyName, err)
	}
	// desktopCopyFile creates its destination with 0o644 (fine for
	// rootfs.img); tighten an SSH private key to 0o600 — golang.org/x/crypto/ssh
	// doesn't itself check file permissions, but leaving a private key
	// world/group-readable on a multi-user machine is exactly the kind of
	// avoidable exposure OpenSSH's own tooling refuses to use a key over.
	if err := os.Chmod(stagedKeyPath, 0o600); err != nil {
		return fmt.Errorf("desktop:app: chmod %s: %w", desktopSSHKeyName, err)
	}

	fmt.Println("==> desktop:app: write Contents/Info.plist")
	if err := desktopCopyFile(filepath.Join(desktopDir, "Info.plist"), filepath.Join(contentsPath, "Info.plist")); err != nil {
		return fmt.Errorf("desktop:app: stage Info.plist: %w", err)
	}

	// Contents/MacOS/oap IS the bundle's main executable, so signing the .app
	// re-signs it — the entitlements MUST be passed on the BUNDLE sign or they
	// get stripped. Sign the bundle with --entitlements in one step: this seals
	// the resources AND applies the virtualization entitlement to the main
	// executable. (A separate binary sign first would be undone by this step.)
	entitlementsPath := filepath.Join(desktopDir, "vm.entitlements")
	fmt.Println("==> desktop:app: codesign oap.app (ad-hoc, virtualization entitlement)")
	if err := sh.RunV("codesign", "--force", "--options", "runtime",
		"--entitlements", entitlementsPath, "-s", "-", appPath); err != nil {
		return fmt.Errorf("desktop:app: codesign oap.app: %w", err)
	}
	fmt.Println("==> desktop:app: codesign --verify --strict")
	if err := sh.RunV("codesign", "--verify", "--strict", "--verbose=2", appPath); err != nil {
		return fmt.Errorf("desktop:app: codesign verify: %w", err)
	}

	fmt.Printf("desktop:app: done -> %s\n", appPath)
	fmt.Println("desktop:app: this is an AD-HOC signature (-s -), for local development only.")
	fmt.Println("desktop:app: first launch needs right-click -> Open (Gatekeeper) since it isn't notarized.")
	return nil
}

// ---------------------------------------------------------------------------
// Shared helpers
// ---------------------------------------------------------------------------

// desktopRequireTools fails closed, with one actionable error naming every
// missing tool and where to get it, rather than letting the first sh.RunV
// call fail deep into a target with a bare "executable file not found in
// $PATH".
func desktopRequireTools(names ...string) error {
	var missing []string
	for _, n := range names {
		if _, err := exec.LookPath(n); err != nil {
			missing = append(missing, n)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	return fmt.Errorf(
		"desktop: missing required tool(s) on PATH: %s — install via Homebrew, e.g. "+
			"`brew install --cask docker` (docker+buildx; also runs the desktop:rootfs "+
			"convert+customize step — no host qemu-img/libguestfs needed), "+
			"`brew install zstd curl`; codesign ships with Xcode Command Line Tools "+
			"(`xcode-select --install`)",
		strings.Join(missing, ", "))
}

// desktopDownload fetches url to dest if dest doesn't already exist under
// build/desktop/cache/ — that directory survives across mage invocations, so
// re-running desktop:rootfs after a small code change doesn't re-download a
// ~600MB cloud image + k3s release every time. Delete the cached file (or
// bump the version constants above, which changes the cache filename) to
// force a re-download.
func desktopDownload(url, dest string) error {
	if _, err := os.Stat(dest); err == nil {
		fmt.Printf("==> desktop: %s already cached, skipping download\n", dest)
		return nil
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stat %s: %w", dest, err)
	}
	fmt.Printf("==> desktop: downloading %s -> %s\n", url, dest)
	tmp := dest + ".tmp"
	if err := sh.RunV("curl", "-fL", "--progress-bar", "-o", tmp, url); err != nil {
		if rmErr := os.Remove(tmp); rmErr != nil && !os.IsNotExist(rmErr) {
			fmt.Fprintf(os.Stderr, "desktop: warning: could not remove partial download %s: %v\n", tmp, rmErr)
		}
		return fmt.Errorf("curl %s: %w", url, err)
	}
	if err := os.Rename(tmp, dest); err != nil {
		return fmt.Errorf("rename %s -> %s: %w", tmp, dest, err)
	}
	return nil
}

// desktopCopyFile copies src to dst (truncating dst if it already exists).
// Used to stage/rename files ahead of virt-customize --copy-in (which
// preserves the LOCAL basename) and to assemble oap.app/Contents.
func desktopCopyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open %s: %w", src, err)
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("create %s: %w", dst, err)
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return fmt.Errorf("copy %s -> %s: %w", src, dst, err)
	}
	return out.Close()
}
