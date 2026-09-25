// Package uihelpers contains small render helpers shared across the oap CLI's
// "show" commands so each command file stays focused on its resource shape.
package uihelpers

import (
	"fmt"
	"io"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// PrintConditions renders a list of metav1.Conditions with the same shape used
// elsewhere in oap show commands: one line per condition with type=status,
// reason inline, and the message indented underneath (split on newlines).
func PrintConditions(out io.Writer, conds []metav1.Condition, indent string) {
	if len(conds) == 0 {
		fmt.Fprintf(out, "%s(none)\n", indent)
		return
	}
	for _, c := range conds {
		fmt.Fprintf(out, "%s- %s=%s reason=%s\n", indent, c.Type, c.Status, c.Reason)
		if c.Message != "" {
			for _, line := range strings.Split(c.Message, "\n") {
				fmt.Fprintf(out, "%s    %s\n", indent, line)
			}
		}
	}
}
