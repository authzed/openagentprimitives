package bento

import (
	"fmt"
	"strings"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// buildStreamYAML translates a Channel's bento.generate config into
// the YAML Bento's StreamBuilder.SetYAML expects. The output uses our
// locally-registered `forward_to_pipeline` output (see
// forward_output.go) so each generated message lands in the
// channelsd pipeline.
//
// We emit YAML rather than building the underlying internal config
// programmatically because (a) it's the public contract Bento
// guarantees stability on, and (b) it sidesteps version drift in
// the unexported StreamConfig type between bento minor versions.
func buildStreamYAML(ch *spiceboxv1alpha1.Channel) (string, error) {
	if ch.Spec.Bento == nil || ch.Spec.Bento.Generate == nil {
		return "", fmt.Errorf("buildStreamYAML: Channel %q has no spec.bento.generate", ch.Name)
	}
	gen := ch.Spec.Bento.Generate

	// Use YAML single-quoted-scalar syntax so the bloblang script's
	// embedded double quotes don't need to be escaped. Single-quoted
	// YAML escapes only the single quote itself (by doubling).
	mapping := strings.ReplaceAll(gen.Mapping, "'", "''")

	var b strings.Builder
	fmt.Fprintln(&b, "input:")
	fmt.Fprintln(&b, "  generate:")
	fmt.Fprintf(&b, "    mapping: '%s'\n", mapping)
	fmt.Fprintf(&b, "    interval: %q\n", gen.Interval)
	if gen.Count > 0 {
		fmt.Fprintf(&b, "    count: %d\n", gen.Count)
	} else {
		fmt.Fprintln(&b, "    count: 0")
	}
	fmt.Fprintln(&b, "output:")
	fmt.Fprintln(&b, "  forward_to_pipeline:")
	fmt.Fprintf(&b, "    channel_name: %q\n", ch.Name)
	fmt.Fprintf(&b, "    channel_namespace: %q\n", ch.Namespace)
	fmt.Fprintf(&b, "    authz_subject: %q\n", ch.Spec.AuthzSubject)
	return b.String(), nil
}
