# apex-engine

**Compile plain English into a checked workflow over your own tools, then run it so that it survives crashes.**

Point apex-engine at an OpenAPI spec, an MCP server or an Activepieces piece. Send it a sentence. It returns a typed DAG in which every step is a real operation from your tools and every data reference has been checked. Run the DAG on Temporal: one durable activity per node, with parallel branches, loops, gates and human approvals. When a run fails partway, continue it without repeating what already succeeded.

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

Validation is more than "does this tool exist". It rejects a field the spec says the response does not have, a loop over something that is not a list, `{{item}}` outside a loop, a yes/no decision used as an id, a reference written without its braces, and page one read where every page was asked for. Each of those came from a graph a model really wrote; see [evals](evals/README.md).

## Start an engine

You need Docker. It generates every secret and offers to connect a model.

```bash
git clone https://github.com/satyanvm/apex-engine && cd apex-engine
./apex up
eval "$(./apex env)"
```

That gives you an engine at `http://localhost:8700`, with Postgres and Temporal behind it. Already run those? Use the `apex-engine` binary or embed it, as below.

## Use it from your code

The flow is the same everywhere: **import tools, compile a sentence into a plan, review it, run it.** There are four ways in.

### Go client

`client` uses only the standard library.

```go
c := client.New(os.Getenv("APEX_URL"), os.Getenv("APEX_API_KEY"))

res, err := c.Compile(ctx, client.CompileRequest{Prompt: "Refund every late order", CatalogIDs: ids})
if err != nil {
	return err // an error means no plan; there is never a fallback graph
}
run, err := c.StartRun(ctx, client.RunRequest{DAG: res.Plan.DAG(), CatalogIDs: ids})
run, err = c.WaitRun(ctx, run.ID, client.WaitOptions{StopAtApproval: true})
for _, step := range run.Waiting() {
	c.Decide(ctx, run.ID, step.Key, true, "approved by policy")
}
```

Full program: [examples/client](examples/client/main.go).

### Embed it in your Go service

`engine` runs the compiler, API and durable worker inside your process, and you mount the handler wherever you like. You supply Postgres and Temporal.

```go
e, err := engine.New(ctx, engine.Config{
	DatabaseURL:      "postgres://...",
	TemporalHostPort: "localhost:7233",
	SecretKey:        base64Of32Bytes,
	APIKeys:          map[string]string{"a-long-api-key": "acme"},
})
defer e.Close()
mux.Handle("/apex/", http.StripPrefix("/apex", e.Handler()))
```

Full program: [examples/embed](examples/embed/main.go). Set `Role` to `engine.RoleAPI` or `engine.RoleWorker` to scale the halves separately.

### Inside a product that has its own customers

If your product offers automation to its customers, each customer is a tenant and you do not want to mint a key per customer. In Go, ask the engine for a client by customer id. There is no key, no network hop, and nothing to set up when a customer signs up:

```go
e, err := engine.New(ctx, engine.Config{DatabaseURL: "...", SecretKey: "...", MaxOpenRuns: 50})

apex := e.Client(customerID).As(user.Email, "operator") // your login decides who and what role
res, err := apex.Compile(ctx, client.CompileRequest{Prompt: prompt, CatalogIDs: tools})
```

From any other language, give the engine one platform key and name the customer on each request:

```bash
curl -s $APEX_URL/v1/workflows -H "Authorization: Bearer $APEX_PLATFORM_KEY" \
  -H "X-Apex-Tenant: cust_42" -H "X-Apex-Actor: dana@acme.com" -H "X-Apex-Role: editor"
```

The actor is the name the engine records: who started a run, who approved a step, who changed a tool. The role is enforced on every request. `DELETE /v1/tenant?confirm=cust_42` removes everything a customer has when they leave. Full program: [examples/saas](examples/saas/main.go).

### CLI, for terminals and CI

Plans are files, so you can review them in a pull request. Results are JSON on stdout and progress goes to stderr.

```bash
go install github.com/satyanvm/apex-engine/cmd/apexctl@latest

apexctl tools add Orders --openapi https://api.acme.com/openapi.json --header 'Authorization: Bearer ...'
apexctl compile "Refund every late order, asking me first" -o refunds.json
apexctl review refunds.json      # what it does, in plain words
apexctl preview refunds.json     # what it would do to live data; nothing is changed
apexctl run refunds.json --wait
apexctl compile "skip orders over 300" --base refunds.json -o refunds.json   # change it
```

Exit codes: `0` succeeded, `1` failed, `2` bad usage, `3` waiting for approval (with `--stop-at-approval`), `4` finished but some loop items failed. Run `apexctl help` for the rest.

### Any other language

The contract is published at `GET /openapi.yaml` ([source](internal/httpapi/openapi.yaml)), and a test fails if it drifts from the routes the server answers. Generate a client from it, or read [docs/API.md](docs/API.md).

```bash
curl -s $APEX_URL/v1/compile -H "Authorization: Bearer $APEX_API_KEY" \
  -d '{"prompt":"Refund every late order","catalogIds":["cat_..."]}' | jq .plan
```

## Review before anything changes

A compiled graph is only useful if the person responsible can agree to it, and most of them will not read a graph. The engine gives them three things.

**What it does, in words.** Every compile returns a `review`, and `POST /v1/validate` returns one for any graph. No model writes it; it is derived from the graph and the tools, so it cannot disagree with what will run.

```
 1. Start
 2. Read from Orders: List late orders (every page)
 3. Repeat the steps after this for each item of outputs.list.items
 4. Ask a person: "Refund order {{item.id}}?"
 5. Change Orders: Refund an order, once for each item, after a person approves
1 read(s), 1 change(s), 1 approval(s)
```

A step that changes something with no approval in front of it is listed under `warnings`. Set `APEX_REQUIRE_APPROVAL=deletes` (or `changes`) to turn that warning into a rule: such a graph is refused, and a compile is told the rule and adds the approval itself.

**What it would do to real data, without doing it.** Start a run with `"preview": true`. Every read is made for real, so loops run over the records that are actually there. Every write is recorded instead of sent, nobody is asked to approve anything, and waits are skipped. `GET /v1/runs/{id}/preview` then lists the result:

```
would ask   Approve refund [item 0]: Refund order A-1001?
would call  Refund an order [item 0]: POST https://api.acme.com/orders/A-1001/refund {"reason":"late"}
would ask   Approve refund [item 1]: Refund order A-1003?
would call  Refund an order [item 1]: POST https://api.acme.com/orders/A-1003/refund {"reason":"late"}
```

A preview treats every HTTP method except GET, HEAD and OPTIONS as a write. MCP tools run only if their server marks them read-only, and Activepieces actions never run, because neither says reliably whether it changes anything. A step that needs the response of a write the preview held back is reported as `unknown` rather than guessed.

**A way to change it by saying so.** Send the plan back as `base` with a new prompt ("only orders over 100", "also post it to Slack"). The steps the change does not concern are kept as they are, and the response carries a `diff`: which steps were added, removed and changed.

## Events: know when a run needs you

Approvals can take days, so don't poll. Pass `callbackUrl` when you start a run or save a workflow, and the engine POSTs `run.waiting_approval` and `run.finished` to it.

```go
func receive(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	if !client.VerifySignature(secret, body, r.Header.Get("X-Apex-Signature")) {
		http.Error(w, "bad signature", http.StatusUnauthorized)
		return
	}
	// decode a client.Event from body
}
```

Get `secret` from `c.CallbackSecret(ctx)`. Delivery is at least once, retried for about a minute, and a retry keeps the same `delivery` value, so drop repeats. A receiver that is down never fails the run.

## When something fails

Real work fails in the middle: the 37th refund of 200 is declined, or the API goes down.

```bash
apexctl run refunds.json --wait        # fails at item 37
apexctl retry run_...  --wait          # continues: items 1 to 36 are not refunded again
apexctl items run_... refund --status failed
```

- **Continue a failed run.** `retry` starts a new run that reuses the result of every step and every loop item that succeeded. Calls that were in flight keep their original `Idempotency-Key`.
- **Carry on past a bad item.** Give a loop `"onError": "continue"` and a failed item is recorded while the rest go ahead. The run ends `succeeded` with `failedItems` above zero, `apexctl` exits `4`, and every item's outcome is kept.
- **Per-item records.** A loop of 200 leaves 200 records per step, not just the last one.
- **A decision counts once.** Each approval wait has its own token, so a double click or a retried request cannot approve the next item.

## Connecting tools

```bash
apexctl tools add Orders --openapi https://api.acme.com/openapi.json
apexctl tools auth <id> --header 'Authorization: Bearer env:ORDERS_TOKEN'

# OAuth, machine to machine: the engine fetches, caches and refreshes the token
apexctl tools auth <id> --oauth client_credentials --token-url https://idp.acme.com/token \
  --client-id ... --client-secret env:CLIENT_SECRET --scope orders.write

# OAuth with a consent screen: prints an address for a person to open, once
apexctl tools oauth <id> --auth-url https://... --token-url https://... --client-id ... --scope ...

# An Activepieces piece (needs the pieces runner: ./apex pieces)
apexctl tools pieces slack
apexctl tools add "Team Slack" --piece slack
```

Credentials are sealed with AES-GCM, never returned by the API, and sent only to the tool's own address. With OAuth, a 401 makes the engine fetch a new token and repeat the call once; rotated refresh tokens are stored; a refused refresh fails the step with a message that says to reconnect.

List operations that page (by page number, offset, cursor or a `Link` header) get an `allPages` setting, so "every late order" means every one and not the first 20. See [docs/API.md](docs/API.md#paging).

The [pieces runner](pieces/README.md) covers what Activepieces pieces can and cannot do here. Their triggers are not supported.

## Who can do what

`APEX_API_KEYS` holds each tenant's administrator. Make narrower keys for people and systems:

```bash
apexctl keys add asha   --role approver   # may read and decide approvals
apexctl keys add ci-bot --role editor     # may build and run, but not approve
apexctl audit --action run.               # who started, approved and cancelled what
```

Roles are `viewer`, `approver`, `editor`, `operator` (editor and approver) and `admin`. Only an admin can connect tools, set credentials, manage keys or read the audit log. An approval records the name of the key that decided. Every change is written to an audit log the engine never edits.

`GET /metrics` serves Prometheus counters and gauges (runs started and finished, compiles, runs open, the age of the oldest run waiting for a person) to whoever presents `APEX_METRICS_TOKEN`.

There are no user accounts or single sign-on: identity is the name on an API key, or the actor your product names with a platform key.

Guard rails for running it for other people:

| Setting | What it does |
|---|---|
| `APEX_PLATFORM_KEY` | One key that acts for any tenant; see [above](#inside-a-product-that-has-its-own-customers) |
| `APEX_MAX_OPEN_RUNS` | The most unfinished runs one tenant may have. The next start gets `429 too_many_runs`, so one customer cannot fill the queue |
| `APEX_RETENTION_DAYS` | Finished runs, with every step's stored output, are deleted this many days after they finish |
| `APEX_AUDIT_RETENTION_DAYS` | The same for the audit log |
| `APEX_REQUIRE_APPROVAL` | `deletes` or `changes`: graphs that do that with no approval step are refused |

## Triggers

A saved workflow can start from your code, from a cron **schedule** that always runs its latest saved version, or from an inbound **webhook** another system POSTs to. The webhook is off until you turn it on.

### Models

Set any one key in `.env`, or use `./apex model`. Compiling uses a cheap model to pick relevant tools and a strong one to write the graph, and each defaults to the provider you connected.

| Provider | Setting |
|---|---|
| Google Gemini | `GEMINI_API_KEY` |
| Anthropic Claude | `ANTHROPIC_API_KEY` |
| OpenAI | `OPENAI_API_KEY` |
| Any OpenAI-compatible server (OpenRouter, Groq, LM Studio) | `OPENAI_BASE_URL` and `PLANNER_MODEL=openai/<model>` |
| Ollama on your computer, no key | `PLANNER_MODEL=ollama/<model>` |

Override either tier with `PLANNER_MODEL` (writes the graph), `PLANNER_MAP_MODEL` (picks tools) or `APEX_AGENT_MODEL` (runs AI steps). The provider follows the model name.

### Managing the local stack

| Command | What it does |
|---|---|
| `./apex up` | Build and start the engine and what it needs |
| `./apex down` | Stop everything, keeping your data |
| `./apex env` | Print the `APEX_URL` and `APEX_API_KEY` exports |
| `./apex model` | Connect or change the model |
| `./apex status` | Show what is running and which model is connected |
| `./apex logs` | Follow the engine's logs |
| `./apex reset` | Delete all data |
| `./apex console` | Turn on the optional web inspector and open it |
| `./apex pieces` | Add the Activepieces runner, so pieces can be tools |

`apexctl demo` installs a sample Orders API and a ready-made workflow on a local stack, and runs without a model. To reach a service on your own computer from the Docker stack, use `host.docker.internal` rather than `localhost`.

## What has been measured

These came from this repository, on a laptop, against real Postgres and a real Temporal server.

- **Compile quality (2026-11-11):** [`evals/`](evals/README.md) holds 22 requests over a shop, a help desk and a CRM. The harness compiles each one, runs the graph against a mock backend that pages two records at a time, and passes the case only if the writes that happened are exactly the writes that were asked for. On a local 8B model (`ollama/qwen3:8b`) the first run passed **6 of 22**. Almost every failure was a mistake the compiler could have caught, so each became a compile-time check that tells the model what to write instead. The final run passed **22 of 22**, with 16 graphs needing at least one repair round and a median compile of about two minutes on that hardware.
- **What that number does not say:** the checks were written by reading these same 22 cases, so it is a development-set score, from one run, on one small model, against clean mock data. No hosted model has been run through the eval yet. Run it on yours, with requests of your own, before trusting it: `go run ./cmd/apex-eval --execute`.
- **Live compile on a hosted model (2026-11-08):** "get late orders, check the policy for each, approve, refund, summarize" over one OpenAPI catalog and one MCP server compiled in **18 s** into a 7-node graph with no repair rounds (`gemini-3.1-flash-lite` for selection, `gemini-3.5-flash` for the graph). One graph, so an example and not a benchmark.
- **Crash recovery (re-checked 2026-11-11):** `scripts/crash-demo.sh` sends `SIGKILL` to the worker during the second of three nodes. A new worker finishes the run. The first node's upstream call was made once, and only the in-flight node was retried.
- **A product's path, end to end (2026-11-12):** an engine started with only a platform key, driven as one customer's users through the CLI on the same local model: compile, review, preview (the sample API received one read and no write), change the plan with a second sentence (one step changed, four kept), run, an editor refused at the approval and an approver's name recorded on it. The same path is a test over the public packages: `engine/adoption_integration_test.go`.
- **Resume:** a run that failed at the second of three refunds was continued with `retry`. The first order was refunded once in total, and the retried call carried the same `Idempotency-Key`.

Not yet exercised against the real thing: OAuth with a real provider (the tests use a fake one), more than one Activepieces piece end to end, and paging against real APIs.

## Guarantees and limits

- **Isolation:** every catalog, run, schedule, key and audit entry belongs to the tenant of the API key. Another tenant gets a 404.
- **SSRF:** user-supplied URLs, including OAuth token endpoints, are checked after DNS resolution, at connect time. Private, loopback, link-local and metadata addresses are refused unless `APEX_ALLOW_PRIVATE_NETWORK=1`, which is for local development only. Calls made inside an Activepieces piece are not covered; see [pieces](pieces/README.md#security).
- **Credentials** are attached only to the base URL of the catalog that owns them. Credentialed requests do not follow redirects, and a next-page link on another host is refused.
- **At-least-once execution.** If a worker dies during a call, that call is retried. Non-GET API calls carry a stable `Idempotency-Key`, which also survives `retry`, but the upstream has to honor it. Piece actions and MCP tools carry no such key.
- **Model output can flow into later steps.** Text from an `agent.llm` node or a tool can be interpolated downstream, so put `logic.approval` before anything irreversible.
- **Webhooks:** the URL is the credential, so treat it like a password. The JSON body becomes the run input and can reach any step that uses `{{input.x}}`, so put `logic.approval` in front of anything irreversible. Each webhook is limited to 120 runs a minute. A repeated `Idempotency-Key` header does not start a second run.
- **Sizes:** response bodies up to 512 KB, collected pages up to 1 MB, data passed to a node up to 1.5 MB, loops up to 500 items, graphs up to 500 nodes. One run may move 30 MB through its steps or run 10,000 of them; past that it fails with that reason.
- **Previews** show writes that were held back, not what the upstream would have answered. A value that comes from a write's response is empty in a preview, and a step that needs one is reported as `unknown`. An `agent.llm` step does run in a preview, so it costs a model call.
- **Changing a plan by prompt** rewrites the graph in one model call, so it works on graphs the model can write in one call (a few dozen steps), and a weak model may touch more than it was asked to. The `diff` shows what it did.
- **Loops** run one item at a time and cannot contain another loop.
- **OpenAPI 3.x only**, and MCP over Streamable HTTP only. Paging is recognised from parameter names, which is a heuristic.
- **Upgrading:** workers replay open runs against their recorded Temporal history. The DAG runner records its version and retains the original behavior for histories created before versioning. For a rolling upgrade that starts emitting new commands, deploy compatible worker code everywhere before enabling that behavior.

### Workflow upgrade rule for contributors

Keep Temporal workflow and activity names stable and keep decoding older payloads. Guard any change to workflow commands, their order, or the decisions that produce them with `workflow.GetVersion`. Preserve the branch for older histories, including `workflow.DefaultVersion`, for as long as those histories need to replay. The `apex-run` marker covers the DAG runner. Changes to schedule setup need their own marker before the changed command. Run `go test ./internal/runtime -run TestWorkflowReplay` and record a new history when adding a command path. [Temporal replay testing](https://docs.temporal.io/develop/go/best-practices/testing-suite)

## Layout

| Path | What it does |
|---|---|
| `internal/compiler` | route → map → reduce, validation, repair, sectioned authoring for large graphs |
| `internal/catalog` | OpenAPI, MCP and piece import, MCP client |
| `internal/oauth` | OAuth 2.0 tokens: fetch, refresh, consent with PKCE |
| `internal/runtime` | deterministic Temporal workflow and node activities |
| `internal/httpapi` | the public API, saved workflows, webhooks, keys, audit log, metrics |
| `engine` | public: embed the engine in your program; `Engine.Client(tenant)` for a product's own customers |
| `client` | public: typed Go client, standard library only |
| `cmd/apexctl` | the CLI |
| `internal/web` | the optional inspector, off unless `APEX_CONSOLE=1` |
| `internal/netguard` | SSRF-safe HTTP clients |
| `cmd/demo-api` | a sample Orders API and MCP server for trying it out |
| `pieces` | the Activepieces runner, a small Node service |
| `evals`, `cmd/apex-eval` | the compile eval set and its harness |
| `examples` | `client` (a remote engine), `embed` (in your server), `saas` (a product with its own customers) |

## Tests

```bash
go test ./...
# real Postgres and Temporal; only model calls are stubbed
APEX_TEST_DATABASE_URL=postgres://... TEMPORAL_TEST_HOSTPORT=localhost:7233 \
  go test -p 1 -tags integration ./internal/httpapi ./engine
# add these to also run a real Activepieces piece end to end
#   APEX_TEST_PIECES_URL=http://127.0.0.1:7391 APEX_TEST_PIECES_TOKEN=...
```

## Deploy

On a host with Docker and a DNS record pointing at it, create `.env` from `.env.example`, set `DOMAIN`, and run:

```bash
docker compose -f docker-compose.yml -f docker-compose.prod.yml up -d --build
```

Only Caddy is published, with automatic HTTPS, and the sample tools and private-network access stay off whatever `.env` says. The keys in `APEX_API_KEYS` are each tenant's administrator; hand out narrower ones with `apexctl keys add`. A key is also how you sign in to the console. Set `APEX_ROLE=api` or `worker` to scale the two halves separately. Back up `APEX_SECRET_KEY`: without it, stored credentials cannot be read.

## License

MIT
