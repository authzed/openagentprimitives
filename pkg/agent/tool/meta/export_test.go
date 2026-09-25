package meta

// MaxPhasesForTest exposes the plan-size limit to external _test.go files so a
// limit test derives its input from the constant rather than hardcoding a
// number that silently stops testing the boundary when the limit changes.
const MaxPhasesForTest = defaultMaxPhases
