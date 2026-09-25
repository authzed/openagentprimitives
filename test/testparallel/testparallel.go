// Package testparallel sizes `go test -p` for the envtest-backed tiers.
//
// go's default is -p=GOMAXPROCS, which over-subscribes these suites: a
// "package" here is not a goroutine. The integration tier boots an envtest
// apiserver + etcd per package and e2e adds a SpiceDB container, so the
// binding resource is per-package footprint, not CPU.
//
// The value used to be a hardcoded 2. That was set to stop the gate starving
// itself — but the starvation turned out to be leaked runner goroutines
// (InProcessRunnerFactory.Shutdown never cancelled them, so tests from finished
// packages kept burning CPU), not genuine resource pressure. With that fixed
// the tier scales close to linearly with -p, and the old cap was leaving most
// of it on the table.
package testparallel

// Floor and Ceiling bound what Derive will return.
//
// Floor 2: below this the tiers serialize, which is the bottleneck removing
// the serial test/e2e package existed to fix.
//
// Ceiling 8: the highest value actually MEASURED green — 50 packages / 148
// tests, 201s against a 465s baseline, on a 10-core / 64 GB machine. Going
// higher is not obviously wrong, it is simply unmeasured, and each extra slot
// costs another apiserver + etcd + SpiceDB container rather than another
// goroutine. Raise it only alongside a measurement.
const (
	Floor   = 2
	Ceiling = 8
)

// Derive returns how many test binaries may run concurrently on a machine with
// numCPU cores, clamped to [Floor, Ceiling].
//
// Two cores are held back for the OS and the `go test` driver itself: the
// per-package processes are not the only things running, and starving the
// driver shows up as the poll-deadline flakiness this tier is prone to.
func Derive(numCPU int) int {
	n := numCPU - 2
	if n < Floor {
		return Floor
	}
	if n > Ceiling {
		return Ceiling
	}
	return n
}
