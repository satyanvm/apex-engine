# apex-engine HTTP API (v1)

All `/v1/*` routes need `Authorization: Bearer <api key>`. A key belongs to one
tenant and every resource is scoped to the tenant. Errors are
`{"error":{"code":"bad_request","message":"..."}}` with a 4xx/5xx status.
Timestamps are RFC3339.

## Keys and roles

Keys in `APEX_API_KEYS=key:tenant,key2:tenant2` are each tenant's administrator
(they show as `root`). Make narrower keys for people and systems:

    POST   /v1/keys {"name":"asha","role":"approver"} -> 201 ApiKey + {"key":"apx_..."}
           The key is in this response only. The engine stores its SHA-256.
    GET    /v1/keys         -> {"keys":[ApiKey]}
    DELETE /v1/keys/{id}    -> ApiKey      (revoked; the key stops working at once)

    ApiKey = { id, name, role, prefix, createdBy, createdAt, lastUsedAt, revokedAt }

| role | can |
|---|---|
| `viewer` | read everything except keys and the audit log |
| `approver` | viewer, and decide approvals |
| `editor` | viewer, and compile, save workflows, start, cancel and retry runs, manage schedules and webhooks |
| `operator` | editor and approver |
| `admin` | everything: also add and remove tools, set their credentials, manage keys, read the audit log |

An editor cannot approve, so the person who starts a run need not be the person
who signs it off. A key whose role is too low gets `403 forbidden`.

`GET /v1/status` includes `actor` and `role` for the key that asked.

### The platform key: one key for a product's own customers

`APEX_PLATFORM_KEY` (32+ characters) acts for any tenant. It is for a product
whose customers are the tenants, so nothing has to be created when a customer
signs up. Each request says whom it is for:

    Authorization: Bearer <platform key>
    X-Apex-Tenant: cust_42            required. 1-64 letters, digits or . _ : -
    X-Apex-Actor:  dana@acme.com      optional. The end user; "platform" when left out
    X-Apex-Role:   editor             optional. viewer|approver|editor|operator|admin; admin when left out

A tenant exists from the first request that names it. The actor is what the
engine records: `startedBy` on a run, `decidedBy` on an approval, `actor` in the
audit log. The role is enforced exactly as a key's role is. A platform key
without `X-Apex-Tenant` gets `400`. It can do anything to any tenant, so keep
it on your server and never send it to a browser.

In Go, `engine.Engine.Client(tenant)` gives the same thing inside the process,
with no key and no network; `.As(actor, role)` narrows it to one end user.

### Offboarding a tenant

    DELETE /v1/tenant?confirm=<the tenant's name>     (admin)
      -> {"tenant":"cust_42","runsStopped":2,"deleted":{"runs":41,"workflows":3,"catalogs":2,...}}

Stops the tenant's unfinished runs, removes its schedules, then deletes every
tool (with its sealed credentials), workflow, run, schedule, key, counter and
audit entry. It cannot be undone. The record that it happened is in the
engine's own log, since the tenant's audit log goes too.

### Guard rails

| setting | effect |
|---|---|
| `APEX_MAX_OPEN_RUNS` | The most unfinished runs one tenant may have. Starting another returns `429 too_many_runs` with `Retry-After`. Unset means no cap |
| `APEX_RETENTION_DAYS` | Finished runs, with their steps and stored outputs, are deleted this many days after finishing. Checked at start and hourly |
| `APEX_AUDIT_RETENTION_DAYS` | The same for audit entries |
| `APEX_REQUIRE_APPROVAL` | `deletes`: a graph with a DELETE that has no `logic.approval` upstream is invalid. `changes`: the same for any write, MCP tool not marked read-only, or piece action. Compiles are told and add the approval |

## Audit log

    GET /v1/audit?limit=100&before=<id>&action=run.&actor=asha&target=run_...
      -> {"entries":[{ id, at, actor, action, target, detail }]}     (admin)

Every change is recorded with the name of the key that made it: `run.start`,
`run.cancel`, `run.approve`, `run.reject`, `workflow.create|update|delete`,
`workflow.webhook_on|off`, `schedule.create|delete`, `catalog.create|delete`,
`catalog.credentials`, `catalog.oauth_start`, `catalog.oauth_connected`,
`key.create`, `key.revoke`. Runs started by a webhook have the actor `webhook`;
scheduled runs have `startedBy: "schedule"` on the run. `action` matches as a
prefix. Entries are never edited or deleted by the engine.

## Metrics

    GET /metrics     Authorization: Bearer <APEX_METRICS_TOKEN>

Prometheus text for every tenant, so it takes its own token and not an API key.
404 when `APEX_METRICS_TOKEN` is empty.

    apex_runs_started_total{tenant,source}       apex_runs_open{tenant,status}
    apex_runs_finished_total{tenant,status}      apex_oldest_waiting_approval_seconds{tenant}
    apex_compiles_total{tenant,outcome}          apex_compile_milliseconds_total{tenant}

## Types

    Node = { key, type, name, dependsOn: string[], config: object, col?: int, row?: int }
    Dag  = { nodes: Node[], defaultInput?: object }

The machine-readable contract is `GET /openapi.yaml`. Go programs can use
package `client` instead of this API directly.

Templates inside `config`: `{{input.x}}`, `{{outputs.<nodeKey>.<field>}}`.

## Health

    GET /healthz -> {"status":"ok"}          (no auth)

## Status

    GET /v1/status
      -> {"models":{"ready":bool,"problem?":"...","map":"...","reduce":"...","agent":"..."},
          "demoAvailable":bool}

    POST /v1/demo -> 201 {"workflowId":"wfl_...","catalogIds":[...]}
      Installs the sample tools and workflow. Repeating it changes nothing (200).
      404 unless the engine was started with APEX_DEMO_URL.

## Catalogs: the tools the planner may use

    POST /v1/catalogs
      {"name":"Acme API","source":{"type":"openapi","url":"https://..."}}
      {"name":"Acme API","source":{"type":"openapi","spec":{...}},"baseUrl":"https://api.acme.com"}
      {"name":"Acme API","source":{"type":"openapi","spec":"openapi: 3.0.3 ..."}}   (a YAML string)
      {"name":"Docs","source":{"type":"mcp","url":"https://.../mcp","headers":{"Authorization":"Bearer x"}}}
      {"name":"Slack","source":{"type":"piece","piece":"slack","auth":<connection>}}
      -> 201 Catalog                                                    (admin)

    Catalog = { id, name, kind:"openapi"|"mcp"|"piece", baseUrl, nodeCount, hasCredentials,
                auth:"none"|"headers"|"oauth2"|"oauth2_pending"|"piece",
                credentialHeaders: string[], createdAt, nodes?: CatalogNode[] }
    CatalogNode = { id, label, description, kind, runtime, config: Field[], outputs: string[] }
    Field = { key, label, control, required, help? }

    GET    /v1/catalogs            -> {"catalogs":[Catalog]}     (no nodes)
    GET    /v1/catalogs/{id}       -> Catalog                    (with nodes)
    DELETE /v1/catalogs/{id}       -> 204
    PUT    /v1/catalogs/{id}/credentials -> Catalog                   (admin)
           {"headers":{"Authorization":"Bearer x"}}
           {"query":{"api_key":"x"}}                 a key some APIs take in the URL
           {"oauth":{"grant":"client_credentials","tokenUrl":"https://...","clientId":"...",
                     "clientSecret":"...","scope":"...","audience":"..."}}
           {"oauth":{"grant":"refresh_token","tokenUrl":"...","clientId":"...","clientSecret":"...",
                     "refreshToken":"..."}}
           {"piece":<connection>}                    for a piece
           Replaces everything stored. Write-only, sealed with AES-GCM; values are
           never returned. OAuth settings are tried first: wrong ones are refused
           with 422 oauth_failed and the provider's reason, and nothing is saved.

### OAuth

With an `oauth` block the engine fetches a token, caches it sealed, refreshes it a
minute before it expires, and stores a rotated refresh token when the provider
issues one. If the API answers 401, the engine fetches a new token and repeats the
call once, inside the same step. A refused refresh fails the step with a message
that says to reconnect; it is not retried.

For a provider that needs a person to consent:

    POST /v1/catalogs/{id}/oauth/authorize                             (admin)
      {"authUrl":"https://.../authorize","tokenUrl":"https://.../token","clientId":"...",
       "clientSecret":"...","scope":"...","authParams":{"access_type":"offline"}}
      -> {"authorizeUrl":"...","redirectUrl":"<APEX_PUBLIC_URL>/oauth/callback","expiresInSeconds":900}

Register `redirectUrl` with the provider, then open `authorizeUrl` in a browser.
The provider sends the person to `GET /oauth/callback`, which exchanges the code
(with PKCE) and stores the tokens. `auth` goes from `oauth2_pending` to `oauth2`.
The state in the link is the only credential the callback takes, expires in 15
minutes and works once.

### Activepieces pieces

    GET /v1/pieces?q=slack -> {"pieces":[{ name, displayName, version, ... }]}

Lists the pieces installed on the pieces runner (`pieces/`). 503 when the engine
has no `APEX_PIECES_URL`. Adding one as a catalog turns each of its actions into a
tool named `piece.<catalog>.<action>` whose output is `result`.

`<connection>` is whatever the piece's own connection form asks for: a string for
an API key, `{"username":..,"password":..}` for basic auth, or an object of fields
for a custom connection. For a piece that uses OAuth, add the piece without a
connection and run the authorize route above with the `authUrl`, `tokenUrl` and
scopes the piece lists; the engine then passes it a fresh `access_token` on every
call. Triggers of a piece are not supported, only actions. A dropdown that
Activepieces fills in from the API (a Slack channel, say) takes the id here.

### Paging

When an operation's query parameters look like paging (`page`, `offset`, `cursor`,
`starting_after`, `pageToken` and their relatives), its node gains two settings and
three outputs:

    allPages: "true"      collect every page (default: first page only)
    maxPages: 20          stop after this many (at most 200)
    -> items              the list, from one page or all of them
       pages, truncated   how many pages were read, and whether maxPages cut it short

The engine follows a `Link: <...>; rel="next"` header, a next cursor in the body
(`next_cursor`, `nextPageToken`, `meta.next_cursor`, `paging.cursors.after`, ...),
`has_more` with the last item's id, or counts pages and offsets itself. It stops
when a page is empty, short, or the same as the one before. A next link on another
host is refused, because credentials only go to the catalog's own address. This is
a heuristic: OpenAPI has no standard way to describe paging. Collected items are
capped at 1 MB.

## Compile: prompt -> validated DAG

    POST /v1/compile {"prompt":"...","catalogIds":["cat_..."],"name?":"...","base?":Plan}
      -> {"plan":Plan,"trace":Trace,"review":Review,"diff?":Diff}

    Plan  = { name, description, nodes: Node[], defaultInput, schedule: string|null, tags: string[] }
    Trace = { durationMs, stages: Stage[], repairs: int, models: {map, reduce} }
    Stage = { stage:"route"|"map"|"reduce"|"validate"|"repair", status:"done"|"failed",
              durationMs, detail: string }

    POST /v1/compile/stream  (same body) -> text/event-stream
      event: stage   data: {"stage":"route","status":"start"|"done"|"failed","detail":"..."}
      event: result  data: {"plan":Plan,"trace":Trace}
      event: error   data: {"message":"..."}

    POST /v1/validate {"dag":Dag,"catalogIds":[...]} -> {"valid":bool,"problems":[string],"review?":Review}

    A compile that cannot produce a valid graph fails with the reason. There is
    no fallback graph.

### Review: what a graph does, in words

    Review     = { steps: ReviewStep[], reads: int, changes: int, approvals: int, warnings: string[] }
    ReviewStep = { node, kind, text, tool?, perItem: bool, afterApproval: bool }
    kind: start|read|write|delete|action|model|approval|loop|gate|wait|transform

Every compile returns a review, and `validate` returns one for a valid graph, so
a host can describe a graph it stored or edited. It is worked out from the graph
and the tools, with no model, so it always matches what will run. `write`,
`delete` and `action` are the kinds that can change something outside the
engine; an `action` is an MCP tool or piece action that does not say whether it
does. `warnings` names each such step that has no approval in front of it.

### Changing a plan

Send an existing plan as `base` and say what to change in `prompt`. The model
is given the current graph and told to keep every step the change does not
concern. The result is validated and repaired like any compile, and carries

    Diff = { added: string[], removed: string[], changed: string[], unchanged: int }   (step keys)

The whole graph is rewritten in one model call, so this suits graphs of a few
dozen steps. Read the diff: a weak model may change more than it was asked to.

## Runs: durable execution

Run IDs and approval endpoints stay the same when workers are replaced. The
DAG runner pins its command path in Temporal history so a compatible worker
can finish a run started by an earlier engine.

    POST /v1/runs {"dag":Dag,"catalogIds":[...],"input":{...},"idempotencyKey?":"...","name?":"...","callbackUrl?":"...","preview?":true}
      -> 202 Run

    Run  = { id, status, name, workflowId: string|null, source: "api"|"webhook"|"schedule"|"retry",
             startedBy, resumeOf: string|null, failedItems: int, preview: bool,
             createdAt, finishedAt: string|null, error: string|null, input: object,
             catalogIds: string[], nodes: NodeRun[], dag: Node[] }
    status: queued|running|waiting_approval|succeeded|failed|cancelled
    NodeRun = { key, type, name, status, startedAt, finishedAt, output, error, attempts,
                reused?, itemsSucceeded?, itemsFailed? }
    node status: pending|running|waiting_approval|succeeded|failed|skipped

    GET  /v1/runs?limit=50&status=&workflowId=   -> {"runs":[Run]}   (nodes and dag omitted)
    GET  /v1/runs/{id}                      -> Run
    POST /v1/runs/{id}/cancel               -> Run
    POST /v1/runs/{id}/retry                -> 202 Run   (a new run; 200 if already retried)
    GET  /v1/runs/{id}/nodes/{key}/items?status=failed&limit=100&offset=0
                                            -> {"items":[{ index, status, output, error, attempts, ... }]}
    POST /v1/runs/{id}/nodes/{key}/approve  {"approved":true,"note?":"..."} -> Run   (approver)

### Preview: what a run would do, without doing it

    POST /v1/runs {..., "preview": true}            (also on POST /v1/workflows/{id}/runs)
    GET  /v1/runs/{id}/preview
      -> { runId, status, error, done, counts: {call, approval, wait, unknown}, truncated,
           actions: [{ node, name, type, item: int|null, kind, detail }] }

A preview run makes every read for real and sends no write, so its loops run
over the records that are actually there. What it holds back it records:

| kind | what was held back | detail |
|---|---|---|
| `call` | a write | `tool`, `operation`, `method`, `url`, `body` (API and HTTP steps); `arguments` (MCP tools and pieces) |
| `approval` | a question to a person; the preview carries on as if the answer were yes | `message` |
| `wait` | a pause | `seconds` |
| `unknown` | a step that needs the response of a write that was not sent | `reason`, `detail` |

What counts as a write: any HTTP method other than GET, HEAD and OPTIONS; any
MCP tool its server does not mark `readOnlyHint`; every piece action. Credentials
never appear in an action. `agent.llm` steps do run. Nobody is notified and no
approval event is sent. Retrying a failed preview gives another preview, never
a real run. `GET .../preview` on a run that was not a preview returns `409`.

### When something fails

A run that fails or is cancelled can be continued with `retry`. The new run
reuses the result of every step, and every loop item, that succeeded: those are
marked `reused` and are not executed again. Only what failed or never ran is
executed. Calls keep the `Idempotency-Key` they had in the first run, so an API
that honours the header will not repeat a write that was in flight when the run
died. A run that succeeded cannot be retried (409).

A loop stops at the first failed item unless it has `"onError": "continue"`.
With that, the failure is recorded and the loop carries on. The run then
finishes `succeeded` with `failedItems` above zero, so check that field: the
loop's output has `succeeded`, `failed` and `errors`, and each item is in the
`items` route above.

An approval is recorded with `decidedBy`, the name of the key that decided.
Sending a decision twice is safe: each wait has its own token and the second
copy answers a wait that is already over.

### Limits of one run

A run fails with a plain message, rather than being killed by Temporal, when it
has moved 30 MB through its steps or run 10,000 of them. A loop takes at most
500 items and cannot contain another loop.

    The DAG is validated before it starts; an invalid DAG returns 422 with
    {"error":{"code":"invalid_dag","message":"...","problems":[...]}}.

## Events

    A run started with a callbackUrl (also accepted when saving a workflow, on
    POST /v1/workflows/{id}/runs and on schedules) receives:

    POST <callbackUrl>
      {"event":"run.waiting_approval"|"run.finished","runId":"...","status":"...",
       "error":"...","node":"...","message":"...","delivery":"...","at":"..."}
      X-Apex-Event:     the event name
      X-Apex-Delivery:  the same on every retry, so a receiver can drop repeats
      X-Apex-Signature: sha256=<hex HMAC-SHA256 of the raw body>

    GET /v1/callback-secret -> {"secret":"<hex key>"}     one key per tenant

    A 2xx ends delivery. A 408, 429, 5xx or network error is retried up to 6
    times over about a minute. Any other answer is final. Delivery never affects
    the run's outcome. Addresses are checked like every user-supplied URL, and
    redirects are not followed.

## Workflows: saved graphs

    POST /v1/workflows {"name":"...","description?":"...","prompt?":"...","nodes":Node[],
                        "defaultInput?":{},"catalogIds":[...],"callbackUrl?":"..."} -> 201 Workflow
    Workflow = { id, name, description, prompt, nodes, defaultInput, catalogIds,
                 webhookPath: string|null, createdAt, updatedAt, lastRun? }

    GET    /v1/workflows             -> {"workflows":[Workflow]}
    GET    /v1/workflows/{id}        -> Workflow + {"schedules":[Schedule]}
    PUT    /v1/workflows/{id}        (same body as POST) -> Workflow
    DELETE /v1/workflows/{id}        -> 204   (removes its schedules; runs stay)
    POST   /v1/workflows/{id}/runs   {"input?":{},"idempotencyKey?":"..."} -> 202 Run

    A graph that fails validation is refused with 422 invalid_dag, so a stored
    workflow is always one that passed.

## Webhooks

    PUT    /v1/workflows/{id}/webhook -> {"webhookPath":"/hooks/whk_..."}
           Turns it on, or replaces the URL if it was on.
    DELETE /v1/workflows/{id}/webhook -> 204

    POST /hooks/{token}      (no Authorization header; the URL is the credential)
      Body: an optional JSON object, which becomes the run input.
      Header: Idempotency-Key, so a repeated delivery starts nothing new.
      -> 202 {"runId":"run_...","status":"queued"}
      404 for an unknown or disabled URL, 429 past 120 runs a minute.

## Schedules

    POST   /v1/schedules {"cron":"0 9 * * 1","workflowId":"wfl_..."} -> 201 Schedule
           Each tick runs the workflow's latest saved graph.
    POST   /v1/schedules {"cron":"0 9 * * 1","dag":Dag,"catalogIds":[...],"input":{}} -> 201 Schedule
           Runs this exact graph forever.
    Schedule = { id, cron, createdAt, paused, workflowId: string|null }    (cron is UTC)
    GET    /v1/schedules           -> {"schedules":[Schedule]}
    DELETE /v1/schedules/{id}      -> 204

## Built-in node types

| type | config | outputs |
|---|---|---|
| `trigger.manual` | | `input` |
| `trigger.schedule` | `cron` | `input` |
| `agent.llm` | `system`, `prompt` | `text`, `model` |
| `tool.http` | `method`, `url`, `headers`, `body` | `status`, `body` |
| `data.transform` | `expression` (jq) | `result` |
| `logic.branch` | `condition` | `passed`, `condition` |
| `logic.filter` | `condition` | `passed`, `condition` |
| `logic.loop` | `items`, `onError` (`stop` or `continue`) | `items`, `count`, `results`, `succeeded`, `failed`, `errors` |
| `logic.wait` | `seconds` or `until` (RFC3339) | `seconds` |
| `logic.approval` | `message` | `approved`, `note`, `decidedBy`, `message` |

Imported tools appear as `api.<catalog>.<operationId>`, `mcp.<catalog>.<tool>` and
`piece.<catalog>.<action>`; their outputs are `status`/`body` (plus `items`, `pages`,
`truncated` for paged lists) and `result`.

**Conditions** are a comparison (`== != > < >= <= contains`), a bare reference
tested for truthiness, or several joined with `&&` and `||`, negated with `!` and
grouped with parentheses: `outputs.fetch.status == 200 && outputs.fetch.body.total > 100`.
`contains` tests a substring, or membership of a list. A condition that does not
parse, or reads something that is not there, fails the step; it never opens the branch.

**`data.transform`** runs a jq expression over `{input, outputs, item, loop}`, so
upstream data is `.outputs.<dependency key>.<field>`. It is not a `{{template}}`.
It cannot read the environment or files. One result comes back as is, several as
a list. It has three seconds and 512 KB. The compiler rejects an expression that
does not parse, reads a step it does not depend on, or reads a field such as
`.body` at the top, where only `.input`, `.outputs`, `.item` and `.loop` exist.

**`logic.wait`** pauses on a durable timer for at most 30 days and survives restarts.
