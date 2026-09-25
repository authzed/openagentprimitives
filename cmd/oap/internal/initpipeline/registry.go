package initpipeline

var registry []Component

// Register appends c to the package-level component registry.  Call Register
// from an init() function in each component's package so that the binary's
// import graph controls which components are available.
func Register(c Component) {
	registry = append(registry, c)
}

// All returns all registered components in registration order.
func All() []Component {
	out := make([]Component, len(registry))
	copy(out, registry)
	return out
}

// ResetForTest clears the registry.  It must only be called from tests.
func ResetForTest() {
	registry = nil
}
