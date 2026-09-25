//go:build !linux

// This stub exists solely so `go build ./...` succeeds on non-Linux
// development hosts (e.g. the macOS box building the oap bundle). The real
// implementation (main.go) is linux-only: apguest only ever runs inside
// the guest VM's rootfs.
package main

func main() {}
