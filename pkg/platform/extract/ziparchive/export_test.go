package ziparchive

// DeclaredTotalExceeds exposes the saturating accumulator to the package's
// external test, which asserts the ARITHMETIC: a crafted archive whose
// declared sizes wrap uint64 cannot be built through archive/zip's writer, so
// the overflow behaviour is tested where it lives.
func DeclaredTotalExceeds(declared []uint64, ceiling int64) bool {
	return declaredTotalExceeds(declared, ceiling)
}
