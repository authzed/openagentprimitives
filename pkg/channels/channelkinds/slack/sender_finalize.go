package slack

import (
	"context"
	"errors"
	"fmt"
	"slices"

	slackapi "github.com/slack-go/slack"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

var _ channelkinds.SessionFinalizer = (*slackSender)(nil)

// FinalizeSession takes down the "still working" state this sender rendered for
// a session that has ended. The status caption itself is dismissed by
// channelsd's empty-Notification clear (sendNotification's sentinel); what
// only this sender can reach is the plan cards, whose in-progress item keeps
// its hourglass until something re-renders the card. The runner does that on a
// normal exit (a PauseCauseStopped/Failed snapshot), but a session the operator
// ended without it — a boot failure, a crashed or evicted runner — leaves
// every card reading "working" on a dead session.
//
// So each card still showing an in-progress item, and not already paused, is
// re-rendered from its last payload as paused with a stopped banner. Cards that
// are finished, or that the runner already paused, are left exactly as they
// are. Every card for the session is dropped from the map either way.
//
// Plan cards are tracked in process memory, so a card posted before a channelsd
// restart is beyond reach here; that is the accepted limit of a best-effort
// cleanup the runner normally does itself.
func (s *slackSender) FinalizeSession(ctx context.Context, sess channelkinds.SessionInfo, failed bool) error {
	s.forgetWatchdog(sess)
	cause := channelevents.PauseCauseStopped
	if failed {
		cause = channelevents.PauseCauseFailed
	}
	var errs []error
	for _, card := range s.planMessages.takeSession(sess) {
		if card.last == nil || card.last.Paused || card.channelID == "" ||
			!slices.ContainsFunc(card.last.Items, func(it channelevents.PlanItemRef) bool { return it.Status == "in_progress" }) {
			continue
		}
		pl := *card.last
		pl.Paused, pl.PauseCause = true, cause
		_, _, _, err := s.client.UpdateMessageContext(ctx, card.channelID, card.ts,
			slackapi.MsgOptionBlocks(renderPlanBlocks(pl)...))
		if err != nil && isUnsupportedBlocksErr(err) {
			_, _, _, err = s.client.UpdateMessageContext(ctx, card.channelID, card.ts,
				slackapi.MsgOptionBlocks(renderPlanBlocksLegacy(pl)...))
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("slack sender: finalize plan %q: %w", pl.PlanName, err))
		}
	}
	return errors.Join(errs...)
}
