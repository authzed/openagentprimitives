package slack

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	slackapi "github.com/slack-go/slack"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

const sessionInstructionsActionID = "view_session_instructions"

func instructionsButton(sessRef, label string, page int) *slackapi.ButtonBlockElement {
	return slackapi.NewButtonBlockElement(sessionInstructionsActionID,
		encodeApprovalButtonValue(discSessionInstructions, strconv.Itoa(page), "", sessRef),
		slackapi.NewTextBlockObject("plain_text", label, false, false))
}

func openingBlocks(summary, sessRef string) []slackapi.Block {
	return []slackapi.Block{
		slackapi.NewSectionBlock(slackapi.NewTextBlockObject("plain_text", summary, false, false), nil, nil),
		slackapi.NewActionBlock("", instructionsButton(sessRef, "View exact instructions", 0)),
	}
}

func (s *slackSender) sendSessionOpening(ctx context.Context, sess channelkinds.SessionInfo, channelID, threadTS string, opening channelevents.SessionOpening) (channelkinds.SubChannelSendResult, error) {
	if opening.Summary == "" || opening.Instructions == "" {
		return channelkinds.SubChannelSendResult{}, fmt.Errorf("session opening requires a summary and exact instructions")
	}
	opts := []slackapi.MsgOption{slackapi.MsgOptionText(opening.Summary, false), slackapi.MsgOptionBlocks(openingBlocks(opening.Summary, sess.Namespace+"/"+sess.Name)...)}
	if threadTS != "" {
		opts = append(opts, slackapi.MsgOptionTS(threadTS))
	}
	ch, ts, err := s.client.PostMessageContext(ctx, channelID, opts...)
	if err != nil {
		return channelkinds.SubChannelSendResult{}, fmt.Errorf("post session opening: %w", err)
	}
	result := channelkinds.SubChannelSendResult{OpeningMessage: &channelkinds.MessageRef{ChannelID: ch, TS: ts}}
	if threadTS == "" {
		result.External = map[string]string{"channel_id": ch, "thread_ts": ts}
	}
	return result, nil
}

// Each page uses at most 40 plain-text sections below Slack's 3000-character
// limit. Paging exposes every rune without truncating or interpreting markup.
func instructionModal(text, sessRef string, page int) slackapi.ModalViewRequest {
	const chunkSize, chunksPerPage = 2800, 40
	runes := []rune(text)
	pageSize := chunkSize * chunksPerPage
	pages := (len(runes) + pageSize - 1) / pageSize
	if pages == 0 {
		pages = 1
	}
	if page < 0 || page >= pages {
		page = 0
	}
	start, end := page*pageSize, min((page+1)*pageSize, len(runes))
	var blocks []slackapi.Block
	for i := start; i < end; i += chunkSize {
		blocks = append(blocks, slackapi.NewSectionBlock(slackapi.NewTextBlockObject("plain_text", string(runes[i:min(i+chunkSize, end)]), false, false), nil, nil))
	}
	blocks = append(blocks, slackapi.NewContextBlock("", slackapi.NewTextBlockObject("plain_text", fmt.Sprintf("Page %d of %d · %s", page+1, pages, sessRef), false, false)))
	var buttons []slackapi.BlockElement
	if page > 0 {
		button := instructionsButton(sessRef, "Previous page", page-1)
		button.ActionID += "_previous"
		buttons = append(buttons, button)
	}
	if page+1 < pages {
		button := instructionsButton(sessRef, "Next page", page+1)
		button.ActionID += "_next"
		buttons = append(buttons, button)
	}
	if len(buttons) > 0 {
		blocks = append(blocks, slackapi.NewActionBlock("", buttons...))
	}
	return slackapi.ModalViewRequest{Type: slackapi.VTModal, Title: slackapi.NewTextBlockObject("plain_text", "Exact instructions", false, false), Close: slackapi.NewTextBlockObject("plain_text", "Close", false, false), Blocks: slackapi.Blocks{BlockSet: blocks}}
}

type viewUpdater interface {
	UpdateViewContext(context.Context, slackapi.ModalViewRequest, string, string, string) (*slackapi.ViewResponse, error)
}

func (l *slackListener) handleSessionInstructionsAction(ctx context.Context, cb slackapi.InteractionCallback) (bool, error) {
	for _, a := range cb.ActionCallback.BlockActions {
		if a.ActionID != sessionInstructionsActionID && a.ActionID != sessionInstructionsActionID+"_previous" && a.ActionID != sessionInstructionsActionID+"_next" {
			continue
		}
		v, ok := decodeApprovalButtonValue(a.Value)
		if !ok || v.V != discSessionInstructions {
			return true, fmt.Errorf("invalid session instructions button")
		}
		ns, name, valid := strings.Cut(v.S, "/")
		text := "Exact instructions are unavailable."
		var readErr error
		if !valid || ns == "" || name == "" || l.deps.K8sClient == nil || l.deps.AuthzReader == nil {
			readErr = fmt.Errorf("session or instruction reader unavailable")
		} else {
			canonical, err := l.resolveCanonicalForSlackUser(ctx, cb.User.ID)
			if err != nil {
				readErr = err
			} else if canonical == "" {
				readErr = fmt.Errorf("clicker identity unavailable")
			} else {
				owner := identity.CanonicalFromTrusted(strings.TrimPrefix(canonical, "user:"), "Slack-signed instruction-view click")
				allowed, err := l.deps.AuthzReader.CheckInteract(ctx, ns, name, owner, true)
				if err != nil {
					readErr = err
				} else if !allowed {
					readErr = fmt.Errorf("session access denied")
				} else {
					var sess v1.AgentSession
					if err := l.deps.K8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &sess); err != nil {
						readErr = err
					} else if sess.Spec.Prompt.Inline == "" {
						readErr = fmt.Errorf("inline instructions unavailable")
					} else {
						text = sess.Spec.Prompt.Inline
					}
				}
			}
		}
		if readErr != nil {
			log.FromContext(ctx).Info("session instructions unavailable", "session", v.S, "clicker", cb.User.ID, "err", readErr)
		}
		page, err := strconv.Atoi(v.R)
		if err != nil {
			return true, fmt.Errorf("invalid instructions page: %w", err)
		}
		view := instructionModal(text, v.S, page)
		if cb.View.ID != "" {
			updater, ok := l.api.(viewUpdater)
			if !ok {
				return true, fmt.Errorf("instruction page updates unavailable")
			}
			_, err = updater.UpdateViewContext(ctx, view, "", cb.View.Hash, cb.View.ID)
		} else {
			_, err = l.api.OpenViewContext(ctx, cb.TriggerID, view)
		}
		if err != nil {
			return true, fmt.Errorf("show exact instructions: %w", err)
		}
		return true, nil
	}
	return false, nil
}
