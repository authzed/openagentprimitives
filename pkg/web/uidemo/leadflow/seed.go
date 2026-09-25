package leadflow

// seedLeads returns the deterministic fabricated pipeline every NewBook() starts
// from. Fixed literal dates, never time.Now(), so a test asserting a stage count
// or a date-range filter never depends on the clock. The twelve Updated
// timestamps span roughly ninety days (2026-05-02 through 2026-07-31) so a
// date-range change visibly narrows both the table and the stage_breakdown
// chart. Company and owner names are fabricated.
func seedLeads() []Lead {
	return []Lead{
		{Value: "lead-aurorabyte", Label: "Aurorabyte Systems", Stage: "new", Owner: "Dana Osei", Updated: "2026-05-02T09:15:00Z"},
		{Value: "lead-brightfern", Label: "Brightfern Robotics", Stage: "new", Owner: "Marcus Webb", Updated: "2026-05-14T11:40:00Z"},
		{Value: "lead-cobaltstatic", Label: "Cobalt Static Systems", Stage: "new", Owner: "Priya Menon", Updated: "2026-05-28T16:05:00Z"},
		{Value: "lead-driftwood", Label: "Driftwood Logistics", Stage: "qualified", Owner: "Dana Osei", Updated: "2026-06-04T08:30:00Z"},
		{Value: "lead-emberline", Label: "Emberline Foods", Stage: "qualified", Owner: "Marcus Webb", Updated: "2026-06-11T13:50:00Z"},
		{Value: "lead-fernwood", Label: "Fernwood Analytics", Stage: "qualified", Owner: "Priya Menon", Updated: "2026-06-19T10:20:00Z"},
		{Value: "lead-granitepeak", Label: "Granite Peak Freight", Stage: "proposal", Owner: "Dana Osei", Updated: "2026-06-27T15:00:00Z"},
		{Value: "lead-harborline", Label: "Harborline Marine Supply", Stage: "proposal", Owner: "Marcus Webb", Updated: "2026-07-05T09:45:00Z"},
		{Value: "lead-ironwood", Label: "Ironwood Materials", Stage: "proposal", Owner: "Priya Menon", Updated: "2026-07-13T12:10:00Z"},
		{Value: "lead-juniper", Label: "Juniper Home Goods", Stage: "won", Owner: "Dana Osei", Updated: "2026-07-21T14:35:00Z"},
		{Value: "lead-kestrel", Label: "Kestrel Outdoor Gear", Stage: "won", Owner: "Marcus Webb", Updated: "2026-07-28T17:00:00Z"},
		{Value: "lead-larkspur", Label: "Larkspur Wellness Group", Stage: "won", Owner: "Priya Menon", Updated: "2026-07-31T09:00:00Z"},
	}
}
