# Repository Guidelines

## Project Structure & Module Organization

This repository is a single Go 1.22 service. `main.go` contains configuration, HTTP routes, upstream clients, caching, authentication, and the embedded web server. `main_test.go` contains Go tests. Browser UI files live in `web/` (`index.html`, `browser.html`, `settings.html`, and `api-docs.html`); static media and documentation assets are under `docs/`. `openapi.yaml` is the checked-in API contract and is embedded into the binary. Docker and release files are `Dockerfile`, `docker-compose*.yml`, and `.github/workflows/release.yml`.

## Build, Test, and Development Commands

Use PowerShell from the repository root:

```powershell
go test ./...
go vet ./...
go run .
docker compose -f docker-compose.local.yml up -d --build
```

The first two commands validate Go code; `go run .` starts the service using the local configuration; the Compose command builds and starts the local container on port `8123`. Do not commit `config.json` or credentials.

## Coding Style & Naming Conventions

Run `gofmt` on changed Go files and follow idiomatic Go naming: exported identifiers use `PascalCase`, internal identifiers use `camelCase`, and handlers use `handle<Resource>`. Keep HTTP paths lowercase and descriptive. Match the existing plain HTML/CSS/JavaScript style, preserve responsive layouts, and avoid introducing a frontend build dependency unless necessary.

## Testing Guidelines

Add or update table-driven tests in `main_test.go` for route, authentication, configuration, and upstream parsing changes. Run `go test ./...`, `go vet ./...`, and `git diff --check` before committing. For UI changes, also validate the page JavaScript syntax and exercise the affected route through the local server or Docker container.

## API Documentation and UI Synchronization

Every new or changed API must be reflected in `openapi.yaml` and the catalog/debugger in `web/api-docs.html`, including method, path, authentication, request examples, and response behavior. Update the relevant `docs/*.md` file when the public configuration or API contract changes.

## Commit & Pull Request Guidelines

Use concise Conventional Commit-style subjects, such as `feat: add ...`, `fix: correct ...`, or `ui: adjust ...`. Keep each commit focused. Pull requests should explain behavior changes, list validation commands, mention configuration or deployment impact, and include screenshots for visible browser or device UI changes.

## Security & Configuration Tips

OAuth tokens, cookies, Basic Auth passwords, App API keys, and proxy credentials are secrets. Keep them in ignored local configuration or deployment secrets, never in source, logs, screenshots, URLs, or documentation examples. Preserve authentication on configuration and debugging endpoints.
