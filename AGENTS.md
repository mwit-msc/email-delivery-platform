# Contributor and coding-agent guide

This guide applies to the whole repository. Read it alongside `README.md`,
`CONTRIBUTING.md`, and any more specific instructions in the files you change.
Use the current task's explicit requirements to resolve conflicts with this guide.
Tools that do not discover `AGENTS.md` automatically can be pointed at this file.

## Architecture

EDP is a self-hosted Go application with a PostgreSQL delivery queue. It uses
standard `net/http` routing and server-rendered `html/template` pages. The same
binary/image runs the web server, worker, migrations, and seed commands.

| Location | Responsibility |
| --- | --- |
| `cmd/edp/main.go` | CLI commands and process startup |
| `internal/edp/auth.go` | Local login, OIDC, sessions, CSRF, and scoped RBAC |
| `internal/edp/messages.go` | Admission, rendering, quotas, and idempotency |
| `internal/edp/worker.go` | Queue claims, leases, SMTP, retries, and maintenance |
| `internal/edp/campaigns.go` | Imports, mappings, approval, and campaign controls |
| `internal/edp/pmg.go`, `fallback.go` | Read-only gateway tracking and secondary routing |
| `internal/edp/config.go`, `runtime.go`, `settings.go` | Bootstrap configuration and encrypted runtime settings |
| `internal/edp/http.go`, `dashboard.go` | Routes, pages, statistics, and administration |
| `internal/edp/store.go`, `migrations/` | Persistence, migration runner, and repeatable seeding |
| `internal/edp/web/views/`, `web/assets/` | Templates, custom UI code, bundled libraries, and fonts |
| `api/openapi.yaml` | Public service API contract |
| `scripts/`, `deploy/`, `.github/workflows/` | Operations, feedback adapter, and CI/release automation |

Paths abbreviated after `internal/edp/` in the table are inside that directory.
The UI uses Tabler, HTMX, Chart.js, and GrapesJS. There is no frontend framework
or Node.js build pipeline. Keep additions consistent with this architecture.

## Local development

```sh
make dev                         # Unique local credentials, migrations, demo seed
docker compose logs -f web worker
docker compose up --build -d      # Rebuild after changing embedded views/assets
make down                        # Stop services; retain database data
```

Dashboard: `http://localhost:8080`. Mailpit: `http://localhost:8025`.
Compose exposes its development PostgreSQL instance on localhost port 15432.
Use the Go version in `go.mod` for local Go commands. Docker handles the runtime
for the development stack; JavaScript syntax checks optionally use local Node.js.

Do not print the generated `.env` or copy its credentials into documentation.
Test email against Mailpit or isolated mock servers, never real recipients.
Do not delete database volumes, run `reset-root`, or replace deployment data as
routine test cleanup. Seeding must preserve an existing root password and data.

## Making changes

- Inspect the relevant code and tests first. Keep edits focused and preserve
  unrelated or concurrent work. If using multiple agents, assign distinct files
  or responsibilities and reconcile their changes without reverting others.
- Format changed Go files with `gofmt`. Use the existing error, audit, routing,
  and persistence patterns before adding dependencies or abstractions.
- Add new numbered SQL migrations; do not rewrite migrations already released.
  The runner embeds them, serializes application, and records each version.
  Each migration must record its version and preserve existing installations.
- Keep the OpenAPI contract and README consistent with API/configuration changes.
  Use organization-neutral defaults and synthetic examples.
- Bundled minified libraries and fonts are vendor assets. Change custom source
  files for application behavior. Update vendor versions from their official
  distribution and retain the matching licenses when an update is necessary.

## Delivery and identity invariants

- A successful admission response follows the committed durable message/job.
  Preserve application scope, payload-sensitive idempotency, recipient limits,
  rolling quotas, queue bounds, and shared worker throttling.
- A worker owns a message through its lease token. Recheck sending controls and
  expiration before SMTP submission. Losing acceptance certainty produces
  `unknown`; never automatically resend an uncertain message.
- `relay_accepted` is gateway acceptance; `remote_accepted` is recipient-server
  acceptance. Neither proves inbox placement or that someone read the message.
- Secondary routing is explicitly enabled per application and allowed once,
  only after a correlated, confirmed permanent IP reputation rejection.
  Deferrals, invalid recipients, quarantine, and uncertainty do not qualify.
  PMG tracking reads evidence; it must not manipulate the gateway's queue.
- Approved campaigns use frozen template versions and rendered jobs. Import or
  mapping changes require draft state. Pause, cancellation, expiration, and
  transactional capacity reservations must remain effective during delivery.
- Escape template variables and sanitize both email HTML and editor project
  data. Do not reintroduce scripts through GrapesJS metadata or executable
  template expressions.
- Preserve OIDC signature, issuer/audience, state, nonce, PKCE, and UserInfo
  identity checks. Persist rotated refresh tokens before dependent requests.
  Local login remains independent of SSO; stale group grants fail closed.
- Enforce permissions and CSRF on the server for every route. Hidden UI controls
  are not authorization. Do not link identities solely by email or treat an
  arbitrary provider role claim as platform authority.
- Saved runtime settings override bootstrap values without mutating bootstrap
  configuration. Secrets remain encrypted with the deployment's stable key;
  blank inputs retain values and explicit clear controls remove them.

## UI navigation

HTMX navigation swaps `#page-content` and updates `#edp-sidebar`/`#edp-topbar`.
Preserve workspace scope, active navigation, browser history, keyboard focus,
and the normal no-JavaScript fallback. Mutation forms retain their current
submission behavior unless a task explicitly changes it.

Page scripts load once. Use the page lifetime/abort signal to stop polling,
destroy chart/editor instances, and remove page-owned handlers when navigating.
Guard unsaved editor changes. Keep sensitive HTML out of HTMX's local history
cache and redirect expired sessions to login. Verify navigation into and out of
charts/editor pages, Back/Forward, workspace changes, and connection tests.
Maintain the dark theme, responsive layout, and reduced-motion support.

## Validation

Use the checks relevant to the change. Documentation-only edits need link/path
review and `git diff --check`; do not invent runtime tests for prose changes.
For Go or delivery/authentication changes, run:

```sh
go vet ./...
make test
make integration
```

`make test` skips database-dependent tests when `EDP_TEST_DATABASE_URL` is absent;
that is not integration proof. `make integration` uses local Compose PostgreSQL
and the race detector. Test helpers create and remove isolated temporary schemas.
For another dedicated test instance, use:

```sh
EDP_TEST_DATABASE_URL='<test PostgreSQL URL>' go test -race ./... -count=1
python3 -m unittest discover -s scripts -p 'test_*.py'
```

Check changed custom JavaScript with `node --check` when available. For UI
changes, verify behavior in a browser after rebuilding the embedded assets.
For workflow changes, validate YAML/actions and preserve the pinned action SHAs.
Report checks actually run and distinguish mock integration tests from live
provider verification. Do not claim live SMTP, PMG, or SSO verification without it.

## Privacy and publication

Never commit credentials, real recipient lists, organization-specific assets,
database dumps, internal planning, or private conversation exports. Respect
`.gitignore` and `.dockerignore`; do not force-add private inputs. Keep logs,
screenshots, test fixtures, and release notes free of deployment secrets.

Before a requested commit or push, inspect the exact staged files and diff for
private data, including generated credentials. Use Conventional Commits. Do not
add co-author or AI-attribution trailers unless explicitly requested.

`main` runs CI. Publishing is controlled by `release` and produces GHCR images
for **linux/amd64 only**; local builds can use the host architecture. Do not push
to `release`, create a release, overwrite tags/images, or change repository
visibility without an explicit request. Preserve immutable version ownership,
automatic versioning, and protections against rolling back the release alias.
Documentation updates alone do not require a container release.
