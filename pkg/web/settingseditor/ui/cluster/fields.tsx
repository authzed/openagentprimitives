import * as React from "react";
import { Input, Label, Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@ap/design";
import {
  type SettingsSpec,
  updateSpec,
  getPath,
  boolTriState,
  boolFromTriState,
  parseLines,
  joinLines,
  type BoolTriState,
} from "./spec";

// Small path-driven field controls shared by LimitsSection and
// DefaultsSection — both edit the same primitive shapes (a Go-duration
// string, a plain number, a tri-state *bool) at different paths in the same
// spec, so the control itself is the reusable unit, not a per-section copy.

export interface FieldProps {
  spec: SettingsSpec;
  onChange: (spec: SettingsSpec) => void;
  path: readonly string[];
  id: string;
  label: string;
  hint?: string;
}

// DurationField edits a metav1.Duration-shaped string field (e.g. "30m").
// Clearing the input deletes the key — see spec.ts's updateSpec doc for why
// an absent key, not an empty string, is what "no ceiling" means on the wire.
export function DurationField({ spec, onChange, path, id, label, hint }: FieldProps) {
  const raw = getPath(spec, path);
  const value = typeof raw === "string" ? raw : "";
  return (
    <div className="flex flex-col gap-1">
      <Label htmlFor={id}>{label}</Label>
      <Input
        id={id}
        placeholder="e.g. 30m"
        value={value}
        onChange={(e) => {
          const text = e.target.value;
          onChange(updateSpec(spec, path, text.trim() === "" ? undefined : text));
        }}
      />
      {hint && <p className="text-xs text-muted-foreground">{hint}</p>}
    </div>
  );
}

// NumberField edits a plain numeric ceiling/count field. Clearing the input
// deletes the key (zero is a real, meaningful value for some of these
// fields' siblings elsewhere, so blank must not silently become 0).
export function NumberField({ spec, onChange, path, id, label, hint }: FieldProps) {
  const raw = getPath(spec, path);
  const value = typeof raw === "number" ? raw : "";
  return (
    <div className="flex flex-col gap-1">
      <Label htmlFor={id}>{label}</Label>
      <Input
        id={id}
        type="number"
        value={value}
        onChange={(e) => {
          const text = e.target.value;
          if (text.trim() === "") {
            onChange(updateSpec(spec, path, undefined));
            return;
          }
          const n = Number(text);
          if (Number.isNaN(n)) return;
          onChange(updateSpec(spec, path, n));
        }}
      />
      {hint && <p className="text-xs text-muted-foreground">{hint}</p>}
    </div>
  );
}

// TextField edits a plain (non-tri-state) string field, e.g. sandbox.kind.
export function TextField({ spec, onChange, path, id, label, hint }: FieldProps) {
  const raw = getPath(spec, path);
  const value = typeof raw === "string" ? raw : "";
  return (
    <div className="flex flex-col gap-1">
      <Label htmlFor={id}>{label}</Label>
      <Input
        id={id}
        value={value}
        onChange={(e) => {
          const text = e.target.value;
          onChange(updateSpec(spec, path, text.trim() === "" ? undefined : text));
        }}
      />
      {hint && <p className="text-xs text-muted-foreground">{hint}</p>}
    </div>
  );
}

// BoolTriStateField edits a *bool ceiling/grant field via an explicit 3-way
// select (unset/true/false) rather than a checkbox, since a checkbox has no
// third state to represent "this tier imposes no requirement".
export function BoolTriStateField({ spec, onChange, path, id, label, hint }: FieldProps) {
  const raw = getPath(spec, path);
  const state = boolTriState(typeof raw === "boolean" ? raw : undefined);
  return (
    <div className="flex flex-col gap-1">
      <Label htmlFor={id}>{label}</Label>
      <Select
        value={state}
        onValueChange={(v) => onChange(updateSpec(spec, path, boolFromTriState(v as BoolTriState)))}
      >
        <SelectTrigger id={id}>
          <SelectValue />
        </SelectTrigger>
        <SelectContent>
          <SelectItem value="unset">Unset (no requirement)</SelectItem>
          <SelectItem value="true">True</SelectItem>
          <SelectItem value="false">False</SelectItem>
        </SelectContent>
      </Select>
      {hint && <p className="text-xs text-muted-foreground">{hint}</p>}
    </div>
  );
}

// AllowlistTextarea is a newline-per-entry list editor. It keeps its OWN
// local text state rather than deriving the displayed value from
// joinLines(parseLines(text)) on every keystroke: parseLines trims and drops
// blank lines, so re-deriving the controlled value from the committed array
// would erase the trailing newline the instant a user presses Enter to start
// a new entry, making it impossible to type a second line. `initial` seeds
// the local state once (this component is expected to remount — via a
// changing `key` or a parent unmount/mount, e.g. a tri-state list's
// Unrestricted/Restrict toggle — whenever the caller needs it to pick up a
// different starting value); `onCommit` still fires the parsed array on
// every keystroke so the parent spec stays live-updated.
export function AllowlistTextarea({
  id,
  initial,
  onCommit,
}: {
  id: string;
  initial: string[];
  onCommit: (items: string[]) => void;
}) {
  const [text, setText] = React.useState(() => joinLines(initial));
  return (
    <textarea
      id={id}
      rows={4}
      className="flex w-full rounded-md border border-input bg-transparent px-3 py-2 text-sm shadow-sm placeholder:text-muted-foreground focus-visible:outline-none focus-visible:ring-1 focus-visible:ring-ring"
      value={text}
      onChange={(e) => {
        setText(e.target.value);
        onCommit(parseLines(e.target.value));
      }}
    />
  );
}

// SummaryCard renders a read-only, flattened one-line summary of a nested
// guard policy this tab does not edit directly (pinning, toolGuard,
// contentInspectors, allowedMCPServers) — see the task-11 brief's deliberate
// narrowing note in spec.ts's module doc.
export function SummaryCard({ label, value }: { label: string; value: string }) {
  return (
    <div className="flex flex-col gap-0.5 rounded-md border border-border p-3">
      <span className="text-xs font-medium text-muted-foreground">{label}</span>
      <span className="text-sm">{value}</span>
      <span className="text-xs text-muted-foreground">Edit in Advanced tab</span>
    </div>
  );
}
