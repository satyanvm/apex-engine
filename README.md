# apex-engine

**Compile plain English into a checked workflow over your own tools, then run it so that it survives crashes.**

Point apex-engine at an OpenAPI spec or an MCP server. Send it a sentence. It returns a typed DAG in which every step is a real operation from your tools and every data reference has been checked. Run the DAG on Temporal: one durable activity per node, with parallel branches, loops, gates and human approvals.

```text
"Get all late orders, check the refund policy for each,
 ask a human to approve, refund them, and summarize."
                          │
                 ROUTE → MAP → REDUCE → VALIDATE ⇄ REPAIR
                          │
trigger → list orders → loop ─┬→ policy lookup (MCP) → approval → refund (API)
                              └→ summary (after the loop)
```

## Why compile instead of letting an agent call tools

| | Agent loop | apex-engine |
|---|---|---|
| Know what it will do before it acts | No | Yes, the whole plan is reviewable |
| Run it again | Pays for models and may choose differently | Same graph, model calls only where a node asks for one |
| A worker dies mid-run | Usually starts over | Resumes at the node that was running |
| A hallucinated tool or field | Fails at run time, maybe after side effects | Rejected at compile time and sent back to the model to repair |

A compile either produces a graph that passes validation or fails with the reason. There is no fallback graph, and a failed step never looks like a successful one.

## Quick start

```bash
cp .env.example .env   # add GEMINI_API_KEY, APEX_SECRET_KEY, APEX_API_KEYS, POSTGRES_PASSWORD
docker compose up -d --build
```

```bash
export KEY=your-api-key API=http://localhost:8700

# 1. Import your tools
CAT=$(curl -s $API/v1/catalogs -H "Authorization: Bearer $KEY" \
  -d '{"name":"Acme","source":{"type":"openapi","url":"https://api.acme.com/openapi.json"}}' | jq -r .id)

# 2. Store the credential (sealed with AES-GCM, never returned, never in workflow history)
curl -s -X PUT $API/v1/catalogs/$CAT/credentials -H "Authorization: Bearer $KEY" \
  -d '{"headers":{"Authorization":"Bearer ..."}}'

# 3. Compile
curl -s $API/v1/compile -H "Authorization: Bearer $KEY" \
  -d "{\"prompt\":\"Email me a summary of yesterday's failed payments\",\"catalogIds\":[\"$CAT\"]}" | jq .plan

# 4. Run it: POST the plan to /v1/runs (see docs/API.md)
```

The full contract, including streaming compile, approvals and schedules, is in [docs/API.md](docs/API.md).

## What has been measured

These came from this repository on 2026-11-08, on a laptop, against real Postgres and a real Temporal server.

- **Live compile:** "get late orders, check the policy for each, approve, refund, summarize" over one OpenAPI catalog and one MCP server compiled in **18 s** into a 7-node graph with no repair rounds (`gemini-3.1-flash-lite` for selection, `gemini-3.5-flash` for the graph). The run then executed it: both refunds went out once each, after approval.
- **Crash recovery:** `scripts/crash-demo.sh` sends `SIGKILL` to the worker during the second of three nodes. A new worker finishes the run. The first node's upstream call was made once, and only the in-flight node was retried.

One graph is an example, not a benchmark. Compile quality has not yet been measured here across a golden set.

## Guarantees and limits

- **Isolation:** every catalog, run and schedule belongs to the tenant of the API key. Another tenant gets a 404.
- **SSRF:** user-supplied URLs are checked after DNS resolution, at connect time. Private, loopback, link-local and metadata addresses are refused unless `APEX_ALLOW_PRIVATE_NETWORK=1`, which is for local development only.
- **Credentials** are attached only to the base URL of the catalog that owns them, and credentialed requests do not follow redirects.
- **At-least-once execution.** If a worker dies during a call, that call is retried. Non-GET API calls carry a stable `Idempotency-Key`, but the upstream has to honor it.
- **Model output can flow into later steps.** Text from an `agent.llm` node or a tool can be interpolated downstream, so put `logic.approval` before anything irreversible.
- **Sizes:** response bodies up to 512 KB, data passed to a node up to 1.5 MB, loops up to 500 items, graphs up to 500 nodes. Loop items run one at a time.
- **OpenAPI 3.x only**, and MCP over Streamable HTTP only. `attempts` on a loop body node counts every item.

## Layout

| Path | What it does |
|---|---|
| `internal/compiler` | route → map → reduce, validation, repair, sectioned authoring for large graphs |
| `internal/catalog` | OpenAPI and MCP import, MCP client |
| `internal/runtime` | deterministic Temporal workflow and node activities |
| `internal/httpapi` | the public API |
| `internal/netguard` | SSRF-safe HTTP clients |
| `cmd/demo-api` | a sample Orders API and MCP server for trying it out |

## Tests

```bash
go test ./...
# real Postgres and Temporal; only model calls are stubbed
APEX_TEST_DATABASE_URL=postgres://... TEMPORAL_TEST_HOSTPORT=localhost:7233 \
  go test -p 1 -tags integration ./internal/httpapi
```

## Deploy

On a host with Docker and a DNS record pointing at it, set `DOMAIN` in `.env` and run:

```bash
docker compose -f docker-compose.yml -f docker-compose.prod.yml up -d --build
```

Only Caddy is published, with automatic HTTPS. Set `APEX_ROLE=api` or `worker` to scale the two halves separately. Back up `APEX_SECRET_KEY`: without it, stored credentials cannot be read.

## License

MIT
