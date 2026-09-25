package cost

import (
	"fmt"

	"github.com/dustin/go-humanize"
)

// formatUSD renders micro-USD as a dollar string. Sub-cent costs get more
// precision so they aren't shown as "$0.00".
func formatUSD(microUSD int64) string {
	usd := float64(microUSD) / 1e6
	if usd != 0 && usd < 0.01 {
		return fmt.Sprintf("$%.4f", usd)
	}
	return fmt.Sprintf("$%.2f", usd)
}

// formatTokens renders a token count with thousands separators (lib-backed).
func formatTokens(n int64) string { return humanize.Comma(n) }
