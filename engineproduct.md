# engineproduct.md

How apex-engine gets used by an organisation, what stood in the way of that, and
what was done about it. Part 1 is the product view. Part 2 is the engineering
log of the earlier work.

Parts 1 and 2 were saved on `safe/product-layer-2026-11-18`. Part 3 records work on `feat/close-eight-gaps`.

# Part 1: the product view (2026-11-12)

## What it is and who picks it up

apex-engine turns a sentence into a checked graph of calls to an organisation's
own systems, then runs that graph so it survives crashes and stops for a person
where it should. It is an engine: nobody uses it directly. A developer puts it
behind something of their own. So "usable" means usable by that developer, on
behalf of people who will never know the engine exists.

Three kinds of team pick it up.

**A. A product team adding automation for its customers.** A help desk, a shop
back office, a billing tool. Their customer types "close every ticket that has
had no reply in 14 days, but ask me first" into the product. Every customer is
a tenant; the product has thousands and gains some every day. The end user is
not technical and never sees a graph.

**B. A platform or operations team automating its own company.** A handful of
internal APIs, plus Slack, Jira and a CRM. Support, finance and ops staff ask
for things; the platform team reviews and owns what runs. A few dozen people,
one tenant or one per department, real money and real customer records.

**C. A script, CI job or AI agent handing off a long job.** Something that can
call an API wants a multi-step job done durably, with a human gate, without
holding a connection open for an hour.

A is the hardest and the one the engine was least ready for, so most of this
part is about A. What A needs, B and C get too.

## The path a product team walks

This is the order things happen in when team A integrates the engine. For each
step: what they need, what was in the way on 2026-11-11, and what is there now.

### 1. Put it in the product

They need the engine inside or beside their backend, speaking to their code.

- *Was there:* `engine.New` runs it inside a Go process; a Docker image runs it
  beside anything else; a typed Go client; an OpenAPI contract.
- *In the way:* embedding gave them an HTTP handler and nothing else. To call
  the engine from their own Go code they had to invent an API key, put it in
  config, and make HTTP requests to themselves.
- *Now:* `e.Client(customerID)` returns the same typed client working inside
  the process. No key, no port, no network. Streaming compiles work through it.

### 2. A customer signs up

They need a new customer to be able to use automation the moment they exist.

- *In the way, and it was a wall:* a tenant existed only as an entry in
  `APEX_API_KEYS`, read at start-up. Adding a customer meant editing an
  environment variable and restarting the engine. A product with its own
  customers could not use the engine at all.
- *Now:* a tenant needs no setup. In Go, `e.Client("cust_42")` is enough. Over
  HTTP, one platform key (`APEX_PLATFORM_KEY`) acts for any tenant and each
  request names it in `X-Apex-Tenant`. The first request that names a tenant
  creates it. Isolation is unchanged: another tenant gets a 404.

### 3. The customer's own users act, with their own names

The product has its own login. It needs the engine to know *which person* did
something, and to enforce what that person may do.

- *In the way:* identity was the name on an API key. A product proxying for
  its users would show every run as started, and every refund as approved, by
  one key. The audit log would be useless exactly where it matters.
- *Now:* the product names the end user and their role on each call:
  `e.Client(customer).As("dana@acme.com", "editor")`, or the `X-Apex-Actor` and
  `X-Apex-Role` headers. That name is what lands in `startedBy`, in an
  approval's `decidedBy`, and in the audit log. The role is enforced: an editor
  can build and run but cannot approve.

### 4. The user asks for something and has to agree to what comes back

This is the centre of the product. The user typed a sentence. The engine wrote
a graph. Someone now has to say yes to it, and that someone does not read
graphs.

- *In the way:* the only thing to review was the graph JSON. That works for
  the developer in scenario B. It does not work for a support manager.
- *Now, three things:*

  **What it does, in words.** Every compile returns a `review`:

  ```
   1. Read from Orders: List all orders
   2. Work out: Filter and prepare orders for refund
        [.outputs.list_orders.body[] | select(.status == "late" and .total <= 300)]
   3. Repeat the steps after this for each item of outputs.transform_orders.result, carrying on if one fails
   4. Ask a person: "Are you sure you want to refund order {{item.id}}? This action cannot be undone."
   5. Change Orders: Refund the order, once for each item, after a person approves
  1 read(s), 1 change(s), 1 approval(s)
  ```

  No model writes this. It is derived from the graph and the tool definitions,
  so it cannot say something different from what will run. A step that changes
  something with nobody asked first is listed as a warning.

  **What it would do to their real data, without doing it.** A run started
  with `preview: true` makes every read for real and sends no write. The
  product then shows the user the exact effects:

  ```
  would ask   Ask for approval before refund [item 0]: Are you sure you want to refund order ord_1? ...
  would call  Refund the order [item 0]: POST http://127.0.0.1:8722/orders/ord_1/refund
  ```

  This is the difference between agreeing to a description and agreeing to an
  outcome. "It will refund late orders" is a description. "It will refund
  ord_1 and ord_3" is something a person can check.

  **A way to fix it by saying so.** Nobody gets the sentence right first time.
  Before, a correction meant a new compile from scratch, which could come back
  as a different graph. Now the product sends the plan back with the
  correction ("do not refund orders over 300"), the model is told to keep
  everything the correction does not concern, and the answer says which steps
  were added, removed and changed.

### 5. It runs, and a person is asked at the right moment

- *Was there:* durable runs, approvals that wait up to seven days, signed
  events to a callback URL when a run needs a person or finishes, resume after
  failure without repeating what succeeded.
- *In the way:* nothing new at this step once step 3 was fixed. The approval
  now carries the end user's name.

### 6. The organisation that owns the product sets rules

The product team's security and legal people ask the same four questions.

| They ask | Was in the way | Now |
|---|---|---|
| Can an AI-written workflow delete things with nobody asked? | Yes. Nothing stopped it | `APEX_REQUIRE_APPROVAL=deletes` (or `changes`) refuses any such graph. A compile is told the rule and puts the approval in itself |
| Can one customer starve the others? | Yes. Only compiles were rate-limited | `APEX_MAX_OPEN_RUNS` caps one tenant's unfinished runs. The next start gets a 429 |
| How long do you keep our customers' data? | Forever. Every step's output stayed in Postgres | `APEX_RETENTION_DAYS` deletes finished runs with all their stored outputs. `APEX_AUDIT_RETENTION_DAYS` for the audit log |
| What happens when a customer leaves? | Nothing could remove a tenant | `DELETE /v1/tenant` stops their running work, removes their schedules, and deletes every tool, credential, workflow, run and key |

### 7. Day 30 and after

- *Was there:* metrics, an audit log, crash recovery, retry.
- *Now:* open runs can survive a worker upgrade that follows the [workflow versioning rule](#2-safe-upgrades-with-runs-in-flight).

## What is still in the way

These gaps were found on the same walk. Part 3 records what shipped and how it was verified.

1. **Nobody has measured compile quality on a model an organisation would
   use.** Everything here ran on a local 8B model, on cases the checks were
   written from. Step 4 is the product, and its hit rate on a hosted model is
   unknown. It needs an API key this machine does not have.
2. **Fixed. Safe upgrades with runs in flight.** A pending approval finished across a binary swap, and eight Temporal histories replay in CI. [Verification](#2-safe-upgrades-with-runs-in-flight)
3. **Only Go has a client.** Most product backends that would embed this are
   TypeScript or Python. The OpenAPI contract is complete and tested against
   the routes, but I did not run a generator over it, so "generate a client"
   is a claim, not a result.
4. **Scenario C has no front door for agents.** An agent talks MCP. The engine
   consumes MCP servers but is not one. A small MCP server exposing compile,
   preview, run and approve would let an agent hand off a job.
5. **A preview cannot see past a write.** If step 5 needs what step 4's POST
   returned, the preview says `unknown` for step 5. That is honest, but a
   product would rather show something. Tools would have to declare example
   responses.
6. **The console does not show review or preview.** They are in the API, the
   Go client and the CLI. The console is the optional inspector, so this is
   the least of it.
7. **One model for everyone.** A product may want each customer on their own
   model key, and wants to know what each customer's compiles cost. Neither
   exists: there is one global model and no token accounting.
8. **Approvals reach people only through the product.** The engine sends a
   signed event, and optionally one Slack webhook for the whole engine. Routing
   "ask Priya, then her manager after a day" is left to the integrator.

## How this was checked

**As a product team would use it, through the public packages only.**
`engine/adoption_integration_test.go` is the walk above as a test, on real
Postgres and a real Temporal server, with no API key anywhere: two customers
appear by being named; one connects a help-desk API; a plan is reviewed in
words; previewed (the customer's API received its read and zero writes); saved
and run; an editor is refused when approving and an approver's name lands on
the decision; the other customer sees none of it; the third unfinished run is
refused with 429 while the other customer is unaffected; the first customer is
offboarded and nothing of theirs is left.

**With the real binary, the CLI and a real model.** The engine was started with
only a platform key. As tenant `cust_42`, acting as `dana@cust42.test`:

| Step | Result |
|---|---|
| A platform key with no tenant | Refused, with the reason |
| Add the sample Orders API | Added |
| Compile "Refund every late order, but ask me before each refund" on `qwen3:8b` | 5 steps, valid first time, review printed |
| Preview, before the API credential was set | Stopped at the read with the API's 401, and said a real run would stop there too |
| Preview, with the credential | 2 questions and 2 refunds listed. The API recorded one list call and no refund |
| Compile "Do not refund orders whose total is over 300" with the plan as base | 1 step changed, 4 kept. The filter gained `and .total <= 300` |
| Preview the changed plan | 1 question and 1 refund: the order over 300 was gone |
| Another tenant lists tools | Empty |
| Run for real as an editor | Stopped at the approval |
| The editor approves | Refused: "your role is editor" |
| `priya@cust42.test`, an approver, approves | Run succeeded. `decidedBy` is her. One refund reached the API, carrying an idempotency key |
| Audit log | Each entry carries the end user's name |

**Tests.** `go vet` is clean. Unit tests pass in 9 packages. 32 tests pass in
the integration packages against real Postgres and Temporal, including the
earlier suite, so nothing from Part 2 regressed. New tests cover the platform
key, preview, retention, the review text, the approval rule and the diff.

**What these checks do not show.**

- The refinement was tried once, on one small change, on a small graph. The
  rule "keep what the change does not concern" is a prompt instruction, and a
  model can ignore it. The diff is there so a person sees it when one does.
- The eval from Part 2 was not run again after these changes. They add one
  check to validation that does nothing unless the approval rule is set, so
  the numbers should hold, but that is reasoning and not a measurement.
- Preview was tested against OpenAPI tools and plain HTTP steps. The MCP and
  Activepieces paths were written and are covered only by reading: no test
  runs a preview through an MCP server that marks a tool read-only.
- The retention sweep's delete was tested. The hourly timer around it was not.
- Offboarding was tested with waiting runs. It was not tested with a schedule
  attached or with hundreds of runs.
- No load was put on any of it.

## What changed in the code for Part 1

| Where | What |
|---|---|
| `engine` | `Config.PlatformKey`, `MaxOpenRuns`, `RetentionDays`, `AuditRetentionDays`, `RequireApproval`; `Engine.Client(tenant)`; the retention sweep; `ConfigFromEnvWithoutKeys`. API keys are no longer required |
| `client` | `As`, `ForTenant`, `Review`, `Preview`, `DeleteTenant`; `CompileRequest.Base`; `CompileResult.Review` and `.Diff`; `RunRequest.Preview` |
| `internal/httpapi` | Platform keys and the three headers; `GET /v1/runs/{id}/preview`; `DELETE /v1/tenant`; `preview` on run starts; `base`, `review` and `diff` on compile; `review` on validate; the open-run cap |
| `internal/runtime` | Preview mode: writes, approvals and waits are recorded instead of done; a step that needs a held-back write reports `unknown` |
| `internal/compiler` | `review.go` (the plain-language review), `refine.go` (change a graph, diff two graphs), the approval rule as a validation problem |
| `internal/catalog` | MCP tools keep their `readOnlyHint`, so a preview can run the ones that only read |
| `internal/store` | Migration `005_adoption.sql`; planned actions; purge; tenant delete |
| `cmd/apexctl` | `review`, `preview`, `compile --base`; `APEX_TENANT`, `APEX_ACTOR`, `APEX_ROLE` |
| `examples/saas` | A product with its own customers, in about 120 lines |
| Docs | README: "Inside a product that has its own customers", "Review before anything changes", guard rails. `docs/API.md` and `openapi.yaml` cover every new route and field |

Decisions worth knowing about:

- **A tenant is just a name.** There is no tenants table and no create call.
  That is what makes onboarding free. The cost is that a typo in a customer id
  makes a new, empty tenant instead of an error; the product owns its ids.
- **The platform key can do anything to anyone.** That is its job. It must
  stay on the integrator's server. The in-process client uses a random key
  made at start-up that never leaves the process.
- **A preview calls anything it cannot prove is a read a write.** MCP tools
  run only when their server marks them read-only; piece actions never run.
  A preview that shows too little is safe. One that sends a write is not.
- **A preview answers yes to every approval.** Otherwise it would stop at the
  first question and show nothing behind it.
- **The review is not written by a model.** A model's summary of a graph can
  be wrong about the graph. For the same reason a transform's expression is
  shown as it is, under the words.
- **Offboarding terminates runs instead of cancelling them**, so a stopping
  run does not write to rows that are being deleted.

# Part 2: the engineering log

This part is the log of the first pass, 2026-11-10 and 2026-11-11: closing the
six gaps a code review found. It is about the engine's insides. Part 1 above is
about the people who integrate it.

## Where it started

A review on 2026-11-10 found six things between the engine and real use:

1. Compile quality was unmeasured: one example graph.
2. Thin vocabulary: 8 built-in steps, no way to reshape data except a model
   call, one comparison per condition, no paging.
3. All-or-nothing failure: one failed loop item failed the run, there was no
   resume, and a re-run repeated side effects.
4. Auth was static headers only. No OAuth.
5. No organisation controls: static keys, no roles, no audit log, no metrics.
6. Three runtime bugs, found by reading: a double approval inside a loop, loop
   history growth, nested loops running silently wrong.

## Where it is now

| # | Gap | State | How it was checked |
|---|---|---|---|
| 1 | Compile quality unmeasured | Measured, on a small local model only: 6/22 before, 22/22 after | 22-case eval that executes each graph. The cases were also used to write the checks. See "The eval" |
| 2 | Thin vocabulary | `data.transform` (jq), `&&`/`\|\|`/`!` conditions, `logic.wait`, paging | Unit tests, and end to end on Postgres and Temporal |
| 3 | All-or-nothing failure | `retry` continues a failed run; `onError: continue`; a record per loop item | End to end: order a was refunded once across a fail and a resume |
| 4 | Static auth only | OAuth client credentials, refresh token, consent with PKCE; query-parameter keys | End to end against a fake provider, including a revoked token |
| 5 | No org controls | Roles on every route, API keys, audit log, `/metrics` | End to end |
| 6 | Three runtime bugs | Fixed | The double approval was reproduced on real Temporal first, then fixed |
| + | Activepieces | Any piece can be a tool, through a runner | End to end with a real piece; a real Slack call reached Slack |

Tests at the end: `go vet` clean; unit tests pass in 9 packages; 28 integration
tests pass against real Postgres 14 and a real Temporal dev server.

## What is not done, and what was not checked

Read this before relying on any of it.

- **No hosted model was used.** There is no Gemini, Anthropic or OpenAI key on
  this machine. Every compile here ran on a local 8B model (`qwen3:8b`). The
  eval number below is for that model and says little about a strong one.
- **The eval has no held-out cases.** The same 22 requests were used to find
  the compiler's gaps and to measure the result.
- **The Docker stack was not run as a whole.** `./apex up`, `./apex pieces`
  and the production overlay were not started. What was checked: both images
  build; the engine image's binary starts; the pieces image, run with the same
  lockdown Compose gives it (read-only filesystem, no capabilities), listed its
  36 pieces, loaded Slack, GitHub, Stripe, Google Sheets and Postgres, and ran
  an action; the Compose files parse.
- **OAuth was tested against a fake provider**, not Google, Microsoft, Slack or
  Salesforce. The flow follows the standard, and providers differ in small ways.
- **Only one piece was run end to end** (`json`, which needs no account). Slack
  was called with a fake token and answered `invalid_auth`, which shows the
  call path works and nothing about the other 700.
- **Paging is a heuristic** over parameter names. It was tested against six
  conventions in unit tests and one end to end, not against real APIs.
- **A transform can use a lot of memory for up to three seconds.** jq has no
  memory limit. `[range(1e12)]` reached about 2 GB in ten seconds in a test, so
  the time limit was cut to three. Anyone with the editor role can write one.
- **No workflow versioning.** The workflow code changed, so a run in flight
  across this upgrade may not resume. Nothing is deployed, so nothing was done
  about it; the README says so.
- **No user accounts or single sign-on.** Identity is the name on an API key.
- **Loops still run one item at a time**, and nested loops are rejected, not
  supported.
- **Piece triggers are not supported**, only actions.
- **The console got a small update** (resume, failed items, pieces). Keys,
  the audit log and OAuth setup are CLI and API only.
- **TypeScript and Python clients** still do not exist; generate them from
  `/openapi.yaml`.

## The three bugs

### A repeated decision approved the next loop item

Each approval node had one signal name. Inside a loop, a second copy of a
decision (a double click, a retried request) stayed queued and answered the
next item's wait, so an order could be refunded with nobody having looked at it.

Reproduced before fixing: with the fix removed, the integration test fails
with "order b was refunded 1 time(s) without anyone approving it".

Fix: each wait has a token, `<node>` or `<node>#<item>`. The waiting step's
output carries it, the approve route sends it back, and the run drops any
signal whose token is not the current one. The decision records `decidedBy`.

### Loop history growth

Every step of every loop item was handed the whole item list, so a few hundred
moderate items could pass Temporal's 50 MB history limit, which ends the
workflow in a way that looks like a crash.

Fix: body steps get the list only if they reference `outputs.<loop>.items`.
A run also counts what it moves, and fails with a plain message at 30 MB or
10,000 steps, before Temporal's limit.

### Nested loops

A loop inside a loop passed validation and ran the inner body once, with the
outer item. It is now rejected when the graph is written and again when it
runs, with a message that says to flatten the lists first.

## What was built

### Runtime (`internal/runtime`)

- `workflow.go`: rewritten around a `runner`. Approval tokens, the history
  budget, `onError: continue`, resume (`ResumeOf`, `IdemRoot`), `logic.wait`.
- `graph.go`: nested-loop rejection.
- `template.go`: a real condition parser: `&&`, `||`, `!`, `and`/`or`/`not`,
  parentheses, `contains`. It fails on anything it cannot read.
- `activities.go`: reuse of prior results, per-item rows, approval tokens.
- `api.go`: calls with headers, query keys and OAuth (one retry on 401 with a
  fresh token), idempotency keys that survive a resume, and paging by page,
  offset, cursor, `starting_after` and `Link` header.
- `nodes.go`: `data.transform` on gojq, `logic.wait`, `piece.*`.

### Compiler (`internal/compiler`)

- New built-ins and their rules in the authoring prompt, plus a short "how to
  build the common shapes" section.
- Compile-time checks. The first group came with the features; the second
  group came from reading what the eval's failures had in common:

  | Rejected at compile time | Why |
  |---|---|
  | A loop inside a loop | It ran silently wrong |
  | A condition that does not parse | It used to fail at run time |
  | A jq expression that does not parse, uses `{{ }}`, or reads a step it does not depend on | Same |
  | `.body` at the top of a jq expression | Only `.input`, `.outputs`, `.item`, `.loop` exist there. 5 of 22 baseline cases |
  | A field of a list (`outputs.x.body.total` where the spec says `body` is a list) | 3 of 22 |
  | A field the spec says the response does not have | The promise in the README, now kept for response fields too |
  | A loop over a gate's `passed`, or over an object | 3 of 22 |
  | `{{item}}` in a step that is not inside a loop | 1 of 22 |
  | A reference written without `{{ }}` in a tool's settings | It was sent as literal text. 1 of 22 |
  | A gate's yes/no used as a value (`"id": "{{outputs.ok.approved}}"`) | It produced `DELETE /contacts/true`. 1 of 22 |
  | `outputs.x.body` when `x` has `allPages` on | `body` is page one; the graph silently did less. 1 of 22 |
  | jq `input`, `inputs`, `env`, `$ENV` | Not available; failed at run time. 1 of 22 |
  | `[ ]` in a gate's condition (`outputs.x.items[].assignee == null`) | A gate tests one value. The one failure of the second run |
  | jq iterating a single value (`.outputs.gate.passed[]`) | Same case |
  | `[ ]` in a `{{ }}` path | Paths use dots (`items.0.id`); a bracket never resolves |

- Repair rounds went from 2 to 3 (`PLANNER_REPAIR_ROUNDS`). A round costs a
  model call only when the graph before it failed validation.

### Tools (`internal/catalog`, `internal/oauth`)

- `openapi.go`: recognises paged list operations and records what each
  response looks like (list or object, and an object's fields), which is what
  the shape checks above read.
- `pieces.go`: turns an Activepieces piece's actions into tools.
- `internal/oauth`: fetch, refresh, refresh-token rotation, PKCE, and a
  fallback between sending the client secret in the body and as Basic auth.

### Storage (`internal/store`)

`migrations/004_product.sql` adds `node_item_runs`, `oauth_states`, `api_keys`,
`audit_log`, `counters`, and columns on `runs`, `node_runs` and `catalogs`.
Credentials are one sealed value (headers, query, OAuth, piece connection);
rows written before this still open. OAuth refresh holds the catalog's row, so
two workers refreshing at once take turns, which matters when the provider
rotates refresh tokens. Only the SHA-256 of an API key is stored.

### API (`internal/httpapi`)

New routes: `POST /v1/runs/{id}/retry`, `GET /v1/runs/{id}/nodes/{key}/items`,
`/v1/keys`, `/v1/audit`, `/v1/pieces`, `POST /v1/catalogs/{id}/oauth/authorize`,
`GET /oauth/callback`, `GET /metrics`. Every route has a required role. The
contract in `openapi.yaml` covers all of them, and the existing test that fails
when the contract and the served routes disagree passes.

### Pieces runner (`pieces/`)

A dependency-free Node service, ported from the worker in `workflow-apex`. It
lists installed pieces without loading them, loads one on demand, and runs one
action per call. 36 common pieces in `package.json`. No published port, a
shared token, a read-only filesystem and no capabilities in Compose.

Decision: a piece is added as a tool, like a spec, one at a time. The 700
pieces are not all offered to the compiler on every request.

### Client, CLI, console

- `client`: every new endpoint.
- `apexctl`: `retry`, `items`, `whoami`, `keys`, `audit`, `tools pieces`,
  `tools auth`, `tools oauth`, `tools add --piece`. A secret can be `@file` or
  `env:NAME`. Exit code 4 means the run finished but some loop items failed.
- Console: "Continue from here", failed-item list, reused marker, pieces in the
  add-tool form, auth status on each tool.

### Stack

`./apex pieces` adds the runner. `docker-compose.yml` gains the `pieces`
profile and passes the new settings. `.env.example` documents them. CI also
checks that the console and the runner parse.

## Also checked

- **Crash recovery** after the runner rewrite: `scripts/crash-demo.sh` kills the
  worker with SIGKILL during the second of three steps. A new worker finished
  the run; the first step's call was made once and only the step in flight was
  repeated.
- **The real binary and CLI together**: `apexctl` against a running
  `apex-engine` added a piece, ran a graph with a piece, a transform, an
  and/or condition and a loop set to continue, listed the failed item, made a
  key, was refused as an editor, and read the audit log and metrics.
- **The console in a browser**: sign in, a failed run, "Continue from here",
  the reused marker, the failed-item list, the Tools page with 707 pieces
  offered.

## Bugs found by checking, not by reading

- The Tools page was broken by this work: it set a read-only DOM property and
  showed "Something went wrong". Found by opening the page; fixed.
- The run page said "the loop was set to carry on" on a run whose loop had
  stopped. Fixed.
- The paging counter counted an empty last page. Found by its test; fixed.
- The first version of the transform test assumed jq errors on a missing
  field. It returns null, and `150 > null` is true. That is why "bind outside
  values first" is in the authoring prompt.

## The eval

`evals/` holds 22 requests over three systems (a shop, a help desk, a CRM).
`cmd/apex-eval` compiles each one, checks the graph's shape, runs it against a
mock backend, and compares the writes that happened with the writes that were
asked for. The mock pages two records at a time, so a graph that forgets
paging fails.

Three full runs, all on `ollama/qwen3:8b` for both model roles, all on
2026-11-11. The reports are in `evals/results/`.

| Run | Compiled | Did exactly what was asked | Needed a repair round | Median compile |
|---|---|---|---|---|
| Baseline (`…-baseline.json`), before the checks below | 17/22 | **6/22** | 7 | 57 s |
| After nine new compile checks (`…-after-checks.json`) | 22/22 | **21/22** | 15 | 124 s |
| Final code, three more checks (`…-final.json`) | 22/22 | **22/22** | 16 | 119 s |

What the 16 baseline failures were:

| Cause | Cases |
|---|---|
| The model gave up after three attempts, each time treating a gate as if it passed data through (`outputs.filter.passed.id`) or writing jq in a condition | 5 |
| jq read `.body` at the top level, which is null, and the run failed with "cannot iterate over: null" | 4 |
| A condition read a field of a list (`outputs.list.body.total`) | 3 |
| The graph ran and did the wrong thing: a call to `DELETE /contacts/true`, two writes skipped, one loop item failed | 3 |
| jq used `input`, which a transform does not have | 1 |

None of these was a hard reasoning mistake. Each was a mistake the compiler
could see before anything ran, so each became a check whose message tells the
model what to write instead. The single failure in the second run was a new
form of the first cause (`outputs.list.items[].assignee == null` in a gate,
then `.outputs.gate.passed[]` in jq). It got three checks of its own, and the
third run passed all 22.

How to read these numbers:

- **The eval set was also the development set.** The checks were written by
  reading these 22 cases' failures. 22/22 says the checks work on the mistakes
  seen so far. It does not say a new request will pass at that rate. A fair
  number needs cases written after the checks, by someone else.
- **One run each.** The model is not deterministic. The same code could score
  20 or 21 on another day.
- **It is a local 8B model.** A hosted model should make fewer first-draft
  mistakes, but that was not measured here: there is no key on this machine.
- **The price was speed.** More graphs now go back for repair (16 of 22
  against 7), and the median compile roughly doubled. On a laptop GPU that is
  two minutes; on a hosted model a repair round is a few seconds.
- **The mock is simple.** Three small systems, clean data, no rate limits, no
  errors from the upstream.

## How to run everything

```bash
go vet ./... && go test ./...

# integration, on this laptop (no Docker)
temporal server start-dev --port 7244 --headless &
PIECES_DIR=~/projects/workflow-apex/apps/pieces-worker PIECES_PORT=7392 PIECES_TOKEN=t node pieces/server.mjs &
APEX_TEST_DATABASE_URL="postgres://$USER@localhost:5432/apex_engine_test?host=/tmp" \
TEMPORAL_TEST_HOSTPORT=localhost:7244 \
APEX_TEST_PIECES_URL=http://127.0.0.1:7392 APEX_TEST_PIECES_TOKEN=t \
  go test -p 1 -count=1 -tags integration ./internal/httpapi ./engine

# crash recovery
DATABASE_URL=... TEMPORAL_HOSTPORT=localhost:7244 APEX_SECRET_KEY=$(openssl rand -base64 32) scripts/crash-demo.sh

# the eval, against a running engine
APEX_URL=... APEX_API_KEY=... go run ./cmd/apex-eval --execute
```

Two databases were created on the local Postgres for this work:
`apex_engine_test` and `apex_engine_eval`. Drop them when you no longer want them.

## What was left on this machine

Cleaned up after each session. On 2026-11-11: the throwaway Temporal dev
server, the pieces runner on 7392 and the engine instances were stopped; the
scratch binaries, `bin/`, the two `:check` Docker images and their build stage,
the `apex_engine_eval` database and the Go build cache were removed. On
2026-11-12: the same processes, the scratch binaries and the
`apex_engine_adopt` database used for the hands-on walk in Part 1.
Still there: the `apex_engine_test` database (10 MB) on local Postgres, which
the integration tests use.

## What to do next, in order

1. **Commit.** Everything in both parts is uncommitted on `main`.
2. **Run the eval on a hosted model, with new cases.** Set one key, add ten
   requests nobody has tuned against, and run
   `go run ./cmd/apex-eval --execute`. It is item 1 of "What is still in the
   way" in Part 1 and the number the product stands on.
3. **Workflow versioning**, so a host product can deploy with runs in flight.
4. **A TypeScript client**, generated from the contract and tested.
5. **Run the Docker stack once** (`./apex up`, `./apex pieces`), and connect
   one real OAuth provider and one real piece with an account.

# Part 3: closing the eight

## 2. Safe upgrades with runs in flight

- A product can replace the engine while its customers have runs waiting for a decision.
- Record a Temporal patch version before the first command of the shared DAG runner.
- Histories without a marker use Temporal's default version and keep the original commands.
- GetVersion works for embedded workers on the existing queue without deployment infrastructure.
- Keep an existing branch when changing command order, timers, signals or workflow inputs.
- Record histories from the saved binary and replay them in the ordinary Go test suite.
- Stop the saved binary with an approval outstanding and finish it through the replacement.

The runner now records `apex-run` version 1. Version 1 and pre-marker
histories emit the same business commands. The README explains how future
changes keep compatibility, including schedule setup before the DAG runner.

- Replayed eight recorded histories in ordinary Go tests. The pending
  approval has 29 events, the loop 65, the retried run 35, the preview 53,
  the schedule 42, the durable wait 40, the completed upgraded approval 59
  and the versioned approval 61. The fixtures use only local test data.
- Built `0d7620a` from the safety branch, then started an approval through
  `apexctl run --wait --stop-at-approval` (exit 3). Stopped that engine with
  `TimerStarted` in the history. Started the new binary on the same Postgres
  and Temporal, and approved through `apexctl approve`. The same run
  reached `succeeded`, with `decidedBy: root` and a true result from its final step.
- `gofmt`, both vet commands, unit tests in nine packages, and all 33 tests in
  the integration invocation passed, including the real Activepieces action.
  The first integration invocation intermittently reported
  `failed reaching server: context deadline exceeded` during setup with
  `localhost:7244`. The full rerun used `127.0.0.1:7244`, the same dev server.
- This checked a stop/start upgrade with a single worker. A mixed-version
  fleet and a Temporal Cloud deployment were outside the check.
