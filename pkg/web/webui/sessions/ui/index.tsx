import { mount } from "@ap/runtime";
import { SessionShell } from "./SessionShell";
import { applyStoredTheme } from "@ap/design";

// Theme is chosen per browser (design/theme.ts); apply it before the first
// render so a light-mode viewer sees one dark frame at most, never a flicker.
applyStoredTheme();

mount(SessionShell);
