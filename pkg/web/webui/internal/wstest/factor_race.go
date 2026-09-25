//go:build race

package wstest

// raceFactor triples every scaled deadline under `go test -race`.
//
// The race detector is what `mage test:unit` runs, and it costs a multiple of
// the uninstrumented runtime on exactly the paths these deadlines bound: every
// read and write on the shared state a websocket handler and its mirror
// goroutine touch is checked. Three is the headroom that made the two observed
// gate failures pass; it is not a measurement of the detector's overhead.
const raceFactor = 3
