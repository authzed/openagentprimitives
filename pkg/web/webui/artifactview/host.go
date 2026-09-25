package artifactview

import (
	"encoding/json"
	"html"

	"github.com/authzed/openagentprimitives/pkg/web/webui"
	"github.com/authzed/openagentprimitives/pkg/web/webui/cspassets"
)

// annotatorCSS styles the annotation chrome (toolbar + review list + note
// popover) that the host bundle builds in the host document. It ships as a
// nonce'd <style> so the classes are authorized under the host page's strict
// style-src 'nonce-X' — the bundle must NOT use inline style="" attributes,
// which a nonce cannot cover. Keep the class names in lockstep with
// ui/host/{toolbar,popover}.ts and the dev harness (web/dev/annotator).
const annotatorCSS = `.ap-annot-toolbar{position:fixed;right:16px;bottom:16px;z-index:2147483647;display:flex;align-items:flex-end;gap:8px;font:13px/1.4 system-ui,sans-serif}
.ap-annot-panel{box-sizing:border-box;min-width:0;max-width:0;opacity:0;overflow:hidden;display:flex;flex-direction:column;gap:8px;background:#111827;color:#fff;padding:0;border-radius:10px;box-shadow:0 2px 10px rgba(0,0,0,.4);transition:max-width .28s ease,opacity .2s ease,padding .28s ease}
.ap-annot-toolbar.ap-annot-open .ap-annot-panel{max-width:320px;opacity:1;padding:10px}
.ap-annot-row{display:flex;align-items:center;gap:8px}
.ap-annot-btn{appearance:none;-webkit-appearance:none;border:0;cursor:pointer;background:#374151;color:#fff;padding:5px 10px;border-radius:6px;font:inherit;line-height:1.4}
.ap-annot-btn:hover{background:#4b5563}
.ap-annot-toggle[aria-pressed='true']{background:#e11d48}
.ap-annot-send{margin-left:auto}
.ap-annot-send:disabled{opacity:.45;cursor:not-allowed}
.ap-annot-fab{flex:none;position:relative;display:flex;align-items:center;justify-content:center;padding:0;width:44px;height:44px;border-radius:50%;background:#e11d48;font:18px/1 system-ui;box-shadow:0 2px 10px rgba(0,0,0,.4)}
.ap-annot-fab:hover{background:#be123c}
.ap-annot-fab-count{position:absolute;top:-4px;right:-4px;min-width:16px;height:16px;line-height:16px;padding:0 4px;border-radius:8px;background:#111827;color:#fff;font-size:11px}
.ap-annot-fab-count[hidden]{display:none}
.ap-annot-count{color:#9ca3af;font-size:12px;white-space:nowrap}
.ap-annot-list{list-style:none;margin:0;padding:0;display:flex;flex-direction:column;gap:4px;max-height:44vh;overflow:auto}
.ap-annot-list:empty{display:none}
.ap-annot-item{display:flex;align-items:center;gap:6px;background:#1f2937;border-radius:6px;padding:4px 6px}
.ap-annot-item-n{flex:none;min-width:16px;height:16px;line-height:16px;text-align:center;border-radius:8px;background:#e11d48;color:#fff;font-size:11px;padding:0 3px}
.ap-annot-item-label{flex:none;max-width:96px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap;color:#cbd5e1}
.ap-annot-item-note{flex:1;min-width:0;overflow:hidden;text-overflow:ellipsis;white-space:nowrap;font-size:12px}
.ap-annot-empty{color:#6b7280;font-style:italic}
.ap-annot-edit,.ap-annot-remove{flex:none;appearance:none;-webkit-appearance:none;border:0;cursor:pointer;background:transparent;color:#9ca3af;font:inherit;padding:2px 4px;border-radius:4px}
.ap-annot-edit:hover,.ap-annot-remove:hover{background:#374151;color:#fff}
.ap-annot-pop{position:fixed;left:16px;bottom:16px;z-index:2147483647;width:300px;max-width:calc(100vw - 32px);display:flex;flex-direction:column;gap:8px;background:#0b1220;color:#fff;padding:12px;border-radius:10px;font:13px/1.4 system-ui,sans-serif;box-shadow:0 4px 18px rgba(0,0,0,.5);border:1px solid #1f2937}
.ap-annot-pop-head{font-size:12px;color:#9ca3af;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
.ap-annot-pop-hint{font-size:11px;color:#6b7280}
.ap-annot-pop textarea{width:100%;box-sizing:border-box;min-height:70px;resize:vertical;background:#111827;color:#fff;border:1px solid #374151;border-radius:6px;padding:6px;font:inherit}
.ap-annot-chips{display:flex;align-items:center;gap:4px;flex-wrap:wrap}
.ap-annot-chips-label{color:#6b7280;font-size:11px;text-transform:uppercase;letter-spacing:.04em;margin-right:2px}
.ap-annot-chip{appearance:none;-webkit-appearance:none;cursor:pointer;border:1px solid #374151;background:transparent;color:#cbd5e1;font:inherit;font-size:12px;padding:2px 8px;border-radius:999px}
.ap-annot-chip[aria-pressed='true']{background:#2563eb;border-color:#2563eb;color:#fff}
.ap-annot-pop-actions{display:flex;justify-content:space-between;gap:8px;margin-top:2px}
.ap-annot-del{background:#7f1d1d}
.ap-annot-del:hover{background:#991b1b}`

// hostPage builds the sandbox-origin host document that the trusted shell frames.
// The host self-embeds the artifact render in an inner iframe that is
// sandbox="allow-same-origin" WITHOUT allow-scripts — so the artifact's own JS
// can never run, structurally — while the host, being same-origin with it, reads
// its DOM and preserves scroll directly. The host's ONLY exit is a postMessage to
// the parent shell; it has no network capability (no connect-src).
//
// The returned csp is the exact Content-Security-Policy header value, carrying
// the same nonce as the injected <style> and <script>, so the browser runs our
// bridge and nothing else.
//
// annotate gates the annotation renderer: when false — the session's class
// doesn't grant the annotation_batch interaction — NEITHER the config global
// (window.__AP_ANNOT) NOR the bundle's <script src> is emitted, so the annotator
// never loads. This is UX only; the real gate is /interact's own session_views
// check, and this just avoids showing a working-looking annotator whose Send
// then fails closed. The swap bridge (hostBridgeJS) is unconditional — the
// read-only live-view works regardless of annotate.
func hostPage(ct, shellOrigin string, annotate bool) (body []byte, csp string, err error) {
	// shellOrigin is caller-controlled, so sanitize it to a bare origin BEFORE it
	// lands in either the CSP frame-ancestors directive or the JS string literal.
	// The CSP sink is a raw, unquoted token: a ';' or ' ' would inject an extra
	// directive (first-directive-wins) or widen frame-ancestors to another host.
	// json.Marshal quotes it safely for the JS sink but does nothing for the CSP
	// sink, so both must use the sanitized value.
	shellOrigin = webui.SanitizeOrigin(shellOrigin)

	// frame-ancestors must never be registered with an EMPTY source list: an
	// empty ancestor-source-list is invalid CSP, so the UA discards the directive
	// as a parse error and leaves THIS page — the one framing agent-generated
	// HTML — embeddable by any origin. Dropping the directive fails identically,
	// since frame-ancestors does not fall back to default-src. So an absent or
	// unusable shell origin denies framing outright rather than granting it.
	// cspassets refuses a sourceless directive (ErrEmptyDirective), but the CHOICE
	// of fail-closed source stays here: only this page knows that "deny" means
	// 'none' for frame-ancestors.
	//
	// An empty shellOrigin is reachable in NORMAL operation, not only on
	// misconfiguration: TrustedOrigin() is webd's externalurl Provider.Get(),
	// which is "" until its first successful ConfigMap poll, and the trusted and
	// sandbox URLs are independent keys with independent pollers — the server
	// keeps serving while only one of them has landed.
	ancestors := shellOrigin
	if ancestors == "" {
		ancestors = "'none'"
	}

	origin, _ := json.Marshal(shellOrigin) // JS string literal, safely quoted
	ctAttr := html.EscapeString(ct)        // ct goes into an HTML attribute value

	// Register the page's assets; cspassets mints one nonce, stamps every tag with
	// it, and derives the CSP from exactly what's registered — so a page without
	// the annotator grants nothing for a bundle it never loads.
	var assets cspassets.Set
	assets.Policy(
		cspassets.Directive("default-src", "'none'"),
		cspassets.Directive("frame-src", "'self'"),
		cspassets.Directive("frame-ancestors", ancestors),
		cspassets.Directive("base-uri", "'none'"),
		cspassets.Directive("form-action", "'none'"),
	)
	// Base layout + the same-origin live-view bridge — always present.
	assets.Style(`html,body{margin:0;height:100%}#ap-artifact{border:0;width:100%;height:100%;display:block;background:#fff}`)
	assets.Script(hostBridgeJS(string(origin)))
	if annotate {
		// Annotator chrome CSS + config global + the sandbox-origin bundle. Styled
		// with CLASSES because the CSP is style-src 'nonce' with no unsafe-inline,
		// which never covers inline style="" — so the rules ship here; pins and
		// outline live in the inner /content frame, whose CSP allows inline styles,
		// and keep positioning inline. The bundle is an external <script src> so it
		// can be built, versioned, and cached independently.
		assets.Style(annotatorCSS)
		assets.Script(`window.__AP_ANNOT={shell:` + string(origin) + `};`)
		assets.ScriptSrc("/artifact-host.js")
	}

	r, err := assets.Render()
	if err != nil {
		return nil, "", err
	}

	// Styles go in <head>; scripts follow the iframe (the bridge reads #ap-artifact
	// on run, so it must exist in the DOM first).
	doc := `<!doctype html><html><head><meta charset="utf-8">` + r.Styles + `</head><body>` +
		`<iframe id="ap-artifact" sandbox="allow-same-origin" src="/content?ct=` + ctAttr + `" title="artifact content"></iframe>` +
		r.Scripts +
		`</body></html>`
	return []byte(doc), r.CSP, nil
}

// hostBridgeJS is the host's same-origin live-view bridge: it swaps and
// scroll-restores the inner frame by DIRECT same-origin access — no postMessage
// to the artifact, no script inside it. The only cross-origin channel is the
// parent shell, origin-pinned to originLit.
func hostBridgeJS(originLit string) string {
	return `(function(){var T=` + originLit + `;var inner=document.getElementById("ap-artifact");` +
		`window.addEventListener("message",function(e){` +
		`if(e.origin!==T||e.source!==window.parent)return;` +
		`var d=e.data;if(!d||d.ap!=="host")return;` +
		`if(d.cmd==="swap"&&typeof d.url==="string"){` +
		`var y=0;try{y=(inner.contentWindow&&inner.contentWindow.scrollY)||0;}catch(_){}` +
		`var onload=function(){inner.removeEventListener("load",onload);try{inner.contentWindow.scrollTo(0,y);}catch(_){}};` +
		`inner.addEventListener("load",onload);inner.setAttribute("src",d.url);}` +
		`});` +
		`try{window.parent.postMessage({ap:"host",cmd:"ready"},T);}catch(_){}})();`
}
