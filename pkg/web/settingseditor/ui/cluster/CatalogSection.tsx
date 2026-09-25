import * as React from "react";
import {
  Button,
  Input,
  Label,
  Table,
  TableHeader,
  TableBody,
  TableRow,
  TableHead,
  TableCell,
  Dialog,
  DialogContent,
  DialogHeader,
  DialogTitle,
  DialogFooter,
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@ap/design";
import {
  type SettingsSpec,
  type ModelCatalogEntry,
  defaultTokenRefFor,
  removeCatalogEntry,
  replaceCatalogEntry,
} from "./spec";
import type { TokenWrite } from "../lib/api";

export interface CatalogSectionProps {
  spec: SettingsSpec;
  onChange: (spec: SettingsSpec) => void;
  tokens: TokenWrite[];
  onTokensChange: (tokens: TokenWrite[]) => void;
}

const CATALOG_PROVIDERS = ["anthropic", "openai", "openrouter"] as const;

interface DialogForm {
  name: string;
  provider: string;
  isDefault: boolean;
  inputPerMTok: number;
  outputPerMTok: number;
  tokenInput: string;
}

function emptyForm(): DialogForm {
  return { name: "", provider: "anthropic", isDefault: false, inputPerMTok: 0, outputPerMTok: 0, tokenInput: "" };
}

function formFor(entry: ModelCatalogEntry): DialogForm {
  return {
    name: entry.name,
    provider: entry.provider ?? "anthropic",
    isDefault: !!entry.default,
    inputPerMTok: entry.inputPerMTok ?? 0,
    outputPerMTok: entry.outputPerMTok ?? 0,
    tokenInput: "",
  };
}

function priceCell(v: number | undefined): string {
  return v === undefined || v === 0 ? "—" : String(v);
}

// CatalogSection is the Cluster tab's model-catalog editor: entries table
// (name/provider/default/prices), an add/edit Dialog, and inline
// delete-with-confirm. The catalog round-trips as one atomic array — every
// mutation here goes through spec.ts's upsert/remove/replace helpers, which
// always write back the FULL array so entries the dialog never touched
// survive untouched (see spec.ts's module doc).
export function CatalogSection({ spec, onChange, tokens, onTokensChange }: CatalogSectionProps) {
  const entries = spec.modelCatalog ?? [];

  const [dialogOpen, setDialogOpen] = React.useState(false);
  const [editingName, setEditingName] = React.useState<string | null>(null);
  const [form, setForm] = React.useState<DialogForm>(emptyForm());
  const [confirmingDelete, setConfirmingDelete] = React.useState<string | null>(null);

  function openAdd() {
    setEditingName(null);
    setForm(emptyForm());
    setDialogOpen(true);
  }

  function openEdit(entry: ModelCatalogEntry) {
    setEditingName(entry.name);
    setForm(formFor(entry));
    setDialogOpen(true);
  }

  function handleSave() {
    const name = form.name.trim();
    if (!name) return;
    const editingEntry = editingName ? entries.find((e) => e.name === editingName) : undefined;
    const tokenRef = editingEntry?.tokenRef ?? defaultTokenRefFor(name);
    // Spread the existing entry FIRST: the dialog only surfaces the curated
    // fields, so any others the entry carries (routing, set via the raw YAML
    // tab) must round-trip untouched rather than being erased by a rebuild.
    // The explicit fields after the spread override it, including
    // `default: form.isDefault` overriding a spread `default: true` when the
    // user unchecks it. For a NEW entry editingEntry is undefined, which an
    // object spread treats as {}.
    const entry: ModelCatalogEntry = {
      ...editingEntry,
      name,
      provider: form.provider,
      default: form.isDefault,
      inputPerMTok: form.inputPerMTok,
      outputPerMTok: form.outputPerMTok,
      tokenRef,
    };
    onChange(replaceCatalogEntry(spec, editingName ?? name, entry));

    const tokenValue = form.tokenInput.trim();
    if (tokenValue !== "") {
      const filtered = tokens.filter(
        (t) => !(t.namespace === tokenRef.namespace && t.name === tokenRef.name && t.key === tokenRef.key),
      );
      onTokensChange([...filtered, { ...tokenRef, value: tokenValue }]);
    }
    setDialogOpen(false);
  }

  function handleDelete(name: string) {
    onChange(removeCatalogEntry(spec, name));
    // Drop any token value staged (but not yet POSTed) for this entry —
    // otherwise deleting the row would still write a Secret for a catalog entry
    // that no longer exists, orphaning it. The target is the entry's own
    // tokenRef, falling back to the default ref a new entry would have used.
    const deleted = entries.find((e) => e.name === name);
    const ref = deleted?.tokenRef ?? defaultTokenRefFor(name);
    const filtered = tokens.filter(
      (t) => !(t.namespace === ref.namespace && t.name === ref.name && t.key === ref.key),
    );
    if (filtered.length !== tokens.length) {
      onTokensChange(filtered);
    }
    setConfirmingDelete(null);
  }

  return (
    <section className="flex flex-col gap-3">
      <div className="flex items-center justify-between">
        <h3 className="text-sm font-medium">Model catalog</h3>
        <Button size="sm" onClick={openAdd}>
          Add model
        </Button>
      </div>

      <Table>
        <TableHeader>
          <TableRow>
            <TableHead>Name</TableHead>
            <TableHead>Provider</TableHead>
            <TableHead>Default</TableHead>
            <TableHead>Input $/MTok</TableHead>
            <TableHead>Output $/MTok</TableHead>
            <TableHead>Actions</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {entries.length === 0 && (
            <TableRow>
              <TableCell colSpan={6} className="text-muted-foreground">
                No catalog entries.
              </TableCell>
            </TableRow>
          )}
          {entries.map((entry) => (
            <TableRow key={entry.name}>
              <TableCell>{entry.name}</TableCell>
              <TableCell>{entry.provider || "—"}</TableCell>
              <TableCell>{entry.default ? "Yes" : "No"}</TableCell>
              <TableCell>{priceCell(entry.inputPerMTok)}</TableCell>
              <TableCell>{priceCell(entry.outputPerMTok)}</TableCell>
              <TableCell>
                {confirmingDelete === entry.name ? (
                  <div className="flex gap-2">
                    <Button variant="destructive" size="sm" onClick={() => handleDelete(entry.name)}>
                      Confirm delete
                    </Button>
                    <Button variant="outline" size="sm" onClick={() => setConfirmingDelete(null)}>
                      Cancel
                    </Button>
                  </div>
                ) : (
                  <div className="flex gap-2">
                    <Button variant="outline" size="sm" onClick={() => openEdit(entry)}>
                      Edit
                    </Button>
                    <Button variant="outline" size="sm" onClick={() => setConfirmingDelete(entry.name)}>
                      Delete
                    </Button>
                  </div>
                )}
              </TableCell>
            </TableRow>
          ))}
        </TableBody>
      </Table>

      <Dialog open={dialogOpen} onOpenChange={setDialogOpen}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>{editingName ? "Edit model" : "Add model"}</DialogTitle>
          </DialogHeader>
          <div className="flex flex-col gap-3">
            <div className="flex flex-col gap-1">
              <Label htmlFor="catalog-name">Name</Label>
              <Input
                id="catalog-name"
                value={form.name}
                onChange={(e) => setForm({ ...form, name: e.target.value })}
              />
            </div>

            <div className="flex flex-col gap-1">
              <Label htmlFor="catalog-provider">Provider</Label>
              <Select value={form.provider} onValueChange={(v) => setForm({ ...form, provider: v })}>
                <SelectTrigger id="catalog-provider">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  {CATALOG_PROVIDERS.map((p) => (
                    <SelectItem key={p} value={p}>
                      {p}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </div>

            <div className="flex items-center gap-2">
              <input
                id="catalog-default"
                type="checkbox"
                checked={form.isDefault}
                onChange={(e) => setForm({ ...form, isDefault: e.target.checked })}
                className="h-4 w-4 rounded border-border"
              />
              <Label htmlFor="catalog-default">Default model</Label>
            </div>

            <div className="flex flex-col gap-1">
              <Label htmlFor="catalog-input-price">Input $/MTok</Label>
              <Input
                id="catalog-input-price"
                type="number"
                value={form.inputPerMTok}
                onChange={(e) => setForm({ ...form, inputPerMTok: Number(e.target.value) || 0 })}
              />
            </div>

            <div className="flex flex-col gap-1">
              <Label htmlFor="catalog-output-price">Output $/MTok</Label>
              <Input
                id="catalog-output-price"
                type="number"
                value={form.outputPerMTok}
                onChange={(e) => setForm({ ...form, outputPerMTok: Number(e.target.value) || 0 })}
              />
            </div>

            <div className="flex flex-col gap-1">
              <Label htmlFor="catalog-token">API token</Label>
              <Input
                id="catalog-token"
                type="password"
                placeholder="unchanged"
                value={form.tokenInput}
                onChange={(e) => setForm({ ...form, tokenInput: e.target.value })}
              />
              <p className="text-xs text-muted-foreground">
                Leave blank to keep the existing token for this entry.
              </p>
            </div>
          </div>
          <DialogFooter>
            <Button onClick={handleSave} disabled={form.name.trim() === ""}>
              Save
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </section>
  );
}
