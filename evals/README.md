# Compile evals

How often does a sentence become a graph that does the right thing?

`cases.json` holds 22 requests over three small systems: a shop, a help desk
and a CRM (`specs/`, with data in `fixtures.json`). For each case `apex-eval`:

1. compiles the prompt through the engine's API,
2. checks the graph's shape (the operations it should and should not use, an
   approval before a destructive step, no needless loop),
3. with `--execute`, runs the graph against a mock backend and compares the
   writes that happened with the writes the request asked for.

A case passes only if all three hold and the run succeeds. The third check is
the one that matters: a graph can compile, look right, and refund the wrong
orders.

The mock returns two records a page, so a graph that forgets to ask for every
page misses records and fails. Approvals are answered automatically, yes or no
as the case says.

## Run it

Against an engine that can reach your machine (`APEX_ALLOW_PRIVATE_NETWORK=1`):

```bash
export APEX_URL=http://localhost:8700 APEX_API_KEY=...
go run ./cmd/apex-eval --execute --report evals/results/$(date +%F)-<model>.json
go run ./cmd/apex-eval --execute --only 'shop-*'      # a subset
go run ./cmd/apex-eval --execute --min-pass 0.8       # exit 1 below 80%, for CI
```

If the engine runs in Docker, add `--mock-listen 0.0.0.0:8799 --mock-url http://host.docker.internal:8799`.
The key needs the admin role, because the harness imports the three specs as
tools named "Eval shop", "Eval desk" and "Eval crm", replacing any from before.

## Reading a failure

The report keeps the plan of every failed case. The common causes so far:

- the graph reads the wrong field, or the first page when it asked for all,
- it narrows a list with a gate instead of `data.transform`, or reaches into
  a list with `[ ]` inside a condition,
- it uses `{{item}}` outside a loop.

Each of those is now rejected when the graph is written, and the reason goes
back to the model. When a new cause shows up, the fix is usually a check in
`internal/compiler` so it cannot reach a run again, then a line here.

## Adding a case

Add an object to `cases.json`. `expect.calls` is the exact list of writes, as
`METHOD /<system>/<path>`, repeated when a call should happen more than once.
Work it out from `fixtures.json` by hand. Keep the prompt unambiguous: if two
readings of the sentence give different writes, the case measures luck.

## Results

See `results/`. Each file records the models, the date and every case.

| File | Model | Passed |
|---|---|---|
| `2026-11-11-qwen3-8b-baseline.json` | `ollama/qwen3:8b` | 6/22 |
| `2026-11-11-qwen3-8b-after-checks.json` | same, after nine compile checks | 21/22 |
| `2026-11-11-qwen3-8b-final.json` | same, after three more | 22/22 |

The compile checks were written from these cases' failures, so the later
numbers are development-set scores. Add cases the compiler has never been
tuned against before quoting a pass rate.
