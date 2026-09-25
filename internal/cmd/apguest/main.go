//go:build linux

// Command apguest is the tiny in-VM guest agent baked into the desktop
// bundle's rootfs (see build/desktop/apguest.service). Its only job is to
// report the VM's NAT-assigned IPv4 address to the host over AF_VSOCK so
// the host side (cmd/oap/internal/desktop/vz/provider_darwin.go's
// receiveGuestIP) can rewrite the kubeconfig server URL. See that file's
// package doc for why this is a guest-dials-out handshake rather than the
// host reaching into the guest.
//
// Wire protocol (must match receiveGuestIP exactly):
//   - guest connects OUT to the host over AF_VSOCK, CID VMADDR_CID_HOST
//     (2), port 5051 (guestIPVsockPort in provider_darwin.go).
//   - guest writes the IPv4 address as UTF-8 text followed by a single
//     "\n", then closes the connection. The host reads one line with
//     bufio.Scanner.
package main

import (
	"fmt"
	"net"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

const (
	// hostVsockPort must match guestIPVsockPort in
	// cmd/oap/internal/desktop/vz/provider_darwin.go.
	hostVsockPort = 5051

	maxAttempts   = 30
	retryInterval = 2 * time.Second
)

func main() {
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if err := reportIP(); err != nil {
			lastErr = err
			fmt.Fprintf(os.Stderr, "apguest: attempt %d/%d failed: %v\n", attempt, maxAttempts, err)
			time.Sleep(retryInterval)
			continue
		}
		return
	}
	fmt.Fprintf(os.Stderr, "apguest: giving up after %d attempts: %v\n", maxAttempts, lastErr)
	os.Exit(1)
}

// reportIP determines the VM's primary non-loopback IPv4 address, dials
// the host over vsock, and writes the address as a newline-terminated
// UTF-8 string.
func reportIP() error {
	ip, err := primaryIPv4()
	if err != nil {
		return fmt.Errorf("determine primary IPv4: %w", err)
	}

	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM, 0)
	if err != nil {
		return fmt.Errorf("create AF_VSOCK socket: %w", err)
	}
	defer unix.Close(fd)

	addr := &unix.SockaddrVM{CID: unix.VMADDR_CID_HOST, Port: hostVsockPort}
	if err := unix.Connect(fd, addr); err != nil {
		return fmt.Errorf("connect to host vsock port %d: %w", hostVsockPort, err)
	}

	if _, err := unix.Write(fd, []byte(ip+"\n")); err != nil {
		return fmt.Errorf("write IP over vsock: %w", err)
	}

	return nil
}

// primaryIPv4 enumerates network interfaces and returns the first global
// unicast IPv4 address on an up, non-loopback interface — the VM's NAT
// lease (e.g. 192.168.64.x).
func primaryIPv4() (string, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return "", fmt.Errorf("list interfaces: %w", err)
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			ipNet, ok := addr.(*net.IPNet)
			if !ok {
				continue
			}
			ip4 := ipNet.IP.To4()
			if ip4 == nil || !ip4.IsGlobalUnicast() {
				continue
			}
			return ip4.String(), nil
		}
	}
	return "", fmt.Errorf("no global unicast IPv4 address found on any up, non-loopback interface")
}
