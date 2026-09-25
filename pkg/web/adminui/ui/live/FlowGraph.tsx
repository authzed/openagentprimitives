import * as React from "react";

const W = 560;
const NODE_R = 28;
const COL = { channel: 60, agent: 260, tools: 470 };

function Node({ x, y, label, stroke }: { x: number; y: number; label: string; stroke: string }) {
  return (
    <g>
      <circle cx={x} cy={y} r={NODE_R} fill="hsl(var(--card))" stroke={stroke} strokeWidth={1.5} />
      <text x={x} y={y + 4} textAnchor="middle" fontSize={10} fill="hsl(var(--foreground))" fontFamily="monospace">
        {label.length > 11 ? label.slice(0, 10) + "…" : label}
      </text>
    </g>
  );
}

function Edge({ x1, y1, x2, y2, active }: { x1: number; y1: number; x2: number; y2: number; active: boolean }) {
  return (
    <path
      className={active ? "ap-edge-active" : undefined}
      d={`M ${x1} ${y1} C ${(x1 + x2) / 2} ${y1}, ${(x1 + x2) / 2} ${y2}, ${x2} ${y2}`}
      fill="none"
      stroke={active ? "hsl(var(--primary))" : "hsl(var(--border))"}
      strokeWidth={1.5}
      strokeDasharray={active ? "6 6" : undefined}
    >
      {active && (
        <animate attributeName="stroke-dashoffset" from="24" to="0" dur="0.9s" repeatCount="indefinite" />
      )}
    </path>
  );
}

export function FlowGraph({
  channelKind,
  agentClass,
  tools,
  active,
}: {
  channelKind?: string;
  agentClass?: string;
  tools: string[];
  active: boolean;
}) {
  const shown = tools.slice(0, 8); // fan-out stays readable
  const height = Math.max(160, shown.length * 64 + 40);
  const midY = height / 2;
  return (
    <svg viewBox={`0 0 ${W} ${height}`} className="w-full" role="img" aria-label="session flow graph">
      <Edge x1={COL.channel + NODE_R} y1={midY} x2={COL.agent - NODE_R} y2={midY} active={active} />
      {shown.map((tool, i) => {
        const ty = 40 + i * 64 + 12;
        return (
          <React.Fragment key={tool}>
            <Edge x1={COL.agent + NODE_R} y1={midY} x2={COL.tools - NODE_R} y2={ty} active={active} />
            <Node x={COL.tools} y={ty} label={tool} stroke="hsl(var(--state))" />
          </React.Fragment>
        );
      })}
      <Node x={COL.channel} y={midY} label={channelKind ?? "channel"} stroke="hsl(var(--success))" />
      <Node x={COL.agent} y={midY} label={agentClass ?? "agent"} stroke="hsl(var(--primary))" />
    </svg>
  );
}
