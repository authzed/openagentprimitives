//go:build darwin && arm64

package vz

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/desktop"
)

// generateTestSSHKeypair returns a ready-to-use ssh.Signer plus the
// corresponding PEM-encoded OpenSSH private key bytes (for loadSSHSigner /
// staging into a temp key file, exactly as Desktop.App stages
// build/desktop/out/ap-vm-key into the .app bundle).
func generateTestSSHKeypair(t *testing.T) (ssh.Signer, []byte) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	block, err := ssh.MarshalPrivateKey(priv, "vz-test")
	require.NoError(t, err)
	signer, err := ssh.NewSignerFromKey(priv)
	require.NoError(t, err)
	return signer, pem.EncodeToMemory(block)
}

// writeTestKeyFile stages privPEM under a fresh t.TempDir(), mode 0600
// (matching Desktop.App's staged Contents/Resources/ap-vm-key), and
// returns its path.
func writeTestKeyFile(t *testing.T, privPEM []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ap-vm-key")
	require.NoError(t, os.WriteFile(path, privPEM, 0o600))
	return path
}

// acceptOnlyKey builds a PublicKeyCallback that authenticates exactly one
// key (any username) — a stand-in for the guest's baked
// /root/.ssh/authorized_keys containing exactly the one key
// desktopGenerateSSHKeypair produced for this build.
func acceptOnlyKey(want ssh.PublicKey) func(ssh.ConnMetadata, ssh.PublicKey) (*ssh.Permissions, error) {
	return func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
		if bytes.Equal(key.Marshal(), want.Marshal()) {
			return nil, nil
		}
		return nil, errors.New("unauthorized key")
	}
}

// serveKubeconfigOnce completes the SSH server handshake on conn, accepts
// exactly one "session" channel, and responds to its "exec" request (the
// production client only ever runs `cat <path>` — see
// fetchKubeconfigOverSSH) by writing output and exiting with exitStatus. A
// handshake failure (e.g. the client presented an unauthorized key) is a
// normal, expected outcome for the negative-path tests below, so it's
// swallowed rather than reported via t — reporting it would race the test
// goroutine that's asserting on the CLIENT-side error instead.
func serveKubeconfigOnce(conn net.Conn, cfg *ssh.ServerConfig, output []byte, exitStatus uint32) {
	sshConn, chans, reqs, err := ssh.NewServerConn(conn, cfg)
	if err != nil {
		return
	}
	defer sshConn.Close()
	go ssh.DiscardRequests(reqs)

	for newCh := range chans {
		if newCh.ChannelType() != "session" {
			_ = newCh.Reject(ssh.UnknownChannelType, "unsupported channel type")
			continue
		}
		ch, requests, err := newCh.Accept()
		if err != nil {
			continue
		}
		go func() {
			defer ch.Close()
			for req := range requests {
				if req.Type != "exec" {
					if req.WantReply {
						_ = req.Reply(false, nil)
					}
					continue
				}
				if req.WantReply {
					_ = req.Reply(true, nil)
				}
				_, _ = ch.Write(output)
				_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{exitStatus}))
				return
			}
		}()
	}
}

// startSingleShotSSHServer starts an in-process SSH server on
// 127.0.0.1:<ephemeral>, serving exactly one connection the way
// serveKubeconfigOnce does, then closing the listener. Returns the
// address to dial and a func to force-close the listener early (unused by
// most tests; present for symmetry/cleanup safety).
func startSingleShotSSHServer(t *testing.T, srvCfg *ssh.ServerConfig, output []byte, exitStatus uint32) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		serveKubeconfigOnce(conn, srvCfg, output, exitStatus)
	}()
	return ln.Addr().String()
}

func newTestServerConfig(t *testing.T, clientKey ssh.PublicKey) *ssh.ServerConfig {
	t.Helper()
	hostSigner, _ := generateTestSSHKeypair(t)
	cfg := &ssh.ServerConfig{PublicKeyCallback: acceptOnlyKey(clientKey)}
	cfg.AddHostKey(hostSigner)
	return cfg
}

func TestLoadSSHSigner(t *testing.T) {
	clientSigner, privPEM := generateTestSSHKeypair(t)

	t.Run("valid key file parses to a signer with the matching public key", func(t *testing.T) {
		path := writeTestKeyFile(t, privPEM)
		got, err := loadSSHSigner(path)
		require.NoError(t, err)
		assert.Equal(t, clientSigner.PublicKey().Marshal(), got.PublicKey().Marshal())
	})

	t.Run("missing file returns a wrapped read error", func(t *testing.T) {
		_, err := loadSSHSigner(filepath.Join(t.TempDir(), "does-not-exist"))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "read ssh private key")
	})

	t.Run("malformed key content returns a wrapped parse error", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "garbage-key")
		require.NoError(t, os.WriteFile(path, []byte("not a key\n"), 0o600))
		_, err := loadSSHSigner(path)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "parse ssh private key")
	})
}

func TestFetchKubeconfigOverSSH(t *testing.T) {
	const wantKubeconfig = "apiVersion: v1\nkind: Config\nclusters: []\n"
	clientSigner, privPEM := generateTestSSHKeypair(t)
	keyPath := writeTestKeyFile(t, privPEM)
	signer, err := loadSSHSigner(keyPath)
	require.NoError(t, err)

	baseClientCfg := func() *ssh.ClientConfig {
		return &ssh.ClientConfig{
			User:            sshUser,
			Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
			HostKeyCallback: ssh.InsecureIgnoreHostKey(),
			Timeout:         2 * time.Second,
		}
	}

	t.Run("successful exec returns stdout", func(t *testing.T) {
		srvCfg := newTestServerConfig(t, clientSigner.PublicKey())
		addr := startSingleShotSSHServer(t, srvCfg, []byte(wantKubeconfig), 0)

		got, err := fetchKubeconfigOverSSH(context.Background(), addr, baseClientCfg(), k3sKubeconfigGuestPath, 2*time.Second)
		require.NoError(t, err)
		assert.Equal(t, wantKubeconfig, string(got))
	})

	t.Run("nonzero exit status is an error", func(t *testing.T) {
		srvCfg := newTestServerConfig(t, clientSigner.PublicKey())
		addr := startSingleShotSSHServer(t, srvCfg, []byte("cat: no such file"), 1)

		_, err := fetchKubeconfigOverSSH(context.Background(), addr, baseClientCfg(), k3sKubeconfigGuestPath, 2*time.Second)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "cat "+k3sKubeconfigGuestPath+" over ssh")
	})

	t.Run("empty stdout is an error (kubeconfig not yet written)", func(t *testing.T) {
		srvCfg := newTestServerConfig(t, clientSigner.PublicKey())
		addr := startSingleShotSSHServer(t, srvCfg, nil, 0)

		_, err := fetchKubeconfigOverSSH(context.Background(), addr, baseClientCfg(), k3sKubeconfigGuestPath, 2*time.Second)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "empty output")
	})

	t.Run("unauthorized client key fails the handshake", func(t *testing.T) {
		// The server only trusts a DIFFERENT key than the one this client
		// presents — simulates dialing a guest whose authorized_keys was
		// baked from a different build's keypair.
		otherSigner, _ := generateTestSSHKeypair(t)
		srvCfg := newTestServerConfig(t, otherSigner.PublicKey())
		addr := startSingleShotSSHServer(t, srvCfg, []byte(wantKubeconfig), 0)

		_, err := fetchKubeconfigOverSSH(context.Background(), addr, baseClientCfg(), k3sKubeconfigGuestPath, 2*time.Second)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "ssh handshake")
	})

	t.Run("nothing listening yields a dial error", func(t *testing.T) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		addr := ln.Addr().String()
		require.NoError(t, ln.Close()) // freed; nothing else races to grab it within this test

		_, err = fetchKubeconfigOverSSH(context.Background(), addr, baseClientCfg(), k3sKubeconfigGuestPath, 2*time.Second)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "dial")
	})

	t.Run("a peer that accepts TCP but never speaks SSH is bounded by attemptTimeout, not left hanging", func(t *testing.T) {
		// Models exactly the failure shape a not-yet-ready sshd can produce:
		// the TCP handshake completes (something is listening), but nothing
		// ever sends an SSH version banner. Without fetchKubeconfigOverSSH's
		// explicit conn.SetDeadline this would block forever — neither
		// ssh.NewClientConn nor cfg.Timeout bounds it (see that function's
		// doc comment).
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		t.Cleanup(func() { _ = ln.Close() })
		go func() {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			defer conn.Close()
			<-t.Context().Done() // hold the connection open past the test
		}()

		const attemptTimeout = 200 * time.Millisecond
		start := time.Now()
		_, err = fetchKubeconfigOverSSH(context.Background(), ln.Addr().String(), baseClientCfg(), k3sKubeconfigGuestPath, attemptTimeout)
		elapsed := time.Since(start)

		require.Error(t, err)
		assert.Contains(t, err.Error(), "ssh handshake")
		assert.Less(t, elapsed, 2*time.Second, "must be bounded by attemptTimeout, not hang")
	})
}

func TestPollKubeconfigOverSSH(t *testing.T) {
	const wantKubeconfig = "apiVersion: v1\nkind: Config\n"
	clientSigner, privPEM := generateTestSSHKeypair(t)
	keyPath := writeTestKeyFile(t, privPEM)
	signer, err := loadSSHSigner(keyPath)
	require.NoError(t, err)
	clientCfg := &ssh.ClientConfig{
		User:            sshUser,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         2 * time.Second,
	}

	t.Run("retries through early connections dropped before the handshake, then succeeds", func(t *testing.T) {
		// Models sshd/k3s not being ready yet on the first few poll ticks
		// (the real-world case this loop exists for): the first two TCP
		// accepts are closed immediately, before any SSH handshake — same
		// client-visible failure shape as "nothing is listening yet" one
		// beat earlier — and only the third connection actually serves.
		srvCfg := newTestServerConfig(t, clientSigner.PublicKey())
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		t.Cleanup(func() { _ = ln.Close() })

		var attempts int32
		go func() {
			for {
				conn, err := ln.Accept()
				if err != nil {
					return
				}
				if atomic.AddInt32(&attempts, 1) <= 2 {
					_ = conn.Close()
					continue
				}
				serveKubeconfigOnce(conn, srvCfg, []byte(wantKubeconfig), 0)
			}
		}()

		got, err := pollKubeconfigOverSSH(context.Background(), ln.Addr().String(), clientCfg, k3sKubeconfigGuestPath, 5*time.Second, 20*time.Millisecond, 2*time.Second)
		require.NoError(t, err)
		assert.Equal(t, wantKubeconfig, string(got))
		assert.GreaterOrEqual(t, atomic.LoadInt32(&attempts), int32(3), "must have retried past the two dropped connections")
	})

	t.Run("times out with a descriptive error when nothing ever comes up", func(t *testing.T) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		addr := ln.Addr().String()
		require.NoError(t, ln.Close())

		_, err = pollKubeconfigOverSSH(context.Background(), addr, clientCfg, k3sKubeconfigGuestPath, 100*time.Millisecond, 20*time.Millisecond, 2*time.Second)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "timed out")
		assert.Contains(t, err.Error(), addr)
	})

	t.Run("an already-cancelled caller context is returned as-is, not the timeout error", func(t *testing.T) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		addr := ln.Addr().String()
		require.NoError(t, ln.Close())

		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err = pollKubeconfigOverSSH(ctx, addr, clientCfg, k3sKubeconfigGuestPath, 5*time.Second, 20*time.Millisecond, 2*time.Second)
		require.ErrorIs(t, err, context.Canceled)
	})
}

func TestProvider_Kubeconfig_ValidatesInputsBeforeDialing(t *testing.T) {
	t.Run("empty guest ip is rejected", func(t *testing.T) {
		p := New(desktop.VMConfig{SSHKeyPath: "/some/path"})
		_, err := p.Kubeconfig(context.Background(), "")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "guest ip")
	})

	t.Run("missing SSHKeyPath is rejected", func(t *testing.T) {
		p := New(desktop.VMConfig{})
		_, err := p.Kubeconfig(context.Background(), "192.168.64.2")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "SSHKeyPath")
	})
}

// TestTCPReachable covers the probe that converts a DHCP lease entry from a
// candidate into an answer. The whole point of the probe is that a lease can
// name an address nothing is listening on — fixedGuestMAC keeps this guest's
// lease across boots — so "nothing there" must be distinguishable from "guest
// is up", cheaply and on every poll tick.
func TestTCPReachable(t *testing.T) {
	t.Run("an address that accepts is reachable", func(t *testing.T) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		t.Cleanup(func() { _ = ln.Close() })
		go func() {
			for {
				conn, err := ln.Accept()
				if err != nil {
					return
				}
				_ = conn.Close()
			}
		}()

		assert.True(t, tcpReachable(context.Background(), ln.Addr().String(), 2*time.Second))
	})

	t.Run("an address nothing listens on is NOT reachable — the stale-lease case", func(t *testing.T) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		addr := ln.Addr().String()
		require.NoError(t, ln.Close())

		assert.False(t, tcpReachable(context.Background(), addr, 500*time.Millisecond))
	})

	t.Run("a cancelled context is not reachable, and does not block", func(t *testing.T) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		t.Cleanup(func() { _ = ln.Close() })

		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		assert.False(t, tcpReachable(ctx, ln.Addr().String(), 2*time.Second))
	})
}

// TestPollKubeconfigOverSSH_TimeoutNamesThePhase is the honest-error half of
// the fix. One retry loop covers two unrelated waits — "is a guest answering"
// and "has k3s written its kubeconfig" — and before this the timeout reported
// whichever error the last attempt happened to produce. A wrong address
// therefore failed with "connect: no route to host", which reads as a network
// fault, and was indistinguishable in the log from a guest that was up but
// slow to start k3s. The two need opposite things from whoever reads them.
func TestPollKubeconfigOverSSH_TimeoutNamesThePhase(t *testing.T) {
	clientSigner, privPEM := generateTestSSHKeypair(t)
	keyPath := writeTestKeyFile(t, privPEM)
	signer, err := loadSSHSigner(keyPath)
	require.NoError(t, err)
	clientCfg := &ssh.ClientConfig{
		User:            sshUser,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         2 * time.Second,
	}

	t.Run("never reachable: the error points at connectivity, never k3s", func(t *testing.T) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		addr := ln.Addr().String()
		require.NoError(t, ln.Close())

		_, err = pollKubeconfigOverSSH(context.Background(), addr, clientCfg,
			k3sKubeconfigGuestPath, 150*time.Millisecond, 20*time.Millisecond, 500*time.Millisecond)

		require.Error(t, err)
		assert.Contains(t, err.Error(), "waiting for ssh",
			"the phase that actually ran out must be named")
		// This used to require the message to say the guest "is not running or
		// is not at this address" — a conclusion the loop cannot reach from
		// its own packets going unanswered, and one that was false in
		// production while macOS dropped them. The contract is now that the
		// message names the OBSERVATION (no connection was established) and
		// leaves the cause open; see
		// TestPollKubeconfigOverSSH_TimeoutClassifiesThreePhases.
		assert.Contains(t, err.Error(), "connectivity",
			"a never-reachable guest is a connectivity problem and must say so")
		assert.NotContains(t, err.Error(), "waiting for k3s",
			"k3s was never reached; blaming it is the misleading message this fix removes")
	})

	t.Run("reachable but no kubeconfig: the error blames k3s, and says the network is fine", func(t *testing.T) {
		// SSH serves perfectly; `cat` exits non-zero, exactly as it does
		// while k3s has not yet written the file.
		srvCfg := newTestServerConfig(t, clientSigner.PublicKey())
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		t.Cleanup(func() { _ = ln.Close() })
		go func() {
			for {
				conn, err := ln.Accept()
				if err != nil {
					return
				}
				go serveKubeconfigOnce(conn, srvCfg, nil, 1 /* cat: no such file */)
			}
		}()

		_, err = pollKubeconfigOverSSH(context.Background(), ln.Addr().String(), clientCfg,
			k3sKubeconfigGuestPath, 200*time.Millisecond, 20*time.Millisecond, 2*time.Second)

		require.Error(t, err)
		assert.Contains(t, err.Error(), "waiting for k3s",
			"ssh was reachable, so the only thing left to wait for is k3s")
		assert.Contains(t, err.Error(), "not a network problem",
			"the message must actively rule out the reading it used to invite")
		assert.Contains(t, err.Error(), k3sKubeconfigGuestPath)
	})

	t.Run("reachability is sticky: one good connection means later drops still blame k3s", func(t *testing.T) {
		// The guest answered once, so it exists and its address is right. If
		// it stops answering afterwards, the honest report is still "k3s
		// never got there" — a guest that came up and then went quiet is not
		// the wrong-address failure, and reporting the last attempt's dial
		// error would put us straight back to the misleading message.
		srvCfg := newTestServerConfig(t, clientSigner.PublicKey())
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		addr := ln.Addr().String()

		var served int32
		done := make(chan struct{})
		go func() {
			for {
				conn, err := ln.Accept()
				if err != nil {
					return
				}
				// Serve exactly one full SSH session (proving reachability),
				// then stop listening entirely.
				if atomic.AddInt32(&served, 1) == 1 {
					serveKubeconfigOnce(conn, srvCfg, nil, 1)
					_ = ln.Close()
					close(done)
					return
				}
			}
		}()

		_, err = pollKubeconfigOverSSH(context.Background(), addr, clientCfg,
			k3sKubeconfigGuestPath, 400*time.Millisecond, 20*time.Millisecond, 2*time.Second)

		<-done
		require.Error(t, err)
		assert.Contains(t, err.Error(), "waiting for k3s",
			"reachability once proven must not be forgotten by a later dial failure")
	})
}

// blackholeAddr is an address whose SYNs vanish with no answer of any kind:
// RFC 5737 TEST-NET-1 is guaranteed never to be routed to a real host, so a
// connect to it neither completes nor is refused — it hangs until something
// gives up. That is precisely the shape of the production failure this
// models (macOS's Local Network privacy gate drops an app's LAN traffic the
// same way), and it is the only shape that exposes an unbounded dial: a
// closed port answers with RST immediately and would pass either way.
//
// On a host that instead fails this fast (a sandbox with no route at all),
// the assertions still hold — they just stop being able to catch a
// regression, which is why the elapsed-time bound is asserted rather than
// the error string.
const blackholeAddr = "192.0.2.1:22"

// TestFetchKubeconfigOverSSH_DialIsBoundedByAttemptTimeout pins the half of
// the attempt budget that was never enforced. attemptTimeout was applied via
// conn.SetDeadline — which cannot run until the dial has already returned —
// so the TCP connect itself was bounded only by the caller's context and, in
// production, by the OS connect timeout (darwin net.inet.tcp.keepinit, 75s).
// One attempt therefore outlived thirty-seven ticks of a 2s retry loop that
// believed it was retrying, and a guest that came up during that window was
// not noticed until the attempt in flight finally expired.
func TestFetchKubeconfigOverSSH_DialIsBoundedByAttemptTimeout(t *testing.T) {
	_, privPEM := generateTestSSHKeypair(t)
	signer, err := loadSSHSigner(writeTestKeyFile(t, privPEM))
	require.NoError(t, err)
	clientCfg := &ssh.ClientConfig{
		User:            sshUser,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
	}

	const attemptTimeout = 250 * time.Millisecond
	start := time.Now()
	// context.Background() on purpose: the caller's context must NOT be what
	// bounds one attempt, or the bound scales with the whole retry budget.
	_, err = fetchKubeconfigOverSSH(context.Background(), blackholeAddr, clientCfg, k3sKubeconfigGuestPath, attemptTimeout)
	elapsed := time.Since(start)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "dial")
	assert.Less(t, elapsed, 5*time.Second,
		"the TCP connect must be bounded by attemptTimeout, not by the OS connect timeout")
}

// TestPollKubeconfigOverSSH_TimeoutClassifiesThreePhases covers the reporting
// half. The loop spans three unrelated waits — nothing answers at all, sshd
// answers but will not let us through, sshd let us through but k3s has not
// written the file — and it previously collapsed the first two into one
// message that ASSERTED "the guest is not running or is not at this address".
// That assertion was made without ever checking, and in the failure that
// prompted this it was simply false: the guest was running, at that address,
// with sshd listening and the kubeconfig already written, while macOS was
// dropping this process's packets.
func TestPollKubeconfigOverSSH_TimeoutClassifiesThreePhases(t *testing.T) {
	clientSigner, privPEM := generateTestSSHKeypair(t)
	signer, err := loadSSHSigner(writeTestKeyFile(t, privPEM))
	require.NoError(t, err)
	clientCfg := &ssh.ClientConfig{
		User:            sshUser,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
	}

	t.Run("never dialed: reports connectivity without asserting the guest is absent", func(t *testing.T) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		addr := ln.Addr().String()
		require.NoError(t, ln.Close())

		_, err = pollKubeconfigOverSSH(context.Background(), addr, clientCfg,
			k3sKubeconfigGuestPath, 150*time.Millisecond, 20*time.Millisecond, 100*time.Millisecond)

		require.Error(t, err)
		assert.Contains(t, err.Error(), "connectivity")
		assert.NotContains(t, err.Error(), "the guest is not running or is not at this address",
			"the loop never checked whether the guest was running; it must not assert that it was not")
	})

	t.Run("SYNs swallowed: names the host-side block that produces exactly this", func(t *testing.T) {
		_, err := pollKubeconfigOverSSH(context.Background(), blackholeAddr, clientCfg,
			k3sKubeconfigGuestPath, 400*time.Millisecond, 20*time.Millisecond, 150*time.Millisecond)

		require.Error(t, err)
		assert.Contains(t, err.Error(), "Local Network",
			"a dial that times out rather than being refused is the signature of the macOS privacy gate, "+
				"which is invisible from inside this process and can only be surfaced here")
	})

	t.Run("TCP accepted but ssh never completes: blames sshd, not the address", func(t *testing.T) {
		// The server only trusts a different key, so every attempt gets a
		// full TCP connection and then fails the handshake — proof the guest
		// is up and correctly addressed, and that the problem is credentials.
		otherSigner, _ := generateTestSSHKeypair(t)
		srvCfg := newTestServerConfig(t, otherSigner.PublicKey())
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		t.Cleanup(func() { _ = ln.Close() })
		go func() {
			for {
				conn, err := ln.Accept()
				if err != nil {
					return
				}
				go serveKubeconfigOnce(conn, srvCfg, nil, 0)
			}
		}()

		_, err = pollKubeconfigOverSSH(context.Background(), ln.Addr().String(), clientCfg,
			k3sKubeconfigGuestPath, 200*time.Millisecond, 20*time.Millisecond, 2*time.Second)

		require.Error(t, err)
		assert.Contains(t, err.Error(), "sshd", "TCP was accepted, so the address and the guest are fine")
		assert.NotContains(t, err.Error(), "connectivity",
			"a connection was established; calling this connectivity sends the reader the wrong way")
	})

	t.Run("clientSigner is the trusted key in the happy path (guards the fixture)", func(t *testing.T) {
		srvCfg := newTestServerConfig(t, clientSigner.PublicKey())
		addr := startSingleShotSSHServer(t, srvCfg, []byte("ok\n"), 0)
		got, err := pollKubeconfigOverSSH(context.Background(), addr, clientCfg,
			k3sKubeconfigGuestPath, 2*time.Second, 20*time.Millisecond, 2*time.Second)
		require.NoError(t, err)
		assert.Equal(t, "ok\n", string(got))
	})
}

// startAcceptingListener starts a listener that accepts and immediately
// closes every connection — the cheapest stand-in for "a guest is listening
// on this port now", which is exactly what the reachability probe asks.
func startAcceptingListener(t *testing.T) (host string, port int) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()
	tcpAddr, ok := ln.Addr().(*net.TCPAddr)
	require.True(t, ok)
	return tcpAddr.IP.String(), tcpAddr.Port
}

// startedProvider returns a Provider staged as Start() leaves one: a live
// per-boot guestIPCh, and no pinned MAC, so the lease source is inert and the
// vsock source is the only one under test.
func startedProvider(t *testing.T) (*Provider, chan struct{}) {
	t.Helper()
	p := New(desktop.VMConfig{})
	ch := make(chan struct{})
	p.guestIPCh = ch
	return p, ch
}

// resolveVsock stages the outcome of one boot's vsock handshake the way
// receiveGuestIP would, and closes the boot's channel.
func resolveVsock(t *testing.T, p *Provider, ch chan struct{}, ip string, err error) {
	t.Helper()
	p.guestIPMu.Lock()
	p.guestIP, p.guestErr = ip, err
	p.guestIPMu.Unlock()
	close(ch)
}

// TestProbeUntilReachable covers the shared probe loop that converts a
// candidate address — from either discovery source — into an answer.
func TestProbeUntilReachable(t *testing.T) {
	t.Run("an address that answers is sent on found", func(t *testing.T) {
		host, port := startAcceptingListener(t)
		found := make(chan string, 1)

		go probeUntilReachable(t.Context(), host, port, 10*time.Millisecond, found)

		select {
		case got := <-found:
			assert.Equal(t, host, got)
		case <-time.After(3 * time.Second):
			t.Fatal("a listening address was never reported reachable")
		}
	})

	t.Run("an address that never answers is never sent, and the loop exits with ctx", func(t *testing.T) {
		found := make(chan string, 1)
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		t.Cleanup(cancel)
		done := make(chan struct{})

		go func() {
			defer close(done)
			probeUntilReachable(ctx, "192.0.2.1", 22, 10*time.Millisecond, found)
		}()

		select {
		case ip := <-found:
			t.Fatalf("an unreachable address must never be reported: got %q", ip)
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("probe loop did not exit with its context")
		}
	})
}

// TestGuestIP_VsockAddressIsACandidateNotAnAnswer pins the hole that let
// bring-up advance past "Waiting for guest network" on a host that could not
// reach the guest at all.
//
// The lease source has always probed its candidate before returning it — "a
// matching lease is a CANDIDATE, never an answer" — because a lease can name
// an address nothing is listening on. The vsock source was exempt, and vsock
// is not IP networking: it is a virtio device, so it keeps working when the
// host cannot route a single packet to the guest. In production that exempt
// path won the race in six seconds on a host whose LAN traffic macOS was
// dropping, GuestIP returned an address this process could never reach, and
// the failure surfaced five minutes later, one step downstream, blaming k3s.
func TestGuestIP_VsockAddressIsACandidateNotAnAnswer(t *testing.T) {
	t.Run("an unreachable vsock-reported address is not returned", func(t *testing.T) {
		p, ch := startedProvider(t)
		resolveVsock(t, p, ch, "192.0.2.1", nil)

		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		t.Cleanup(cancel)
		ip, err := p.GuestIP(ctx)

		require.Error(t, err, "an address that answers nothing must not be handed to the caller")
		assert.Empty(t, ip)
		assert.ErrorIs(t, err, context.DeadlineExceeded)
	})

	t.Run("a reachable vsock-reported address is returned", func(t *testing.T) {
		host, port := startAcceptingListener(t)
		p, ch := startedProvider(t)
		p.guestProbePort = port
		resolveVsock(t, p, ch, host, nil)

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		t.Cleanup(cancel)
		got, err := p.GuestIP(ctx)

		require.NoError(t, err)
		assert.Equal(t, host, got)
	})

	t.Run("a vsock handshake failure is surfaced, not waited out", func(t *testing.T) {
		// With no pinned MAC there is no second source, so the vsock failure
		// is the whole story and the caller should hear it immediately rather
		// than block for guestIPWaitTimeout on a race nothing can still win.
		p, ch := startedProvider(t)
		resolveVsock(t, p, ch, "", errors.New("accept vsock connection: boom"))

		start := time.Now()
		ip, err := p.GuestIP(t.Context())

		require.Error(t, err)
		assert.Empty(t, ip)
		assert.Contains(t, err.Error(), "boom")
		assert.Less(t, time.Since(start), 5*time.Second, "must not wait out the full window")
	})

	t.Run("GuestIP before Start is rejected", func(t *testing.T) {
		p := New(desktop.VMConfig{})
		_, err := p.GuestIP(t.Context())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not started")
	})
}

// TestTCPProbe_DistinguishesRefusedFromSilent pins the distinction the
// fail-fast path is built on, and which tcpReachable's bool destroyed.
//
// The two failures mean opposite things. A REFUSAL proves packets reach the
// guest and only sshd is missing, so waiting is correct. SILENCE is what both
// a dead address and a host-side network block look like — and a DHCP lease
// having appeared already rules out the guest never coming up, which is what
// makes sustained silence diagnosable rather than ambiguous.
func TestTCPProbe_DistinguishesRefusedFromSilent(t *testing.T) {
	t.Run("a listener answers: reachable", func(t *testing.T) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		t.Cleanup(func() { _ = ln.Close() })
		assert.Equal(t, probeReachable, tcpProbe(context.Background(), ln.Addr().String(), 2*time.Second))
	})

	t.Run("a closed port on a live host: REFUSED, not silent — the guest is merely still booting", func(t *testing.T) {
		// Bind then immediately close: the port is now certainly nobody's, on a
		// host that certainly answers. That is the shape a guest presents
		// between DHCP and sshd, and it must NOT read as a network block.
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		addr := ln.Addr().String()
		require.NoError(t, ln.Close())

		assert.Equal(t, probeRefused, tcpProbe(context.Background(), addr, 2*time.Second),
			"loopback refuses a closed port; classifying that as silence would fail every slow boot")
	})

	t.Run("a blackholed address: SILENT", func(t *testing.T) {
		// 192.0.2.0/24 is TEST-NET-1 (RFC 5737) and is not routable, so the
		// dial times out with nothing coming back — the same shape as packets
		// being dropped by a host-side block.
		assert.Equal(t, probeSilent, tcpProbe(context.Background(), "192.0.2.1:22", 300*time.Millisecond))
	})

	t.Run("tcpReachable still reports only reachability", func(t *testing.T) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		addr := ln.Addr().String()
		require.NoError(t, ln.Close())
		assert.False(t, tcpReachable(context.Background(), addr, 2*time.Second),
			"the bool wrapper must keep its old meaning for existing callers")
	})
}

// TestReachabilityWitness_OnlyBlockedWhenLeaseSeenAndNothingEverAnswered pins
// the predicate GuestIP fails fast on. Three of the four states must NOT trip
// it: failing a legitimately slow boot on a heuristic would be worse than the
// four-minute wait this replaces.
func TestReachabilityWitness_OnlyBlockedWhenLeaseSeenAndNothingEverAnswered(t *testing.T) {
	// Mirrors GuestIP's guard: !sawLease || sawRefusal => keep waiting.
	blocked := func(lease, refusal bool) bool { return lease && !refusal }

	assert.False(t, blocked(false, false), "no lease yet: the guest may still be booting")
	assert.False(t, blocked(false, true), "no lease: nothing to conclude")
	assert.False(t, blocked(true, true), "something answered — packets get through, so keep waiting for sshd")
	assert.True(t, blocked(true, false), "lease taken, nothing ever answered: the guest is up and we cannot reach it")
}
