// pkg/channels/channelkinds/slack/textformat.go
//
// Slack's markup dialect, as instructions to the model. It lives with the
// channel kind, not in the generic respond tool: formatting is the transport's
// business.
package slack

// slackTextFormattingInstructions is the model-facing description of Slack's
// mrkdwn dialect, appended to respond_to_user's `text` property.
//
// It says what mrkdwn is NOT as loudly as what it is, because the failure it
// prevents is silent on our side and glaring on the user's: a model that
// defaults to CommonMark emits `**bold**`, `# headings` and `[label](url)`,
// all of which Slack renders as the literal characters. Nothing downstream
// can repair that — the sender posts what the model wrote.
const slackTextFormattingInstructions = "Use Slack mrkdwn (NOT CommonMark/GitHub Markdown). " +
	"Supported: *bold* (single asterisk), _italic_ (single underscore), ~strike~, " +
	"`inline code`, ```code block```, > blockquote (no leading space), " +
	"<https://url|label> for links, <@USERID> for user mentions, <#CHANNELID|name> for channel refs. " +
	"NOT supported: headings (# ## ###), **double-asterisk bold**, " +
	"[label](url) link syntax, numbered or bulleted lists (Slack renders them literally). " +
	"For lists, prefix lines with •, -, or numbers manually."

// TextFormattingInstructions implements channelkinds.TextFormatter.
func (*Kind) TextFormattingInstructions() string { return slackTextFormattingInstructions }
