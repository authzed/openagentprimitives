// Package vz — this file holds the pure, platform-independent half of the
// lease-based guest-IP discovery: parsing macOS's NAT DHCP lease file and
// matching a MAC address against it. It carries no build tag (unlike
// provider_darwin.go) so it can be unit-tested on any GOOS/GOARCH.
package vz

import (
	"bufio"
	"strconv"
	"strings"
)

// dhcpLeaseFilePath is where macOS's built-in NAT DHCP server (bootpd)
// records active leases for the shared/NAT network the Virtualization
// framework's NAT attachment rides on. It's world-readable — no sudo
// needed to poll it.
const dhcpLeaseFilePath = "/var/db/dhcpd_leases"

// leaseIPForMAC scans the contents of a macOS DHCP lease file (the
// dhcpLeaseFilePath format: a sequence of `{ ... }` blocks, each holding
// `ip_address=` and `hw_address=` lines among others) for the block whose
// hardware address matches mac, and returns that block's IP address.
//
// hw_address values in the lease file look like "1,6e:14:dc:de:1e:0": a
// leading "<hw-type>," prefix (1 == Ethernet) followed by a MAC address
// whose octets have had their leading zero trimmed (macOS renders "1e:00"
// as "1e:0", "06" as "6", etc). Comparison is therefore done on the parsed
// 6-byte address via normalizeMAC, never on the raw strings.
func leaseIPForMAC(leaseFileContents string, mac string) (string, bool) {
	want, ok := normalizeMAC(mac)
	if !ok {
		return "", false
	}

	var ip, hw string
	inBlock := false

	checkBlock := func() (string, bool) {
		if ip == "" || hw == "" {
			return "", false
		}
		got, ok := normalizeMAC(hw)
		if !ok || got != want {
			return "", false
		}
		return ip, true
	}

	scanner := bufio.NewScanner(strings.NewReader(leaseFileContents))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		switch {
		case line == "{":
			inBlock = true
			ip, hw = "", ""
		case line == "}":
			if inBlock {
				if foundIP, found := checkBlock(); found {
					return foundIP, true
				}
			}
			inBlock = false
		case strings.HasPrefix(line, "ip_address="):
			ip = strings.TrimSpace(strings.TrimPrefix(line, "ip_address="))
		case strings.HasPrefix(line, "hw_address="):
			hw = strings.TrimSpace(strings.TrimPrefix(line, "hw_address="))
		}
	}
	return "", false
}

// normalizeMAC parses a MAC address string into a comparable 6-byte form,
// tolerating the two variations found in a macOS DHCP lease file:
//   - an optional leading "<hw-type>," prefix (e.g. "1,6e:14:dc:de:1e:0")
//   - octets with a trimmed leading zero (e.g. "1e:0" meaning "1e:00")
//
// It returns ok == false if mac doesn't parse as six colon-separated hex
// octets once the optional prefix is stripped.
func normalizeMAC(mac string) (addr [6]byte, ok bool) {
	if idx := strings.Index(mac, ","); idx >= 0 {
		mac = mac[idx+1:]
	}
	parts := strings.Split(mac, ":")
	if len(parts) != len(addr) {
		return addr, false
	}
	for i, p := range parts {
		v, err := strconv.ParseUint(p, 16, 8)
		if err != nil {
			return addr, false
		}
		addr[i] = byte(v)
	}
	return addr, true
}
