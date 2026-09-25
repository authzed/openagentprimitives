import { AlertCircle, AlertTriangle, CheckCircle2, Clock, CircleDollarSign, Info, Lock } from "lucide-react";
import { cn } from "@ap/design";
import { CardExcerpt, CardFields } from "./cardParts";
import type { NoticeWire } from "./types";

// toneStyle maps a NoticeWire.tone (channelinteractions.Tone, denormalised
// onto the wire precisely so a surface needs no registry) to this surface's
// icon + colour.
//
// The tones are NOT an ordered severity — privacy is not "between" degraded
// and critical, it is a different question (see pkg/channels/channelinteractions/
// tone.go) — so this is a lookup, not a ramp: privacy gets its own lock mark
// rather than a hotter or cooler shade of the same alarm.
//
// A tone this build does not recognise falls back to the neutral row. A
// renderer must never be the reason a message goes silent, so an unknown tone
// still draws the notice, just without a colour opinion.
const TONE_STYLES: Record<string, { Icon: typeof Info; color: string }> = {
  critical: { Icon: AlertTriangle, color: "text-destructive" },
  privacy: { Icon: Lock, color: "text-warning" },
  degraded: { Icon: AlertCircle, color: "text-warning" },
  waiting: { Icon: Clock, color: "text-muted-foreground" },
  routine: { Icon: Info, color: "text-muted-foreground" },
  housekeeping: { Icon: Info, color: "text-muted-foreground" },
  resolved: { Icon: CheckCircle2, color: "text-success" },
};
const NEUTRAL_TONE = { Icon: Info, color: "text-muted-foreground" };

// GLYPH_ICONS honours NoticeWire.glyph (channelinteractions.Glyph), the
// optional semantic override for the rare category whose meaning is better
// served by a specific symbol than by its tone. It REPLACES the tone's icon
// (the tone's colour is kept). An unknown glyph is ignored rather than
// blanking the mark.
const GLYPH_ICONS: Record<string, typeof Info> = {
  money: CircleDollarSign,
  clock: Clock,
};

// NoticeCard renders one channelevents.NoticeWire: the one-way, zero-action
// counterpart to InteractionCard. It is what the server means by "the chat UI
// renders it as a real severity-styled card" (see messageResponse.Notice in
// pkg/web/webui/chat/handlers.go).
//
// Every field the wire carries is rendered, because each is load-bearing
// somewhere: `body` is the entire message for a continuation notice, `excerpt`
// carries the failure reason for a refusal, and `nextStep` is what keeps a
// stuck user unstuck (Tone.RequiresNextStep makes it mandatory at the critical
// / privacy / degraded tones). Rendering only lead+nextStep — what
// NoticeWire.Text() produces for single-line surfaces — would drop the other
// two on a surface that has room for them.
//
// SECURITY CONTRACT: `notice.excerpt` is UNTRUSTED — CardExcerpt renders it
// inert and fenced. Every other field is publisher-authored/trusted.
export function NoticeCard({ notice }: { notice: NoticeWire }) {
  const tone = TONE_STYLES[notice.tone ?? ""] ?? NEUTRAL_TONE;
  const Icon = GLYPH_ICONS[notice.glyph ?? ""] ?? tone.Icon;

  return (
    <div
      className="mx-auto flex w-full max-w-[85%] flex-col gap-2 rounded-lg border border-border bg-card/60 px-3 py-2.5"
      data-testid="notice-card"
      data-tone={notice.tone || ""}
    >
      <div className={cn("flex items-center gap-1.5 text-xs font-medium", tone.color)}>
        <Icon className="h-3.5 w-3.5 shrink-0" aria-hidden="true" />
        <span className="break-words text-card-foreground">{notice.lead}</span>
      </div>

      {notice.body && <div className="text-xs text-muted-foreground">{notice.body}</div>}

      <CardFields fields={notice.fields} />

      <CardExcerpt excerpt={notice.excerpt} />

      {notice.nextStep && <div className="text-xs font-medium text-card-foreground">{notice.nextStep}</div>}
    </div>
  );
}
