// Props injected by the Go Page.Build (pkg/web/adminui/adminui.go).
export interface AdminAppProps {
  apiBase: string; // "/admin/api" — webd's proxy prefix to admind
  // The logged-in platform admin, decoded from the auth subject. Absent when
  // the subject couldn't be resolved — AppShell falls back to its default chip.
  // role is still sent by the backend but no longer rendered (every admin here
  // is a platform admin), so it's optional.
  currentUser?: { email: string; role?: string };
}
