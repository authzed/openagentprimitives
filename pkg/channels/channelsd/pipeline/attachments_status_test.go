package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions/categories"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

// decodeEnvelopes unmarshals every payload fakeNATS recorded as a
// channelevents.Envelope. Bytes that fail to decode are a test bug (every
// publish in this package goes through channelevents.PublishOut), so this
// fails loudly rather than skipping them.
func decodeEnvelopes(t *testing.T, nats *fakeNATS) []channelevents.Envelope {
	t.Helper()
	out := make([]channelevents.Envelope, 0, len(nats.payloads))
	for _, p := range nats.payloads {
		var env channelevents.Envelope
		require.NoError(t, json.Unmarshal(p, &env), "unmarshal published envelope")
		out = append(out, env)
	}
	return out
}

// notificationTexts returns every published KindNotification's payload —
// the ephemeral reading-status surface.
func notificationPayloads(t *testing.T, nats *fakeNATS) []channelevents.NotificationPayload {
	t.Helper()
	var out []channelevents.NotificationPayload
	for _, env := range decodeEnvelopes(t, nats) {
		if env.Kind != channelevents.KindNotification {
			continue
		}
		var pl channelevents.NotificationPayload
		require.NoError(t, json.Unmarshal(env.Payload, &pl), "unmarshal NotificationPayload")
		out = append(out, pl)
	}
	return out
}

// interactionRequestsOfCategory returns every published KindInteractionRequest
// payload matching category — the durable-notice surface.
func interactionRequestsOfCategory(t *testing.T, nats *fakeNATS, category string) []channelevents.InteractionRequestPayload {
	t.Helper()
	var out []channelevents.InteractionRequestPayload
	for _, env := range decodeEnvelopes(t, nats) {
		if env.Kind != channelevents.KindInteractionRequest {
			continue
		}
		var pl channelevents.InteractionRequestPayload
		require.NoError(t, json.Unmarshal(env.Payload, &pl), "unmarshal InteractionRequestPayload")
		if pl.Category == category {
			out = append(out, pl)
		}
	}
	return out
}

// noticeText flattens every user-copy field of a notice payload into one
// string so it can be checked against forbiddenInternalOpsTerms (defined in
// attachments_test.go) in one call, the same as the agent-facing note's own
// tests already do.
func noticeText(pl channelevents.InteractionRequestPayload) string {
	var b strings.Builder
	b.WriteString(pl.Lead)
	b.WriteString(" ")
	b.WriteString(pl.Body)
	b.WriteString(" ")
	b.WriteString(pl.NextStep)
	for _, f := range pl.Fields {
		b.WriteString(" ")
		b.WriteString(f.Label)
		b.WriteString(" ")
		b.WriteString(f.Value)
	}
	return b.String()
}

func TestPluralAttachedFiles(t *testing.T) {
	assert.Equal(t, "1 attached file", pluralAttachedFiles(1))
	assert.Equal(t, "2 attached files", pluralAttachedFiles(2))
	assert.Equal(t, "3 attached files", pluralAttachedFiles(3))
}

// TestProcessAttachments_TwoAudiences: the ephemeral status (machinery-facing:
// "I'm working on this") and the failure notice (outcome-facing: "this didn't
// work") must each appear exactly when they should, and on a mixed batch they
// must agree on WHICH files failed. Drift between the two surfaces means a
// failure the agent's note carries that the human is never told about, or the
// reverse.
func TestProcessAttachments_TwoAudiences(t *testing.T) {
	cases := []struct {
		name string

		classGranted bool
		specEnabled  bool
		attachments  []channelkinds.InboundAttachment
		fetchFunc    func(ctx context.Context, deps channelkinds.Deps, externalID string) (io.ReadCloser, error)
		uploadResult InboundAssetResult

		wantStatusTexts  []string // ephemeral "Reading…" notifications, in order; nil = none
		wantFailureFiles []string // the ONE failure notice's Field labels, in order; nil = no notice
	}{
		{
			name:         "read-only: status published, no failure notice",
			classGranted: true,
			specEnabled:  true,
			attachments:  oneAttachment("F1", "quarterly-report.pdf", "application/pdf", 2048),
			fetchFunc: func(context.Context, channelkinds.Deps, string) (io.ReadCloser, error) {
				return io.NopCloser(strings.NewReader("pdf bytes")), nil
			},
			uploadResult:     InboundAssetResult{Ref: "ref-1", Extracted: true, TextRef: "text-1"},
			wantStatusTexts:  []string{"Reading 1 attached file…"},
			wantFailureFiles: nil,
		},
		{
			name:         "stored but no text extracted: status published, still no failure notice",
			classGranted: true,
			specEnabled:  true,
			attachments:  oneAttachment("F1", "scanned.pdf", "application/pdf", 2048),
			fetchFunc: func(context.Context, channelkinds.Deps, string) (io.ReadCloser, error) {
				return io.NopCloser(strings.NewReader("pdf bytes")), nil
			},
			// Extraction failed or was never configured, but the bytes are
			// stored. Telling the human the file "could not be retrieved right
			// now" would be false — it is on the server, and the runner may
			// well be showing it to the model natively in the very same turn.
			// A scanned, image-only PDF lands here every time.
			uploadResult:     InboundAssetResult{Ref: "ref-1"},
			wantStatusTexts:  []string{"Reading 1 attached file…"},
			wantFailureFiles: nil,
		},
		{
			name:         "no extractor claims the type: status published, still no failure notice",
			classGranted: true,
			specEnabled:  true,
			attachments:  oneAttachment("F1", "screenshot.png", "image/png", 4096),
			fetchFunc: func(context.Context, channelkinds.Deps, string) (io.ReadCloser, error) {
				return io.NopCloser(strings.NewReader("png bytes")), nil
			},
			// The path every image takes: bytes stored, no extractor claims
			// image/*. This is the whole reason outcomeStored exists, and it is
			// excluded from the failure notice on purpose — without that
			// exclusion the human is told an image "could not be retrieved",
			// about a file the model may be looking at natively right now.
			// Distinct from the row above: that one reaches outcomeStored via a
			// FAILED extraction, this one via no extractor existing at all.
			uploadResult:     InboundAssetResult{Ref: "ref-1", Unsupported: true},
			wantStatusTexts:  []string{"Reading 1 attached file…"},
			wantFailureFiles: nil,
		},
		{
			name:         "gate closed: failure notice published, NO status",
			classGranted: false, // capability not granted — the gate leg under test
			specEnabled:  true,
			attachments:  oneAttachment("F1", "notes.txt", "text/plain", 10),
			// fetchFunc left nil: attachFakeKind.FetchAttachment fails the test if
			// invoked at all while the gate is closed.
			wantStatusTexts:  nil,
			wantFailureFiles: []string{"notes.txt"},
		},
		{
			name:         "mixed: both published, and they agree on which file failed",
			classGranted: true,
			specEnabled:  true,
			attachments: []channelkinds.InboundAttachment{
				{ExternalID: "F1", Filename: "good.pdf", MIME: "application/pdf", SizeBytes: 10},
				{ExternalID: "F2", Filename: "bad.pdf", MIME: "application/pdf", SizeBytes: 10},
			},
			fetchFunc: func(_ context.Context, _ channelkinds.Deps, externalID string) (io.ReadCloser, error) {
				if externalID == "F2" {
					return nil, errors.New("kind: download timed out")
				}
				return io.NopCloser(strings.NewReader("bytes")), nil
			},
			uploadResult:     InboundAssetResult{Ref: "ref-1", Extracted: true, TextRef: "text-1"},
			wantStatusTexts:  []string{"Reading 2 attached files…"},
			wantFailureFiles: []string{"bad.pdf"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fk := resetAttachFakeKind(t)
			fk.fetchFunc = tc.fetchFunc

			ch := newAttachChannel("fake-attach", &spiceboxv1alpha1.ChannelAttachmentsSpec{Enabled: tc.specEnabled})
			p, _, mem, nats, _ := newPipeline(t, ch, newAttachClass(tc.classGranted), newAttachSecret())
			mem.uploadResult = tc.uploadResult

			text, _ := p.processAttachments(context.Background(), channelkinds.InboundEvent{
				Channel:     ch,
				Attachments: tc.attachments,
			}, "ac1", "default", "sess1")

			// --- ephemeral status: exactly the expected set, each within
			// Slack's Short length cap, none leaking internal-ops vocabulary.
			statuses := notificationPayloads(t, nats)
			gotStatusTexts := make([]string, 0, len(statuses))
			for _, s := range statuses {
				gotStatusTexts = append(gotStatusTexts, s.Text)
				assert.Less(t, len(s.Short), 50, "Short must be under 50 chars — Slack rejects longer")
				assertNoInternalOpsLeak(t, s.Text)
			}
			if tc.wantStatusTexts == nil {
				assert.Empty(t, gotStatusTexts, "gate closed: nothing is being read, so no status may be published")
			} else {
				assert.Equal(t, tc.wantStatusTexts, gotStatusTexts)
			}
			// --- failure notice: at most one per message, matching the
			// agent-facing note on which files it names.
			//
			// Collected across ALL THREE attachment classes rather than one
			// named category: which class a batch reports under is the thing
			// under test elsewhere, and hardcoding one here would make this
			// test silently stop seeing a batch that correctly moved class.
			// Every case below is single-class, so the "exactly one" claim
			// also guards against the split fragmenting a homogeneous batch.
			var notices []channelevents.InteractionRequestPayload
			for _, cat := range []string{
				categories.AttachmentNotEnabled,
				categories.AttachmentUnreadable,
				categories.AttachmentReadFailed,
			} {
				notices = append(notices, interactionRequestsOfCategory(t, nats, cat)...)
			}
			if len(tc.wantFailureFiles) == 0 {
				assert.Empty(t, notices, "every attachment read: no failure notice expected")
				return
			}
			require.Len(t, notices, 1, "one message's worth of failures must be ONE notice, not one per file")
			pl := notices[0]
			assertNoInternalOpsLeak(t, noticeText(pl))
			require.Len(t, pl.Fields, len(tc.wantFailureFiles), "the notice must name exactly the failed files, not the read ones")
			for i, fn := range tc.wantFailureFiles {
				assert.Equal(t, fn, pl.Fields[i].Label)
				assert.Contains(t, text, fn,
					"the agent-facing note must name the same file the user notice names — a drift here is exactly the bug this test guards against")
			}
		})
	}
}
