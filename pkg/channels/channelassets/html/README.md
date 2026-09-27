# `html` — the HTML artifact renderer

Sanitizes agent-authored HTML with
[bluemonday](https://github.com/microcosm-cc/bluemonday) under a custom policy,
injects a restrictive Content-Security-Policy `<meta>` tag, and emits diff-based
warnings naming every element and attribute it dropped.

Also the composition target for `DeliveryBundledOnly` kinds: their previews are
built as HTML and rendered here.

| File          | Role                                                                                    |
| ------------- | --------------------------------------------------------------------------------------- |
| `renderer.go` | The `channelassets.Renderer` implementation — policy, sanitize, CSP injection, warnings |
| `css.go`      | `<style>` elements and `style="..."` attributes, through the shared sanitizer           |
| `refs.go`     | `channelassets.RefRewriter` — rewrites asset references in the sanitized document       |
| `serve.go`    | `ServeTransform` — a deliberate no-op                                                   |
| `init.go`     | Registry registration                                                                   |

## Non-obvious constraints

- **CSS sanitization is shared, not local.** Both `<style>` blocks here and the
  standalone stylesheets the `css` kind renders go through
  [`../internal/csssanitize`](../internal/csssanitize/). Sharing it is what
  keeps the two kinds' denylist semantics identical — do not fork it.
- **`ServeTransform` is intentionally inert.** Under the live-view's
  nested-frame isolation the artifact is the inner, script-disabled frame
  (`sandbox="allow-same-origin"`, no `allow-scripts`) of a host page that owns
  all live-view behavior. Nothing is injected and the baked meta-CSP is served
  unchanged; the structural no-`allow-scripts` frame is the real guarantee.
