//go:build !race

package wstest

// raceFactor leaves a deadline alone when the race detector is off: an
// uninstrumented local run is the machine the base durations were chosen for.
const raceFactor = 1
