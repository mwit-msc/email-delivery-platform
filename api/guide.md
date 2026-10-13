# Email Delivery Platform API

The authoritative request/response contract is `/openapi.yaml`. The interactive
reference is `/docs`. These routes and `/llms.txt` and `/llms-full.txt` require
an EDP login session or a valid application service token. Use
`Authorization: Bearer <service-token>` when reading with an API client.
No token is included in these documents.

## Authentication and application scope

Create a service key in Application settings → Service credentials. Select only
the scopes needed by the integration. `GET /v1/me` returns the key's application,
scopes, and server version. All `/v1` operations use bearer authentication;
an interactive dashboard session alone does not authorize API mutations.
Template, campaign, asset, and webhook IDs must belong to the key's application.
`relay.manage` is a privileged platform-wide scope.

## Prepare templates without sending

Use `template.view` and `template.edit`:

1. `GET /v1/templates` lists active templates. `include_archived=true` includes
   archived templates.
2. `POST /v1/templates` creates a named template with a unique lowercase slug,
   `subject`, `html`, `plain`, and optional editor `project`.
3. `PUT /v1/templates/{id}` creates a new immutable version. Send `version`
   equal to `current_version`; a stale update returns 409.
4. `POST /v1/templates/{id}/preview` renders sample `variables`, optionally an
   unsaved `template` or stored `version`. It does not save or send. `html` is
   the editable sanitized fragment; `document` is the full HTML preview with
   trusted font loading; `plain` and `subject` are also returned.
5. `POST /v1/assets` accepts multipart `file` (PNG/JPEG/GIF, at most 1 MiB and
   16 megapixels). Its public, immutable image URL can be used in templates.
6. `POST /v1/templates/{id}/archive` with `{"archived":true}` hides a template
   from the default gallery and new campaign pickers. Set false to restore.
   Versions and existing campaign rendering are preserved. Direct use by ID
   or slug remains possible.

Variables use `{{variable_name}}`. HTML variables are escaped, not executable.
Variables whose names contain `url` require HTTP(S) URLs. Arbitrary scripts and
stylesheet definitions are removed. Specifying `Noto Sans Thai` in inline
`font-family` loads the bundled font in supported clients; Gmail and clients
that block web fonts use system fallbacks.

## Create draft certificate campaigns

Use `campaign.view` and `campaign.edit` for preparation. Approval and starting
delivery are separate operations and require `campaign.approve` and
`campaign.send` respectively.

1. `POST /v1/campaigns` with `name` and `kind` (`certificate` or `bulk`).
2. Inspect XLSX tabs with `POST /v1/imports/workbook-sheets`, multipart `file`.
3. `POST /v1/campaigns/{id}/import` accepts multipart `file` and optional `sheet`,
   or JSON `rows` (header first), or a configured read-only `sheet_url`.
4. `POST /v1/campaigns/{id}/mapping` validates recipients, links, templates and
   rendered variables. Certificate column indexes are zero-based; -1 means
   no column. School and certificate URL variables are populated automatically.
5. Inspect `GET /v1/campaigns/{id}`, `/progress`, and `/export`. The campaign
   remains a draft and no messages are queued until explicitly approved/started.

Several award rules can select one reusable template. An exact event/award
rule wins over a rule whose event is empty. For example:

```json
{
  "email": 0,
  "school": 1,
  "event": -1,
  "award": 2,
  "url": 3,
  "default_event": "example",
  "rules": [
    {"award": "Winner", "template": "example.certificate", "variables": {"award_text": "Winner"}},
    {"award": "Participation", "template": "example.certificate", "variables": {"award_text": "Participation"}}
  ],
  "skip_rows_without_email": true
}
```

Rule variables override `columns`, which override common `constants`. Variables
are frozen with the selected template versions at approval. Review all row
errors before approval; do not guess missing or invalid addresses. Imports are
limited to 5,000 data rows, 52 columns, and 8 MiB files. XLSX formulas/macros are
not executed. Each tab can become a separate campaign.

`POST /v1/campaigns/{id}/{action}` accepts explicit `approve`, `draft`, `start`,
`pause`, `resume`, `cancel`, `exclude`, or `test`. Send a JSON body (use `{}` when
there are no parameters). A `test` actually sends email and needs message.send.
Only `start` begins a campaign. A scheduled start uses RFC3339 `schedule_at`.

## Individual messages and delivery

`POST /v1/messages` requires `message.send`. Supply `to`, `template`, `variables`
and an `Idempotency-Key` header. Accepted responses follow durable queue commit.
Repeat the same key and identical payload to retrieve the same admission;
changing its payload conflicts. Check `GET /v1/messages/{id}` with message.view.
Cancellation requires message.cancel and is effective only before dispatch.
Messages currently have one To recipient; CC/BCC is not supported.

`relay_accepted` means gateway acceptance. `remote_accepted` means acceptance by
the recipient mail server. Neither proves inbox placement. An `unknown` result
means acceptance was uncertain: do not automatically resend it.

Delivery reports use `delivery.report`. Outgoing webhooks use `webhook.manage`;
they are signed and delivered at least once. Verify signatures and deduplicate
event IDs. Public HTTPS endpoints only are accepted. See the OpenAPI schemas for
the exact signing headers and payload contract.

Named SMTP relays and exact sender-domain rules use `relay.manage`.
`POST /v1/relays/{id}/test` tests connectivity/authentication without sending.
Fallback is opt-in, at most once, and only after a confirmed permanent IP
reputation rejection. Unknown acceptance, deferrals, invalid recipients and
quarantine do not trigger fallback.

## Errors and operational limits

401 means missing/invalid/expired bearer credentials. 403 means insufficient
scope. 404 means an object is absent in this application. 409 means conflicting
versions/state/idempotency. 429 indicates admission controls; honor Retry-After
when supplied. Check the JSON `error` field and the operation's OpenAPI response
contract rather than assuming a failed request was accepted.

Do not place tokens in URLs, generated email HTML, webhook URLs, source control,
or logs. Preparing a draft is separate from approving, starting, and sending.
