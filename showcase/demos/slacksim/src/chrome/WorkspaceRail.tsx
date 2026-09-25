import {
  Home,
  MessageCircle,
  Bell,
  MoreHorizontal,
  Plus,
  Moon,
  Sun,
} from "lucide-react";
import { useScenario } from "../runtime/context";

// The far-left workspace rail: the workspace glyph tile, primary nav icons, and
// a bottom cluster (create + theme toggle). Purely presentational in the sim.
export function WorkspaceRail({
  theme,
  onToggleTheme,
}: {
  theme: "light" | "dark";
  onToggleTheme: () => void;
}) {
  const { workspace } = useScenario();
  return (
    <nav className="sk-rail" aria-label="Workspaces">
      <div className="sk-rail-top">
        <button
          className="sk-rail-ws"
          style={{ background: workspace.accent }}
          title={workspace.name}
        >
          {workspace.glyph}
        </button>
        <RailItem icon={<Home size={22} />} label="Home" active />
        <RailItem icon={<MessageCircle size={22} />} label="DMs" />
        <RailItem icon={<Bell size={22} />} label="Activity" badge={9} />
        <RailItem icon={<MoreHorizontal size={22} />} label="More" />
      </div>
      <div className="sk-rail-bottom">
        <button
          className="sk-rail-btn"
          title="Create new"
          aria-label="Create new"
        >
          <Plus size={20} />
        </button>
        <button
          className="sk-rail-btn"
          title="Toggle theme"
          aria-label="Toggle theme"
          onClick={onToggleTheme}
        >
          {theme === "dark" ? <Sun size={18} /> : <Moon size={18} />}
        </button>
      </div>
    </nav>
  );
}

function RailItem({
  icon,
  label,
  active,
  badge,
}: {
  icon: React.ReactNode;
  label: string;
  active?: boolean;
  badge?: number;
}) {
  return (
    <button
      className={`sk-rail-item ${active ? "is-active" : ""}`}
      title={label}
      aria-label={label}
    >
      <span className="sk-rail-icon">
        {icon}
        {badge ? <span className="sk-rail-badge">{badge}</span> : null}
      </span>
      <span className="sk-rail-label">{label}</span>
    </button>
  );
}
