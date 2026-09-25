//go:build darwin && arm64

package desktopcmd

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestShouldKillOrphan(t *testing.T) {
	const ourRootfs = "/Users/tester/Library/Application Support/oap/rootfs.img"

	cases := []struct {
		name       string
		openFiles  []string
		rootfsPath string
		want       bool
	}{
		{
			name: "our orphan: holds our rootfs, no Docker path -> kill",
			openFiles: []string{
				"/dev/null",
				ourRootfs,
			},
			rootfsPath: ourRootfs,
			want:       true,
		},
		{
			name: "our orphan with a stale deleted handle (re-staged/uninstalled since) -> kill",
			openFiles: []string{
				ourRootfs + " (deleted)",
			},
			rootfsPath: ourRootfs,
			want:       true,
		},
		{
			name: "Docker Desktop's VM surfacing our rootfs via virtiofs + Docker.raw -> never kill",
			openFiles: []string{
				ourRootfs,
				"/Users/tester/Library/Containers/com.docker.docker/Data/vms/0/data/Docker.raw",
			},
			rootfsPath: ourRootfs,
			want:       false,
		},
		{
			name: "Docker Desktop's VM surfacing our rootfs + com.docker path -> never kill",
			openFiles: []string{
				ourRootfs,
				"/Applications/Docker.app/Contents/MacOS/com.docker.backend",
			},
			rootfsPath: ourRootfs,
			want:       false,
		},
		{
			name: "unrelated process (does not hold our rootfs at all) -> not a candidate",
			openFiles: []string{
				"/usr/lib/dyld",
				"/System/Library/Frameworks/Foundation.framework/Foundation",
			},
			rootfsPath: ourRootfs,
			want:       false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := shouldKillOrphan(tc.openFiles, tc.rootfsPath)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestHoldsPath(t *testing.T) {
	const target = "/tmp/rootfs.img"

	cases := []struct {
		name      string
		openFiles []string
		want      bool
	}{
		{name: "exact match", openFiles: []string{"/tmp/other", target}, want: true},
		{name: "deleted match", openFiles: []string{target + " (deleted)"}, want: true},
		{name: "no match", openFiles: []string{"/tmp/rootfs.img.tmp"}, want: false},
		{name: "empty", openFiles: nil, want: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, holdsPath(tc.openFiles, target))
		})
	}
}

func TestParsePgrepPIDs(t *testing.T) {
	cases := []struct {
		name string
		out  string
		want []int
	}{
		{name: "no matches (empty output)", out: "", want: nil},
		{name: "single pid", out: "83506\n", want: []int{83506}},
		{name: "multiple pids", out: "111\n222\n333\n", want: []int{111, 222, 333}},
		{name: "trailing/blank lines tolerated", out: "\n111\n\n222\n", want: []int{111, 222}},
		{name: "non-numeric lines skipped", out: "111\ngarbage\n222\n", want: []int{111, 222}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, parsePgrepPIDs(tc.out))
		})
	}
}

func TestJoinInts(t *testing.T) {
	assert.Equal(t, "", joinInts(nil, ","))
	assert.Equal(t, "12", joinInts([]int{12}, ","))
	assert.Equal(t, "12,34,56", joinInts([]int{12, 34, 56}, ","))
}

func TestParseLsofPidNameOutput(t *testing.T) {
	// Real `lsof -Fpn` output: a "p<pid>" line starts each process, followed
	// by any number of "n<name>" lines (interleaved in practice with other
	// field lines this parser ignores, but -Fpn only ever emits p/n fields).
	raw := "p111\nn/dev/null\nn/tmp/rootfs.img\np222\nn/tmp/rootfs.img (deleted)\nn/Applications/Docker.app/Contents/MacOS/Docker\n"

	got := parseLsofPidNameOutput(raw)

	assert.Equal(t, []string{"/dev/null", "/tmp/rootfs.img"}, got[111])
	assert.Equal(t, []string{"/tmp/rootfs.img (deleted)", "/Applications/Docker.app/Contents/MacOS/Docker"}, got[222])
	assert.Len(t, got, 2)
}
