package slack

import (
	"errors"
	"testing"

	slackapi "github.com/slack-go/slack"
	"github.com/stretchr/testify/assert"
)

func TestBlocksRejectionDetail(t *testing.T) {
	sre := slackapi.SlackErrorResponse{Err: "invalid_blocks"}
	sre.ResponseMetadata.Messages = []string{"invalid block [json-pointer:/blocks/0/chart]: too many data points"}

	// Error() surfaces only "invalid_blocks"; the actionable per-block reason
	// lives in ResponseMetadata.Messages and would otherwise be dropped.
	assert.Equal(t, sre.ResponseMetadata.Messages, blocksRejectionDetail(sre))
	assert.Nil(t, blocksRejectionDetail(errors.New("network down")), "a non-Slack error carries no block detail")
	assert.Nil(t, blocksRejectionDetail(nil))
}
