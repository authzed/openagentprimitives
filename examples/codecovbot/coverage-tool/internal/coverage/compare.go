package coverage

import (
	"fmt"
	"math"
	"math/big"
	"strconv"
)

// Comparison is unavailable when a compatible exact baseline is absent or either
// denominator is zero. The caller authenticates the baseline revision and scope.
type Comparison struct {
	Metric         string   `json:"metric"`
	Compatible     bool     `json:"compatible"`
	Regression     bool     `json:"regression"`
	DropPP         *float64 `json:"drop_pp"`
	ThresholdPP    float64  `json:"threshold_pp"`
	CurrentDisplay string   `json:"current_percent_display"`
	BaseDisplay    string   `json:"base_percent_display"`
	Reason         string   `json:"reason,omitempty"`
}

func Compare(current Report, base *Report, name string, threshold float64) (Comparison, error) {
	out := Comparison{Metric: name, ThresholdPP: threshold, CurrentDisplay: "unavailable", BaseDisplay: "unavailable"}
	if name != "functional" && name != "raw" {
		return out, fmt.Errorf("preferred metric must be functional or raw")
	}
	if math.IsNaN(threshold) || math.IsInf(threshold, 0) || threshold < 0 || threshold > 100 {
		return out, fmt.Errorf("threshold must be between zero and 100 percentage points")
	}
	choose := func(r Report) Metric {
		if name == "raw" {
			return r.Raw
		}
		return r.Functional
	}
	cur := choose(current)
	out.CurrentDisplay = metric(cur.Covered, cur.Total).DisplayPercent
	if base == nil {
		out.Reason = "exact compatible base report unavailable"
		return out, nil
	}
	prev := choose(*base)
	out.BaseDisplay = metric(prev.Covered, prev.Total).DisplayPercent
	if current.SchemaVersion != "v1" || base.SchemaVersion != "v1" || current.AnalyzerVersion != base.AnalyzerVersion || current.AnalyzerVersion != AnalyzerVersion || current.PolicyHash == "" || current.PolicyHash != base.PolicyHash {
		out.Reason = "base schema, analyzer version or policy fingerprint differs"
		return out, nil
	}
	if cur.Total <= 0 || prev.Total <= 0 || cur.Covered < 0 || prev.Covered < 0 || cur.Covered > cur.Total || prev.Covered > prev.Total {
		out.Reason = "coverage denominator unavailable or counts invalid"
		return out, nil
	}
	// Rational arithmetic avoids overflow and rounding at the threshold boundary.
	diff := new(big.Rat).Sub(big.NewRat(prev.Covered, prev.Total), big.NewRat(cur.Covered, cur.Total))
	diff.Mul(diff, big.NewRat(100, 1))
	thresholdRat, ok := new(big.Rat).SetString(strconv.FormatFloat(threshold, 'g', -1, 64))
	if !ok {
		return out, fmt.Errorf("invalid threshold")
	}
	drop, _ := diff.Float64()
	out.DropPP = &drop
	out.Compatible = true
	out.Regression = diff.Sign() > 0 && diff.Cmp(thresholdRat) >= 0
	return out, nil
}
