package livemirror

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/delivery"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/replydelivery"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/turn"
	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"
)

func TestHistoryReplaysOnlyAcceptedRepliesWithoutPublicationAcknowledgement(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		hasReceipt, published bool
	}{
		{"pending approval hides proposal", false, false},
		{"accepted body without acknowledgement", true, false},
		{"receipt and publication shown once", true, true},
		{"legacy published reply remains visible", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Now().UTC()
			p := channelevents.OutboundUserMessagePayload{Text: "Stand up and stretch!"}
			var err error
			p.Delivery, err = channelevents.NewDeliveryOperation("root-uid", "call", p)
			require.NoError(t, err)
			i := delivery.Intent{Session: channelevents.SessionRef{Namespace: testScope, Name: "root"}, Payload: p, CreatedAt: now, Destination: delivery.Destination{Kind: "browser", ChannelUID: "channel", BindingDigest: "pin", Recipient: "owner"}}
			digest, err := i.Destination.Digest()
			require.NoError(t, err)
			c := replydelivery.Content{Intent: i, Receipt: delivery.Receipt{OperationID: p.Delivery.ID, SessionUID: "root-uid", PayloadDigest: p.Delivery.PayloadDigest, DestinationDigest: digest, Transport: "browser-transcript", Reference: "accepted-row", AcceptedAt: now.Add(time.Second)}}
			accepted, err := json.Marshal(c)
			require.NoError(t, err)
			proposal, err := json.Marshal(map[string]any{"content": []memory.ContentBlock{{Type: "text", Text: "internal preamble"}, {Type: "tool_use", ToolUse: &memory.ToolUseBlock{ID: "call", Name: "respond_to_user", Input: json.RawMessage(`{"text":"Model proposal differs from the accepted wire body"}`)}}}})
			require.NoError(t, err)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				entries := []memory.Entry{}
				if strings.Contains(r.URL.Path, "/reply_delivery/") {
					if tc.hasReceipt {
						entries = append(entries, memory.Entry{Kind: replydelivery.KindName, Content: accepted, Provenance: &memory.Provenance{Publisher: "system:operator"}})
					}
				} else {
					entries = append(entries, memory.Entry{Kind: turn.KindName, ID: turn.EntryID(1, "assistant"), Content: proposal, CreatedAt: now})
					if tc.published {
						raw, err := json.Marshal(map[string]any{"content": []memory.ContentBlock{{Type: "text", Text: `{"delivered":["call"]}`}}})
						require.NoError(t, err)
						entries = append(entries, memory.Entry{Kind: turn.KindName, ID: turn.EntryID(2, "system_note"), Content: raw, CreatedAt: now.Add(time.Second)})
					}
				}
				w.Header().Set("Content-Type", "application/json")
				require.NoError(t, json.NewEncoder(w).Encode(memory.QueryResult{Entries: entries}))
			}))
			defer srv.Close()
			history, err := ReadHistory(context.Background(), srv.URL, "read-only-token", testScope, "root", logr.Discard())
			require.NoError(t, err)
			if !tc.hasReceipt && !tc.published {
				require.Empty(t, history.Timeline)
				return
			}
			require.Len(t, history.Timeline, 1)
			if !tc.hasReceipt {
				require.Equal(t, "Model proposal differs from the accepted wire body", history.Timeline[0].Text)
				require.Empty(t, history.Timeline[0].OperationID)
				return
			}
			require.Equal(t, p.Text, history.Timeline[0].Text)
			require.Equal(t, p.Delivery.ID, history.Timeline[0].OperationID)
		})
	}
}
