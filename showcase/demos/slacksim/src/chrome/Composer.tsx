import {
  Plus,
  Smile,
  AtSign,
  Video,
  Mic,
  Code2,
  SendHorizontal,
  ChevronDown,
} from "lucide-react";

// The bottom message composer. Presentational only — the sim never sends;
// scripted "typing" is animated by the driver posting messages directly.
export function Composer({ placeholder }: { placeholder: string }) {
  return (
    <div className="sk-composer">
      <div className="sk-composer-box">
        <div className="sk-composer-input" aria-label={placeholder}>
          <span className="sk-composer-placeholder">{placeholder}</span>
        </div>
        <div className="sk-composer-toolbar">
          <div className="sk-composer-tools-left">
            <button className="sk-icon-btn" aria-label="Add">
              <Plus size={18} />
            </button>
            <button className="sk-icon-btn" aria-label="Emoji">
              <Smile size={18} />
            </button>
            <button className="sk-icon-btn" aria-label="Mention">
              <AtSign size={18} />
            </button>
            <button className="sk-icon-btn" aria-label="Video">
              <Video size={18} />
            </button>
            <button className="sk-icon-btn" aria-label="Record">
              <Mic size={18} />
            </button>
            <button className="sk-icon-btn" aria-label="Format">
              <Code2 size={18} />
            </button>
          </div>
          <div className="sk-composer-tools-right">
            <button className="sk-send-btn" aria-label="Send">
              <SendHorizontal size={16} />
              <ChevronDown size={13} />
            </button>
          </div>
        </div>
      </div>
    </div>
  );
}
