# CLIProxyAPI Development Guide

This file is for local development. Deployment and launchd operations live in
`DEPLOY.md`.

## Repository Layout

The local workspace currently has two related projects:

- `CLIProxyAPI/`: Go backend, SDK, Management API, bundled static assets, runtime docs.
- `../Cli-Proxy-API-Management-Center/`: React/Vite Management Center UI.

The backend serves the bundled UI from:

- `internal/managementasset/static/management.html`

The UI source of truth is the separate React project. Do not hand-edit the
generated `management.html`; rebuild the UI and copy the output instead.

## Local Ports

Current local convention:

| Service | URL | Port | Notes |
| --- | --- | ---: | --- |
| Backend/prod local origin | `http://localhost:8317` | `8317` | `cli-proxy-api --config /Users/justin/.cli-proxy-api/config.9router.yaml` |
| Management UI served by backend | `http://localhost:8317/management.html` | `8317` | Uses bundled `management.html` |
| Usage portal | `http://localhost:8317/usage` | `8317` | Requires valid proxy API key |
| UI dev server | `http://127.0.0.1:5175/` | `5175` | Vite dev server |
| Optional pprof | `http://127.0.0.1:8316` | `8316` | Only when enabled |
| Optional OAuth callback | `http://localhost:8080` | `8080` | CLI option dependent |

Machine-local tunnel note: `cook.tuanle.dev` currently points to
`http://127.0.0.1:8317` through cloudflared. Treat this as local operator state,
not a portable development requirement.

## Backend Development

Use the backend repo:

```bash
cd /Users/justin/Dev/VibeLab/CLIProxy/CLIProxyAPI
```

Build:

```bash
go build -o cli-proxy-api ./cmd/server
```

Run with the migrated local config:

```bash
./cli-proxy-api --config /Users/justin/.cli-proxy-api/config.9router.yaml
```

Run with a repo-local disposable config:

```bash
cp config.example.yaml config.dev.yaml
./cli-proxy-api --config config.dev.yaml
```

Targeted tests while developing:

```bash
go test ./internal/config ./internal/api/handlers/management
go test ./internal/access/config_access ./internal/watcher/diff
go test ./internal/api
```

Full backend gate before handing off:

```bash
go test ./...
```

Smoke checks:

```bash
curl -sS http://localhost:8317/healthz
curl -sS -o /tmp/cliproxy-management.html -w "%{http_code} %{size_download} %{content_type}\n" http://localhost:8317/management.html
```

## UI Development

Use the UI repo:

```bash
cd /Users/justin/Dev/VibeLab/CLIProxy/Cli-Proxy-API-Management-Center
```

Install dependencies:

```bash
bun install
```

Run dev server:

```bash
bun run dev --host 127.0.0.1 --port 5175
```

Quality gates:

```bash
bun run type-check
bun run lint
bun run build
```

The production UI build is a single-file Vite output:

```bash
dist/index.html
```

After UI changes that should ship with the backend, copy the built file into
the backend asset path:

```bash
cp /Users/justin/Dev/VibeLab/CLIProxy/Cli-Proxy-API-Management-Center/dist/index.html \
  /Users/justin/Dev/VibeLab/CLIProxy/CLIProxyAPI/internal/managementasset/static/management.html
```

Then rebuild/restart the backend process if testing the bundled production UI.

## Management API And UI Contracts

Management API base path:

```text
/v0/management
```

For UI work, keep the frontend typed against the Management API response shape.
Do not invent metadata in the UI if the backend does not persist it. If a field
matters operationally, add it to the backend contract first and keep the legacy
config path backward-compatible.

For API keys specifically:

- Legacy key list remains `api-keys: []`.
- Per-key operator metadata lives in `api-key-metadata`.
- Metadata keys are stable hashes generated from the API key value.
- Runtime auth enforces `disabled`, `revoked`, `expires-at`, and `ip-allowlist`.
- UI usage/cost/token fields come from usage analytics endpoints, not local guesses.

## Development Safety Rules

- Check `git status --short` before editing; this workspace is often dirty.
- Preserve user changes. Do not revert unrelated edits.
- Never commit real API keys, OAuth tokens, management passwords, DSNs, or auth files.
- Keep real local config outside the repo. The active migrated config is:
  `/Users/justin/.cli-proxy-api/config.9router.yaml`.
- Treat `config.example.yaml` as documentation and examples only.
- Keep startup read-only unless a management endpoint explicitly persists a config change.
- Keep generated assets generated. Source changes belong in the UI repo.
- When updating public/runtime behavior, update tests and docs in the same change.

## UI/UX Guidelines For Management Center

Management Center is an operator console, not a marketing surface.

- Prefer dense but readable tables for scan/compare workflows.
- Surface status, risk, owner, environment, last-used, errors, tokens, and cost near the row.
- Keep destructive actions explicit: disable/revoke/delete must have clear labels and confirmation where appropriate.
- Keep secrets masked by default; copy actions must make credential handling obvious.
- Empty, loading, disabled, and partial-data states must be visible.
- Mobile layout should remain usable, but desktop operator workflows are primary.
- Do not use placeholder text as fake product data. If backend data is missing, say so plainly or add the backend field.

## Recommended Change Flow

1. Inspect the current contract and runtime state.
2. Make backend changes first when persistence or enforcement is needed.
3. Add focused backend tests for config parsing, Management API handlers, and auth behavior.
4. Update frontend services/types.
5. Update UI pages and responsive states.
6. Run backend tests and frontend gates.
7. Build UI and update `internal/managementasset/static/management.html` when the bundled UI should change.
8. Smoke test the relevant local URL with Playwright or browser checks.

## Useful Commands

Backend status:

```bash
lsof -nP -iTCP:8317 -sTCP:LISTEN
launchctl list | grep cliproxyapi
```

UI dev status:

```bash
lsof -nP -iTCP:5175 -sTCP:LISTEN
```

Production health:

```bash
curl -sS http://localhost:8317/healthz
```

UI dev health:

```bash
curl -I http://127.0.0.1:5175/
```
