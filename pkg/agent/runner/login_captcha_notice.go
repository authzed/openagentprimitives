package runner

// login_captcha_notice.go implements the login_captcha notice producer: the
// one-way FYI a browser toolbox's request_human_step surfaces when the agent hits
// a step it cannot complete on its own (a CAPTCHA, or any challenge with no
// actionable kind). Unlike login_human_step it has NO decision leg — there is no
// Done button to resume the agent; a person takes over in the browser out of
// band. Mirrors completionbypass.go's notice shape: fixed TRUSTED copy, the
// model-authored prompt carried inert in Excerpt, delivered as an addressee-less
// participants broadcast.

import (
	"context"
	"fmt"
	"strings"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions/categories"
	"github.com/authzed/openagentprimitives/pkg/channels/notice"
	"github.com/authzed/openagentprimitives/pkg/x/stringsx"
)

// maxCaptchaPromptRunes caps the model-authored prompt rendered in the notice's
// inert Excerpt.
const maxCaptchaPromptRunes = 500

// publishLoginCaptchaNotice builds + publishes the login_captcha notice on the
// session's OUT subject via the generic interaction_request path (same as
// leakage_notice.go). Best-effort at the call site: a session with no channel
// (kubectl-driven) has no InteractionRequestPublish wired, which is reported as
// an error here and surfaced by request_human_step to the model rather than
// swallowed.
func (l *Loop) publishLoginCaptchaNotice(ctx context.Context, prompt string) error {
	if l.InteractionRequestPublish == nil {
		return fmt.Errorf("login captcha notice unavailable (interaction publish hook not configured)")
	}
	sessNS, sessName := l.SessionKey.Namespace, l.SessionKey.Name
	sess := channelevents.SessionRef{Namespace: sessNS, Name: sessName}

	// Lead/Body/NextStep are fixed TRUSTED copy (rendered as live markup). The
	// model-authored prompt is untrusted and travels in Excerpt, rendered inert.
	// NextStep is mandatory for ToneUnavailable (notice.Payload enforces it).
	n := notice.New(categories.LoginCaptcha, notice.Args{
		Lead:     "A sign-in step needs a person",
		Body:     "The agent reached a step it can't complete on its own — a CAPTCHA or similar challenge — and needs someone to take over in the browser session.",
		NextStep: "Complete the step in the browser, then ask the agent to continue.",
		Excerpt:  captchaNoticeExcerpt(prompt),
		Audience: channelevents.InteractionAudience{Scope: channelevents.AudienceParticipants},
	})
	pl, err := n.Payload(sess, newRequestID())
	if err != nil {
		return fmt.Errorf("build login captcha notice: %w", err)
	}
	env, err := channelevents.BuildEnvelope(sessNS, sessName, channelevents.KindInteractionRequest, pl)
	if err != nil {
		return fmt.Errorf("build login captcha notice envelope: %w", err)
	}
	return l.InteractionRequestPublish(ctx, sessNS, sessName, env)
}

// captchaNoticeExcerpt carries the model-authored prompt inert, rune-capped. An
// empty prompt yields no excerpt.
func captchaNoticeExcerpt(prompt string) *channelevents.InteractionExcerpt {
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return nil
	}
	return &channelevents.InteractionExcerpt{
		Label:   "What the agent reported",
		Content: stringsx.CapRunes(prompt, maxCaptchaPromptRunes),
	}
}
