# Slack behaviours this package works around

Everything below was established by **posting to a real workspace** and reading
the API's response, not by reading Slack's reference docs. Each one is either
absent from those docs or contradicts them, and each one shapes code in this
package.

If you are about to "simplify" something here and it looks gratuitous, check
this file first — most of the odd-looking constructions exist because the
obvious version is rejected or silently renders wrong.

Verified 2026-07-31 against `slack-go v0.23.0`. Slack changes this surface
(alert/card/carousel landed April 2026, container in June), so re-verify before
trusting an entry that blocks something you want.

---

## Block availability

| Block                                                   | In a message? | Notes                                                                                                                                                                                                               |
| ------------------------------------------------------- | ------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `alert`                                                 | **No**        | `Unsupported block type: alert`. Modal-only, despite being Slack's purpose-built severity block with exactly the levels you'd want. Usable in `views.open` — which is why Show-Details modals are where it belongs. |
| `container`                                             | **Yes**       | Not in `slack-go` through v0.27.0 — hand-rolled in `block_container.go`.                                                                                                                                            |
| `card`, `carousel`                                      | Yes           | `card.body` caps at 200 chars and has **no colour field**, so severity would fall back to an emoji in the title. Not used here.                                                                                     |
| `section`, `context`, `divider`, `actions`, `rich_text` | Yes           | The long-stable set.                                                                                                                                                                                                |

## Colour and structure are mutually exclusive

There is **no message-surface severity primitive**. `attachments[].color` is the
only way to get a colour, and:

- A `container` inside an attachment is **rejected**:
  `Unsupported block type: container [/attachments/0/blocks/0]`.
- Top-level `blocks` render **above** `attachments`, so "coloured bar, then
  structured detail" cannot be ordered correctly even as siblings.
- Every attachment carries an **"Added by \<app\>" footer**. Two attachments
  means two footers.
- `footer` and `blocks` cannot coexist on one attachment: `invalid_keys`. So you
  cannot replace that footer with your own.

Consequence: this package uses top-level containers and encodes tone as an emoji
chip. Colour was the first choice and is not available.

## Container quirks

- **`has_header_divider` requires `is_collapsible: false`.** Setting both fails
  the whole `chat.postMessage` with `invalid_blocks`. `newContainerBlock` /
  `newCollapsibleContainerBlock` own this pair so the combination cannot be
  expressed.
- **`title` does not render emoji.** A `plain_text` title containing an emoji
  prints `:large_red_square:` literally — _even with `emoji: true`_. Only
  `rich_text_title` with a `{"type":"emoji"}` element renders one. This is why
  every notice builds a rich-text title.
- **`subtitle` renders emoji but drops mrkdwn code spans**, so a session ref
  there shows visible backticks. Provenance goes in a trailing `context` child
  instead.
- **`slack_icon` is not a container property** — `invalid additional property`.
  It belongs to `card`. There is no icon slot; the chip rides the title.
- Allowed `child_blocks`: `actions`, `context`, `divider`, `file`, `header`,
  `image`, `input`, `rich_text`, `section`, `table`, `video`. Max 10. Notably
  **not** `alert`, and **not** `container` — containers do not nest.

## Rich text

- **Ordinary spaces collapse to one**, even as their own element. `"  "` and
  `" "` render identically. Only **U+00A0** holds a wider gap — see
  `chipSpacer`.
- Leading whitespace inside a styled element is trimmed outright, which is why
  the chip/lead spacer is a separate element rather than a prefix on the lead.

## Emoji names are not validated

`chat.postMessage` **accepts an unknown emoji name** and renders it as literal
`:name:` text. There is no error, no warning, and nothing in the response to
check.

This is why `channelinteractions.Glyph` is a closed typed set rather than a
string a caller passes: a typo in a string field would ship and only be visible
to whoever happened to read the message.

## Canvases

Canvas _callouts_ — the coloured admonition blocks — are a canvas text feature.
**Block Kit is not supported in canvases at all**, so there is no path from a
message to a callout.

---

## Re-verifying

To re-check any entry, POST the JSON to `chat.postMessage` with a bot token and
read `response_metadata.messages` — Slack's rejections name the offending
`json-pointer`, which is far more informative than the docs.

Send probes to a DM with yourself rather than a shared channel.
