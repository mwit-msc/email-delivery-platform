# Email Delivery Platform

EDP is a Go application that serves a dark web dashboard and a service API, stores delivery jobs in PostgreSQL, and sends through an existing SMTP gateway. The same image runs the web server, delivery worker, migrations, and seed commands. The dashboard uses Tabler, HTMX, Chart.js, and a GrapesJS newsletter editor; assets and Outfit/Thai fonts are bundled locally.

## Run locally

Requirements: Docker with Compose. No local Node.js runtime is needed.

```sh
make dev
```

This generates `.env` with a unique encryption key and root password, builds the image, starts PostgreSQL and Mailpit, applies migrations, and seeds the root account, a demo application, and a welcome message. Optional organization-specific certificate templates can be supplied through deployment seed files. Seeding is repeatable and preserves changed passwords and templates.

- Dashboard: http://localhost:8080
- Captured email: http://localhost:8025
- Root username and password: `ROOT_USERNAME` / `ROOT_PASSWORD` in the generated `.env`

Local SMTP initially goes to Mailpit; you can change it in Settings. Docker's published ports bind to localhost. `docker compose down` stops the services while preserving the database volume. `.env`, backups, and service-account credentials are ignored by Git and excluded from Docker builds.

Use `docker compose logs -f web worker` for runtime logs. API errors contain an `error` field. The dashboard uses Bangkok time and refreshes statistics and campaign progress automatically. HTMX navigation replaces the content and current workspace controls without reloading the document. Charts and the editor load on demand; editor changes prompt before navigation. Sensitive pages are not stored in HTMX’s local history cache.

## Included in v1

- Configurable OIDC with PKCE S256, state/nonce validation, signed ID tokens, pairwise identities, refresh rotation, and fresh group claims.
- Independent local username/password login with Argon2id, login throttling, server sessions, and CSRF protection.
- Environment-seeded root account; application-scoped roles, direct local grants, SSO group mappings, scoped and revocable service keys.
- Transactional and expiring OTP-message delivery, idempotency, recipient cooldowns, rolling quotas, bounded queues, fair scheduling, shared SMTP throttles, retry backoff, gateway circuit breaker, and pause/cancel controls.
- Visual email editor with immutable versions, sanitized HTML, plain-text alternatives, test sends, and placeholders.
- CSV and read-only Google Sheets bulk/certificate imports, column mapping, duplicate detection, validation, preview, approval, scheduling, pause/resume, cancellation, and progress.
- Statistics, charts, audit records, body-retention cleanup, authenticated Prometheus metrics, example alert rules, and backup/restore-check scripts.
- Read-only PMG API tracking, opt-in secondary SMTP routing after confirmed permanent IP reputation rejection, and gateway outcome ingestion with a durable Postfix-log adapter; `remote_accepted` means the recipient's server accepted transport, not inbox placement or reading.

OTP generation/verification, certificate PDF generation, and file attachments remain outside v1. Certificate dispatch sends existing links. Multiple awards to the same recipient remain distinct dispatches; exact duplicate recipient/school/event/award/link rows require exclusion or correction.

## Authentication and access

Register a confidential OIDC client with callback `PUBLIC_URL/auth/callback`. Configure the provider in **Settings → Single sign-on**, including issuer/discovery URL, client ID, client secret, scopes, provider label, and group claim. The default scopes are `openid profile email groups offline_access`; adapt them to the provider’s registered client. Environment variables remain available for bootstrap configuration.

The login screen offers the configured SSO provider and local login whenever a client is enabled. SSO discovery failures do not stop EDP startup or local login. SSO creates an identity without email-based account linking and without automatic sending access. Group mappings are configured in **People and access**; an identity-provider `role` claim is not treated as EDP administrative authority.

Group claims are refreshed every five minutes. Expired group grants fail closed during an SSO outage. Local login uses only explicit EDP grants and never inherits cached SSO groups. Give designated recovery operators local passwords and direct roles before an outage.

| Role | Application permissions |
| --- | --- |
| Viewer | Read messages, templates, campaigns, statistics |
| Developer | Viewer permissions, send messages, edit templates, manage ordinary service keys |
| Campaign operator | Viewer permissions, prepare/test campaigns and control approved delivery |
| Campaign approver | Viewer permissions, approve reviewed campaigns |
| Application administrator | All application operations and ordinary key management |
| Platform administrator / root | Platform controls, applications, identities, quotas, group mappings, people and grants |

Sender identities, quota changes, and `delivery.report` credential provisioning require platform administration. Root is still subject to sending quotas. API keys expire after 90 days and are displayed once. Store them in the calling application's secret store.

Root seeding uses `ROOT_USERNAME` and either `ROOT_PASSWORD` (minimum 12 characters) or a valid `ROOT_PASSWORD_HASH`. Restarting never overwrites an existing root. For explicit recovery, set a new `ROOT_PASSWORD` in the environment and run:

```sh
docker compose run --rm web reset-root
```

This resets root's password and invalidates its sessions. Remove bootstrap plaintext from production configuration once a recovery procedure is established. Keep `SECRET_KEY` stable: it encrypts stored OIDC tokens and saved connection secrets. Root can change its current password in **Settings → Account**, using the current password; other sessions are invalidated.

## Set up connections in the dashboard

Log in with the seeded root account and select **All workspaces → Settings**. Setup does not require SSO to be available.

- **SMTP:** configure primary and optional secondary address, TLS mode, username/password, and bounce domain. Test connects, negotiates TLS, authenticates, and issues NOOP; it sends no email.
- **PMG tracking:** configure API URL, read-only audit username/password, optional node names, and optional PEM CA bundle. Test validates authentication and tracker read access. An empty URL disables polling.
- **Single sign-on:** configure and test discovery before enabling SSO. Discovery tests cannot validate client credentials, callback registration, or group claims; complete a real sign-in for those. Saving identity settings invalidates existing SSO sessions and pending login states; local sessions remain available.
- **Delivery controls:** change shared rate/concurrency limits, pause sending, and reset the gateway circuit breaker.

Secrets can be entered directly in forms even when no environment value exists. Fields are never populated with the saved secret: leave blank to retain it, enter a replacement, or explicitly clear it. Saved connection settings override bootstrap environment values and apply to web/worker operations without restart. Secrets are encrypted in PostgreSQL with `SECRET_KEY`; that key and database credentials remain deployment configuration. Connection tests use entered values without saving them.

## Service API

Create a service key on the application settings page with the required scopes. Requests use `Authorization: Bearer <key>`.

```sh
curl http://localhost:8080/v1/messages \
  -H "Authorization: Bearer $EDP_API_KEY" \
  -H 'Idempotency-Key: registration-42' \
  -H 'Content-Type: application/json' \
  -d '{"to":"person@example.test","template":"welcome","variables":{"name":"Person"}}'
```

The server returns `202` only after the message and queue job are committed. The response contains `id`, `status_url`, and `replayed`. Repeating the same payload and idempotency key returns the original message; changing the payload returns `409`. The key determines application scope; it cannot submit or read another application's mail.

| Endpoint | Scope |
| --- | --- |
| `POST /v1/messages` | `message.send`; `message.otp` additionally required for OTP priority |
| `GET /v1/messages/{id}` | `message.view` |
| `POST /v1/messages/{id}/cancel` | `message.cancel` |
| `POST /v1/delivery-events` | `delivery.report` |

Message fields: `to`, `template` (slug or ID), optional `template_version`, string-valued `variables`, optional `class` (`transactional` or `otp`), `schedule_at`, and `expires_at` (RFC3339). OTP expiry cannot exceed five minutes. Bulk messages require an approved dashboard campaign. `429` means admission was rejected and no message was queued; `Retry-After` is supplied. Quotas are measured over rolling 24-hour and one-hour windows. Bulk campaigns reserve 20% of queue capacity and 10% of daily quota for transactional traffic. One of every five dispatch opportunities favors bulk to avoid starvation while OTP and transactional messages receive the others.

Email variables use `{{name}}` syntax with no executable template functions. HTML values are escaped. Names ending in `url` require an HTTP(S) URL. Template and subject editing creates a new version; a campaign uses the versions selected during mapping, preserved through approval.

## Campaign imports

A **bulk campaign** maps arbitrary CSV/Sheet columns or constant values to the variables required by a chosen template. Try `examples/contacts.csv` with the welcome template and map `name` and `email`.

For the optional certificate workflow, import `examples/certificates.csv` into a new certificate campaign after creating/importing its templates. Map email, school name, certificate link, event, and award. Create templates whose slugs match `event.award`, for example `example.participation`. Import snapshots support at most 5,000 recipients and 52 columns; CSV files must be at most 8 MiB.

Google Sheets import needs a Google service-account JSON at `secrets/google-service-account.json`, the Sheets API enabled, and read access granted to that account. Set `GOOGLE_CREDENTIALS_FILE=/run/secrets/google-service-account.json` in `.env` and recreate web/worker. Only read-only Sheets scope is requested. The URL's `gid` selects the tab. EDP does not change the source sheet, generate certificates, or broaden Drive sharing. Confirm that recipients can open the certificate links during test/review.

Invalid/duplicate rows block approval unless excluded. Reimporting or remapping requires draft state. An approved but unsent campaign can return to draft. Once started, the frozen messages are persisted atomically. Pause/resume reuses those jobs. Cancellation affects remaining queued/retrying messages; in-flight SMTP transactions cannot be recalled.

## PMG tracking and secondary routing

The worker polls the [PMG tracking API](https://pmg.proxmox.com/pmg-docs/api-viewer/index.html) every 30 seconds when configured. It correlates stable Message-ID, envelope sender, and recipient, reads tracker detail, and records remote acceptance, deferral, or bounce. PMG API credentials need read-only audit access; this integration does not modify PMG queues. Validate gateway credentials and tracker access in Settings before enabling polling.

Configure a secondary SMTP provider in Settings and explicitly enable fallback per application. Only a confirmed permanent transport rejection associated with IP reputation can requeue once through that provider. Temporary deferrals, invalid recipients, filtering/quarantine, and uncertain acceptance never trigger fallback. Sending still obeys shared/application limits, expiration, and campaign pause/cancel controls. Secondary acceptance remains relay acceptance; its final outcome needs the reporting integration supported by that provider.

## Gateway feedback and uncertain outcomes

`relay_accepted` means the SMTP gateway returned final acceptance. Network failure after DATA or a lost worker lease becomes `unknown`, which is never automatically resent. Use the stable MIME `Message-ID` / `X-EDP-Message-ID` to check gateway logs before sending a replacement.

A trusted gateway integration can submit:

```json
{"message_id":"msg_…","status":"remote_accepted","event_key":"unique-gateway-event","detail":"Postfix transport reported sent"}
```

`bounced` is also accepted. Reports are scoped to the application's dedicated `delivery.report` key and deduplicated by event key. Reusing a key for a different message/status is rejected.

`scripts/pmg-feedback.py` consumes Postfix logs, correlates queue IDs with EDP Message-IDs, and retains unsuccessful reports in a SQLite outbox. A timer retries independently of new log lines. Run one adapter per application reporting key against that application's mail stream. Configure `EDP_URL`, `EDP_REPORT_KEY`, `EDP_FEEDBACK_STATE`, and `EDP_INTERNAL_RELAYS` (comma-separated exact `relay=` values for PMG internal filter hops). External SMTP replies containing “queued as” still count as remote acceptance. Validate the deployed gateway's log format and internal relay list before enabling this adapter. Optional `BOUNCE_DOMAIN` changes the envelope sender to `bounce+<message_id>@<domain>`; that domain's routing must be configured on the gateway.

## Production and operations

Copy `.env.production.example` to `.env.production`, set a real PostgreSQL connection, `PUBLIC_URL`, a stable 32-byte base64 `SECRET_KEY`, root bootstrap credentials, a primary SMTP endpoint (or configure it after root login). Configure SMTP authentication, PMG, and OIDC through Settings or bootstrap environment values. Put HTTPS termination in front of the localhost-bound web port. Development defaults are not used by `compose.production.yaml`.

```sh
EDP_IMAGE=ghcr.io/mwit-msc/email-delivery-platform:v1.0.0 \
  docker compose -f compose.production.yaml up -d
```

Only migrations and the root are seeded in production. Configure authorized applications/senders, roles, and quotas in the dashboard. PostgreSQL must be backed up; it owns users, policies, jobs, snapshots, and delivery history. `scripts/backup.sh` backs up the local stack in custom pg_dump format with private file permissions. `scripts/restore-check.sh BACKUP.dump` verifies the dump in a new database without replacing live data. Production backups should use the deployed PostgreSQL backup system and retain `SECRET_KEY` separately.

`GET /healthz` checks the database. `GET /metrics` accepts `Authorization: Bearer <METRICS_TOKEN>` or a platform-admin session. `deploy/prometheus-rules.yml` contains queue-delay, uncertain-delivery, and availability alerts; connect those rules to the existing monitoring stack. After `RETENTION_DAYS` (default 30), terminal message bodies are erased; status history remains. The worker maintains cleanup every 30 seconds. Message content and passwords are not written to runtime logs.

## Release automation

`.github/workflows/release.yml` runs on pushes to **`release`**, tests against PostgreSQL, builds **linux/amd64 only**, and publishes to **GHCR / GitHub Packages** using `GITHUB_TOKEN`. Local Docker builds use the host architecture.

- Initial release: `v1.0.0`.
- `feat:` / `feat(scope):` commits since the last tag: minor bump.
- `!:` or `BREAKING CHANGE:`: major bump.
- Other changes: patch bump.
- Tags: immutable `vX.Y.Z`, immutable `sha-<commit>`, and moving `release` only for the current release-branch head.
- Existing published versions are reused on rerun; version ownership is checked. Old workflow reruns cannot roll back the `release` alias.
- A Git tag and GitHub release are created after container publication, with the digest recorded in release notes.

Actions are pinned to commit SHAs. The workflow needs repository `contents:write` and `packages:write`; the repository/organization must permit those permissions. Merge or push reviewed changes to `release` to publish the next version. The `main` branch runs CI without publishing containers.

## Verification

```sh
make test
make integration
python3 -m unittest discover -s scripts -p 'test_*.py'
```

Integration tests use an isolated temporary schema in `EDP_TEST_DATABASE_URL` and remove that test schema afterward. Tests cover 3,000 concurrent submissions, shared throttles, duplicate prevention, tenant RBAC, root seed preservation, local recovery, OIDC signatures/PKCE and refresh-token persistence, immutable template/campaign snapshots, pause/resume, expired/uncertain jobs, delivery-feedback authentication, and release versioning.

## Generic deployments and licensing

The core supports generic OIDC providers, configurable labels/group claims, arbitrary bulk template variables, SMTP relays, and optional PMG tracking. Organization-specific certificate content and logos are optional local inputs, excluded from the repository and release image. Set `SEED_CERTIFICATES_FILE`/`SEED_LOGO_FILE` only for a deployment that owns those assets.

EDP is available under the [MIT license](LICENSE). `third_party/` and bundled font OFL files contain dependency licenses; these are also copied into the image under `/usr/share/licenses/edp/`.
