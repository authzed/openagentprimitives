package slack

import (
	"context"
	"fmt"
	"io"
	"net/http"

	slackapi "github.com/slack-go/slack"

	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

var _ channelkinds.AttachmentFetcher = (*Kind)(nil)

// slackMaxAttachmentSizeBytes is Slack's practical per-file ceiling (the
// upload limit on a paid workspace). AttachmentBounds advertises it so a
// caller with its own configured per-Channel ceiling can clamp to whichever is
// smaller; it is not itself enforced here — FetchAttachment streams whatever
// the download responds with.
const slackMaxAttachmentSizeBytes int64 = 1 << 30 // 1 GiB

// slackMaxAttachmentsPerMessage bounds how many files from one inbound
// message this kind will fetch, independent of any per-channel setting.
const slackMaxAttachmentsPerMessage = 10

// AttachmentBounds implements channelkinds.AttachmentFetcher.
func (k *Kind) AttachmentBounds() channelkinds.AttachmentBounds {
	return channelkinds.AttachmentBounds{
		MaxSizeBytes:  slackMaxAttachmentSizeBytes,
		MaxPerMessage: slackMaxAttachmentsPerMessage,
	}
}

// attachmentInfoClient is the slack-go subset FetchAttachment needs to
// resolve a Slack file ID to its private download URL via files.info. A
// separate interface from historyClient/slackClient — files.info is used
// nowhere else in this package.
type attachmentInfoClient interface {
	GetFileInfoContext(ctx context.Context, fileID string, count, page int) (*slackapi.File, []slackapi.Comment, *slackapi.Paging, error)
}

// attachmentInfoClientFactory builds an attachmentInfoClient from Deps.
// Overridden directly in unit tests (attachments_test.go's
// installAttachmentInfoClient) to point files.info at an httptest.Server
// instead of slack.com, mirroring historyClientFactory.
//
// When a test has installed a shared fake via InstallTestTransport, it must
// back files.info too — the fake structurally satisfies attachmentInfoClient
// (see fakeslack_seam_test.go's compile-time proof), so the comma-ok
// assertion only fails if a future override value doesn't implement it.
// Without this check, the e2e harness's real-slack-kind scenarios would
// dial actual slack.com for FetchAttachment's first leg and fail closed
// with a network error on every attachment — mirrors newSlackAPIClient
// (sender.go) and newSlackAPIClientFull (listener_client.go), the two
// existing testClientOverride consumers.
var attachmentInfoClientFactory = func(deps channelkinds.Deps) attachmentInfoClient {
	if testClientOverride != nil {
		if c, ok := testClientOverride.(attachmentInfoClient); ok {
			return c
		}
	}
	if deps.Secret == nil {
		return nil
	}
	tok, ok := deps.Secret.Data[SecretKeyBotToken]
	if !ok || len(tok) == 0 {
		return nil
	}
	return slackapi.New(string(tok))
}

// FetchAttachment implements channelkinds.AttachmentFetcher. externalID is
// the Slack file ID recorded as InboundAttachment.ExternalID at listener
// time (see slackInboundAttachments in listener.go).
//
// Two requests are involved, deliberately kept distinct: files.info
// resolves the ID to a private download URL (via the slack-go client, same
// as ReadHistory's conversations.replies), then a plain HTTP GET against
// that URL — url_private_download is Slack's file CDN, not a Web API
// method, and rejects an unauthenticated request, so the bot token is set
// as an Authorization: Bearer header on this second request specifically.
// The response body is returned unread: the caller streams and closes it,
// so an arbitrarily large attachment is never buffered in memory here.
func (k *Kind) FetchAttachment(ctx context.Context, deps channelkinds.Deps, externalID string) (io.ReadCloser, error) {
	if deps.Secret == nil {
		return nil, fmt.Errorf("slack FetchAttachment: no credentials Secret")
	}
	tok, ok := deps.Secret.Data[SecretKeyBotToken]
	if !ok || len(tok) == 0 {
		return nil, fmt.Errorf("slack FetchAttachment: no API client (credentials Secret missing bot-token?)")
	}
	cli := attachmentInfoClientFactory(deps)
	if cli == nil {
		return nil, fmt.Errorf("slack FetchAttachment: no API client (credentials Secret missing bot-token?)")
	}

	file, _, _, err := cli.GetFileInfoContext(ctx, externalID, 0, 0)
	if err != nil {
		return nil, fmt.Errorf("slack files.info(%s): %w", externalID, err)
	}
	if file == nil || file.URLPrivateDownload == "" {
		return nil, fmt.Errorf("slack FetchAttachment: file %s has no private download URL", externalID)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, file.URLPrivateDownload, nil)
	if err != nil {
		return nil, fmt.Errorf("slack FetchAttachment: building download request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+string(tok))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("slack FetchAttachment: download request: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Never leave the connection open on the error path — the caller has
		// no reader to close in this branch.
		resp.Body.Close()
		return nil, fmt.Errorf("slack FetchAttachment: download returned %s", resp.Status)
	}
	return resp.Body, nil
}
