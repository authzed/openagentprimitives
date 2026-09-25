# `slack` — the Slack channel kind

The largest channel kind, and the reference implementation for the optional
capability interfaces in [`../kind.go`](../kind.go). Inbound arrives over
Socket Mode; outbound goes through the Slack Web API as Block Kit messages.

72 files, grouped by concern:

| Concern | Files |
| ------- | ----- |
| **Core** | `kind.go`, `listener.go`, `listener_client.go`, `sender.go`, `sender_helpers.go`, `socket_source.go` |
| **Senders** | `sender_chunk.go`, `sender_plan.go`, `sender_plan_legacy.go`, `sender_tool_progress.go`, `sender_tool_session.go`, `sender_turn_progress.go`, `sender_user_echo.go`, `sender_agent_ui_offer.go`, `sender_live_view_offer.go`, `sender_session_view_offer.go`, `stream_delta_sink.go` |
| **Interactions** | `interaction.go`, `interaction_applied.go`, `interaction_delivery.go`, `interaction_details.go`, `interaction_fanout.go`, `interaction_notice.go`, `interaction_public_note.go`, `interaction_resurface_interrupt.go`, `interaction_ticker.go`, `notice_post.go`, `url_offer_blocks.go` |
| **Threading** | `anchor.go`, `thread_resolve.go`, `thread_title.go`, `starter_cache.go`, `fork_root.go`, `restart.go`, `restart_modal.go`, `response_url.go` |
| **Identity** | `identity.go`, `lookup.go`, `mention_lookup.go`, `oidc.go`, `resolve_canonical.go`, `userprofile.go`, `audience_resolver.go` |
| **App Home** | `app_home.go`, `app_home_view.go`, `capability_screen.go`, `show_settings.go` |
| **Metaagent** | `metaagent_listener.go`, `metaagent_sender.go`, `metaagent_callback.go`, `metaagent_membership.go`, `metaagent_approval_refs.go` |
| **Block Kit plumbing** | `block_container.go`, `blocks_degrade.go`, `buttoncodec.go`, `messagecontent.go`, `textformat.go`, `tool_session_render.go`, `inert.go` |
| **Setup** | `wizard.go`, `wizard_monitoring.go`, `provision_screen.go`, `manifest.go`, `scopes.go`, `features.go`, `schema_fragment.go` |
| **Other** | `attachments.go`, `channel_history.go`, `history.go`, `monitoring.go`, `inbound_observability.go`, `testhooks.go` |

| Package | What it holds |
| ------- | ------------- |
| [`appprovision/`](appprovision/) | Creates and installs the Slack app from the wizard's manifest, without a browser round trip |
| [`fakeslack/`](fakeslack/) | In-memory Slack Web API simulator for tests |

## Non-obvious constraints

- **Button `value` discriminators are wire values.** A button posted before a
  rollout is still clickable after it, so changing a discriminator string
  silently breaks every message already in a channel. Encode and decode both
  live in `buttoncodec.go` for that reason — see its doc comment before
  touching one.
- **Block Kit rejection is expected, and there is one allowlist for it.** Two
  renderers (`sender_plan.go`'s task cards, `interaction_notice.go`'s
  container) use primitives a workspace or SDK may refuse. `blocks_degrade.go`
  answers "did Slack reject the *shape*?" for both — add a rejection code
  there, not in a renderer.
- **`schema_fragment.go` is raw `.zed`, not the structured form.** The Slack
  hierarchy needs subject-relations (`slack_user#user`), which
  `SpiceDBResource` cannot express.
- **App Home re-publishes on every `app_home_opened`, with no cache.** Slack's
  per-app per-user view *is* the cache; a local layer would only add staleness.
  Views cap at 100 blocks (~18 agent cards); beyond that the view truncates with
  a "+N more" rather than failing.
- **`appprovision` depends on an undocumented endpoint.** `apps.developerInstall`
  is the only known source of an app-level `xapp-` token, which Socket Mode
  requires. The wizard's manual path stays precisely so there is a fallback if
  Slack changes it — it is not dead code.
