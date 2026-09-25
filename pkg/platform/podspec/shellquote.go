package podspec

import "strings"

// shellQuote wraps s in single quotes for a POSIX `sh -c` script, escaping any
// embedded single quote.
//
// The mount-unpack script used fmt.Sprintf("%q") here, which is GO-literal
// quoting, not shell quoting: it escapes " and \ and leaves $ and backtick
// intact, and the result was spliced into a double-quoted word where both still
// expand. The paths it quotes derive from SpiceboxMount.Name, which carries no
// Pattern or MaxLength validation and is deliberately NOT constrained to a
// DNS-1123 label.
//
// Not model-reachable today — SpiceboxClass is cluster-scoped and the
// session-provenance path goes through a SafeSlug — so this was a latent shape
// guarded only by who happens to write the field. Same helper as
// pkg/tools/toolchain/kinds/image and pkg/tools/exec/remote; it is three lines
// and fully ours, which is where the prefer-a-library rule stops.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
