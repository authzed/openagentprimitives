import { Button } from "@ap/design";

export interface ActionBarProps {
  dirty: boolean;
  busy?: boolean;
  disabled?: boolean;
  note?: string;
  onValidate: () => void;
  onSave: () => void;
  // saveLabel overrides the Save button's text — AdvancedTab passes its
  // computed arm-to-apply label ("Apply" / "Apply without validating?" /
  // "Applying…") through here rather than ActionBar special-casing the
  // two-click confirm itself.
  saveLabel?: string;
}

// ActionBar is the footer shared by the Cluster and Advanced tabs: Validate
// + Save (or Apply), an "Unsaved changes" indicator, and an optional note.
// Each tab renders it as the LAST child of its bounded flex column, AFTER
// the tab's own scroll region (SettingsApp's h-screen shell means the
// document itself never scrolls) — that placement, not any positioning
// here, is what keeps it on-screen no matter how long the form is. The top
// border and opaque background separate it from the content ending above.
export function ActionBar({ dirty, busy = false, disabled = false, note, onValidate, onSave, saveLabel }: ActionBarProps) {
  const isDisabled = busy || disabled;
  return (
    <div className="flex shrink-0 flex-col gap-2 border-t border-border bg-background py-3">
      <div className="flex flex-wrap items-center justify-between gap-x-4 gap-y-2">
        <div className="flex items-center gap-3">
          <Button variant="outline" onClick={onValidate} disabled={isDisabled}>
            Validate
          </Button>
          <Button onClick={onSave} disabled={isDisabled}>
            {saveLabel ?? "Save"}
          </Button>
          {dirty && (
            <span role="status" className="text-xs text-muted-foreground">
              Unsaved changes
            </span>
          )}
        </div>
        {note && <span className="text-xs text-muted-foreground">{note}</span>}
      </div>
    </div>
  );
}
