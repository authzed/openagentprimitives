# OAP brand marks

The Open Agent Primitives logo files, from the 2026-09-23 finals. Every mark
ships as SVG and as a 2x PNG, in two inks.

## Light and dark name the ink, not the background

| Variant  | Ink       | Use it on         |
| -------- | --------- | ----------------- |
| `-dark`  | `#1D1423` | light backgrounds |
| `-light` | `#F1F0F2` | dark backgrounds  |

This catches people out: the `-light` file is the one for a dark page.

## Which mark

| File              | What it is                                                                                                          | Use it for                                                       |
| ----------------- | ------------------------------------------------------------------------------------------------------------------- | ---------------------------------------------------------------- |
| `oap-icon-*`      | The triangle with its cut-out                                                                                       | Square slots: favicons, avatars, app tiles                       |
| `oap-menu-icon-*` | The icon tuned for small sizes (a larger cut-out, 43% of the triangle against the icon's 34%, so the hole survives) | Only below about 24px: menu bars, tray icons                     |
| `oap-logomark-*`  | O, A and P drawn as a circle, a triangle and a bowl                                                                 | Wide slots where the name is already on the page                 |
| `oap-wordmark-*`  | The logomark with "Open Agent Primitives" set beside it                                                             | Headers and anywhere the logo has to name the project on its own |

## Copies of the geometry

UI code does not load these files. The shapes are inlined and painted with
`currentColor`, so one path serves both themes. When the marks change, update
the inlined copies too:

| Where                                                    | What it holds                                                                                                                                      |
| -------------------------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------- |
| `web/packages/design/src/components/ui/oap-mark.tsx`     | `OapMark` (icon), `OapLogomark`, and the relay outline and hole. The admin UI imports these; rebuild the committed bundle with `mage web:build`.   |
| `pkg/web/webui/document.go`                              | `FaviconHref`, the icon as a data-URI favicon                                                                                                      |
| `pkg/platform/identityd/handlers_password.go`            | The icon on the sign-in page                                                                                                                       |
| `cmd/oap/internal/desktop/setupui/static/index.html`     | The logomark in the desktop setup header, plus the icon                                                                                            |
| `cmd/oap/internal/desktop/menubaricons/render/tunnel.go` | Not a copy: the menu-bar icons are drawn in code. `menuMarkCutout` carries the menu icon's cut-out; regenerate the PNGs with `mage desktop:icons`. |

The repo README and the showcase docs site (`showcase/docs/app`) reference the
files here directly.
