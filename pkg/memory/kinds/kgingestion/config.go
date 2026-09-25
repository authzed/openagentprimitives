package kgingestion

type IngestionStrategy string

const (
	StrategyEveryTurn    IngestionStrategy = "every_turn"
	StrategyContentGated IngestionStrategy = "content_gated"
	StrategyBatch        IngestionStrategy = "batch"
)

type IngestionConfig struct {
	// Strategy selects when a completed turn is handed to the KG. The zero
	// value ("") is not one of the constants and falls through to the
	// every-turn default in handleTurn.
	Strategy IngestionStrategy
	// MinContentLength is the byte threshold below which StrategyContentGated
	// drops a turn. Read only under that strategy; 0 means nothing is gated.
	MinContentLength int
}
