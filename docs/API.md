# apex-engine HTTP API (v1)

All `/v1/*` routes need `Authorization: Bearer <api key>`. A key maps to one
tenant (`APEX_API_KEYS=key:tenant,key2:tenant2`); every resource is scoped to
the tenant. Errors are `{"error":{"code":"bad_request","message":"..."}}` with
a 4xx/5xx status. Timestamps are RFC3339.

## Types

    Node = { key, type, name, dependsOn: string[], config: object, col?: int, row?: int }
    Dag  = { nodes: Node[], defaultInput?: object }

Templates inside `config`: `{{input.x}}`, `{{outputs.<nodeKey>.<field>}}`.

## Health

    GET /healthz -> {"status":"ok"}          (no auth)

## Catalogs: the tools the planner may use

    POST /v1/catalogs
      {"name":"Acme API","source":{"type":"openapi","url":"https://..."}}
      {"name":"Acme API","source":{"type":"openapi","spec":{...}},"baseUrl":"https://api.acme.com"}
      {"name":"Docs","source":{"type":"mcp","url":"https://.../mcp","headers":{"Authorization":"Bearer x"}}}
      -> 201 Catalog

    Catalog = { id, name, kind:"openapi"|"mcp", baseUrl, nodeCount, hasCredentials,
                credentialHeaders: string[], createdAt, nodes?: CatalogNode[] }
    CatalogNode = { id, label, description, kind, runtime, config: Field[], outputs: string[] }
    Field = { key, label, control, required, help? }

    GET    /v1/catalogs            -> {"catalogs":[Catalog]}     (no nodes)
    GET    /v1/catalogs/{id}       -> Catalog                    (with nodes)
    DELETE /v1/catalogs/{id}       -> 204
    PUT    /v1/catalogs/{id}/credentials {"headers":{"Authorization":"Bearer x"}} -> Catalog
           Write-only, sealed with AES-GCM. Values are never returned.

## Compile: prompt -> validated DAG

    POST /v1/compile {"prompt":"...","catalogIds":["cat_..."],"name?":"..."}
      -> {"plan":Plan,"trace":Trace}

    Plan  = { name, description, nodes: Node[], defaultInput, schedule: string|null, tags: string[] }
    Trace = { durationMs, stages: Stage[], repairs: int, models: {map, reduce} }
    Stage = { stage:"route"|"map"|"reduce"|"validate"|"repair", status:"done"|"failed",
              durationMs, detail: string }

    POST /v1/compile/stream  (same body) -> text/event-stream
      event: stage   data: {"stage":"route","status":"start"|"done"|"failed","detail":"..."}
      event: result  data: {"plan":Plan,"trace":Trace}
      event: error   data: {"message":"..."}

    POST /v1/validate {"dag":Dag,"catalogIds":[...]} -> {"valid":bool,"problems":[string]}

    A compile that cannot produce a valid graph fails with the reason. There is
    no fallback graph.

## Runs: durable execution

    POST /v1/runs {"dag":Dag,"catalogIds":[...],"input":{...},"idempotencyKey?":"..."}
      -> 202 Run

    Run  = { id, status, createdAt, finishedAt: string|null, error: string|null,
             input: object, nodes: NodeRun[] }
    status: queued|running|waiting_approval|succeeded|failed|cancelled
    NodeRun = { key, type, name, status, startedAt, finishedAt, output, error, attempts }
    node status: pending|running|waiting_approval|succeeded|failed|skipped

    GET  /v1/runs?limit=50                  -> {"runs":[Run]}   (nodes omitted)
    GET  /v1/runs/{id}                      -> Run
    POST /v1/runs/{id}/cancel               -> Run
    POST /v1/runs/{id}/nodes/{key}/approve  {"approved":true,"note?":"..."} -> Run

    The DAG is validated before it starts; an invalid DAG returns 422 with
    {"error":{"code":"invalid_dag","message":"...","problems":[...]}}.

## Schedules

    POST   /v1/schedules {"cron":"0 9 * * 1","dag":Dag,"catalogIds":[...],"input":{}} -> 201 Schedule
    Schedule = { id, cron, createdAt, paused }
    GET    /v1/schedules           -> {"schedules":[Schedule]}
    DELETE /v1/schedules/{id}      -> 204

## Built-in node types

| type | config | outputs |
|---|---|---|
| `trigger.manual` | | `input` |
| `trigger.schedule` | `cron` | `input` |
| `agent.llm` | `system`, `prompt` | `text`, `model` |
| `tool.http` | `method`, `url`, `headers`, `body` | `status`, `body` |
| `logic.branch` | `condition` | `passed`, `condition` |
| `logic.filter` | `condition` | `passed`, `condition` |
| `logic.loop` | `items` | `items`, `count` |
| `logic.approval` | `message` | `approved`, `note` |

Imported tools appear as `api.<catalog>.<operationId>` and
`mcp.<catalog>.<tool>`; their outputs are `status`/`body` and `result`.
