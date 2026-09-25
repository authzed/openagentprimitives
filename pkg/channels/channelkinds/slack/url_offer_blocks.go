package slack

import (
	slackapi "github.com/slack-go/slack"
)

// buildURLOfferBlocks renders the "here is a link, open it in your browser"
// offer shape both durable-link offer senders share: a markdown section
// prompt above an action block holding exactly one primary-styled button
// that carries the URL directly.
//
// It is deliberately NOT used by liveViewOfferSender: that button carries no
// URL and is minted fresh on each click, so routing it through a
// URL-required builder would mean inventing a meaningless URL argument.
func buildURLOfferBlocks(actionBlockID, actionID, headlineMarkdown, buttonLabel, url string) []slackapi.Block {
	btn := slackapi.NewButtonBlockElement(
		actionID,
		"",
		slackapi.NewTextBlockObject(slackapi.PlainTextType, buttonLabel, false, false),
	).WithURL(url).WithStyle(slackapi.StylePrimary)

	return []slackapi.Block{
		slackapi.NewSectionBlock(
			slackapi.NewTextBlockObject(slackapi.MarkdownType, headlineMarkdown, false, false),
			nil, nil,
		),
		slackapi.NewActionBlock(actionBlockID, btn),
	}
}
