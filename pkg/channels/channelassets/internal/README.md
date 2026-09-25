# `channelassets/internal`

Sanitizer internals shared between renderer kinds. Not importable outside
`pkg/channels/channelassets`.

| Package                        | What it holds                   |
| ------------------------------ | ------------------------------- |
| [`csssanitize/`](csssanitize/) | The shared CSS string sanitizer |

`csssanitize` is used by the [`html`](../html/) renderer (for `<style>` elements
and `style="..."` attributes) and by the [`css`](../css/) renderer (for
standalone stylesheets). Sharing it is what keeps the two kinds' denylist
semantics identical — a second copy would drift silently and the drift would be
a sanitizer bypass.
