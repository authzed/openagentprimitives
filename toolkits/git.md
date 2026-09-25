# git

git CLI toolkit.

## HTTPS authentication — `GIT_TOKEN`

The git toolkit authenticates HTTPS operations from the `GIT_TOKEN`
environment variable. `git` itself does not read any token env var; the
sandbox image bakes in a `GIT_ASKPASS` helper that serves `GIT_TOKEN`
to git in-memory when it needs a credential. So:

- An AgentIdentity binding for `cli:git` injects the token as
  `GIT_TOKEN` (a `static` credential → `secretRef`).
- The agent clones with a **plain** URL: `git clone
  https://github.com/owner/repo`. git obtains the credential via the
  askpass helper; the token is never written to `.git/config` or any
  other file.
- NEVER embed the token in the URL (`https://x-access-token:TOKEN@…`).
  That persists the secret into `.git/config` and the ToolCall CR.
  A toolspec CEL constraint rejects clone URLs that contain a `@`
  userinfo component for exactly this reason.

Because the token is never persisted, the working tree's `.git`
directory carries no secret and can live on a shared volume safely —
no `--separate-git-dir` / `GIT_DIR` gymnastics are needed.
