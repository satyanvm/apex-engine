# productside.md

The business side of apex-engine: who would pay for it, how it can make money,
how it becomes a GitHub project people know, who it is up against, and where it
is stronger and weaker than them. Written 2026-11-12.

At the end there is an appendix that compares it with Kubeflow Pipelines.

Market figures come from public reporting and third-party trackers, listed
under Sources. Many private-company numbers are estimates. Where sources
disagree, I give the range or say which one I used.

---

## 1. The short version

- **The market is real and well funded.** In 2026 Temporal raised at $12.55B,
  n8n at $5.2B, Gumloop raised $50M and Arcade $60M. Money is flowing to two
  things: workflow automation, and infrastructure that makes AI agents reliable.
  apex-engine sits where the two meet.
- **There is a known weakness in the market that apex-engine already
  addresses.** People who generate workflows with AI keep reporting the same
  problem: the result "looks right but fails". apex-engine refuses to return a
  graph that fails its checks. It also explains the graph in plain words, and
  shows what a run would do before anything changes.
- **The best way to make money is embedding.** A software company puts the
  engine inside its own product so its customers can automate things by typing
  a sentence. That buyer pays thousands of dollars a month today:
  - Cyclr publishes prices from $1,595 to $7,195 a month.
  - n8n's embed licence is reported at about $50,000 a year (not confirmed by n8n).
  - n8n's own licence restricts that use. apex-engine's MIT licence does not.
- **Make money in three layers, in this order:**
  1. paid help for the first few companies that embed it;
  2. a commercial licence for the features those companies need;
  3. a hosted cloud.
  Selling to end users who automate their own work is a poor fit, for reasons
  given in section 5.
- **What stands in the way is not code.** It is four things:
  1. no proof yet that compiles work well on a hosted model;
  2. one person behind it;
  3. a name that collides with others;
  4. a setup that needs Postgres and Temporal before anyone can try it.

---

## 2. What we are selling, in one sentence

> **Type what you want automated. Get a workflow that has been checked against
> your real APIs, explained in plain words, and tried on your real data without
> changing anything. Then run it so it cannot be lost, with a person approving
> every risky step.**

Every competitor offers one or two parts of that sentence:

| Part of the sentence | Who else has it |
|---|---|
| Type it, get a workflow | n8n AI Workflow Builder, Zapier Copilot, Gumloop, Relay.app, Make |
| Checked against your real APIs, no invented steps | Nobody publicly. Users of the tools above complain that it is missing |
| Explained in plain words, with no model writing the explanation | Nobody I found |
| Tried on real data without changing anything | Nobody I found as a built-in |
| Cannot be lost (survives crashes, waits days for a person) | Temporal, Inngest, Trigger.dev, Restate. All of them require you to write code |
| A person approves risky steps | Relay.app, HumanLayer (now part of Factory), n8n and Zapier in part |
| Inside your own product, for your customers | Paragon, Prismatic, Cyclr, Workato Embedded, Tray. None compile from a sentence |

Nobody I found does all of it. That is the position to claim.

---

## 3. Who would pay

### 3.1 A software company adding automation to its product (the main target)

- **Who:** a help desk, a shop back office, a CRM, an HR or finance tool, any
  business software with an API.
- **The problem:** their customers keep asking "can it do X automatically?".
  Building each request by hand does not scale. Embedding Zapier or n8n either
  costs a lot or is restricted by licence. Visual builders confuse their users.
- **Why us:**
  - The end user types one sentence.
  - The product shows them the plan in plain words and what it would change.
  - The user says yes, and it runs reliably.
  - The engine handles tenants, roles and approvals (all built in Part 1 of
    `engineproduct.md`).
- **What they pay today for something close:**
  - Cyclr: $1,595 to $7,195 a month.
  - Paragon and Prismatic: priced by quote, per connected customer or per
    installation.
  - n8n embed: about $50,000 a year (reported, not confirmed).
- **What they need from us before they can buy:**
  - proof that compiles work on a model they would use;
  - a TypeScript client;
  - safe upgrades while runs are in progress;
  - a company they can sign a contract with.

### 3.2 An operations or platform team inside a mid-size company

- **Who:** the team that owns internal tooling at a company with 200 to 5,000
  people.
- **The problem:** staff in finance, support and operations ask for
  automations. Those automations touch money and customer records, so the team
  cannot let people build them freely in Zapier.
- **Why us:**
  - It runs on their own servers.
  - It only uses tools the team has imported.
  - Approvals and an audit log are built in.
  - It works with a local model, so no data leaves the building.
- **What they pay today:** Workato and Tray at enterprise prices, or n8n
  Enterprise.
- **Fit:** good, but they buy slowly and ask for security reviews.

### 3.3 Developers building AI agents

- **Who:** teams whose agent needs to hand off a long job, such as "reconcile
  these 400 invoices and ask finance about the odd ones".
- **The problem:** agents lose their place when something crashes, and they
  should not perform dangerous actions without a check.
- **Why us:** the agent describes the job, apex-engine compiles and checks it,
  and the job runs durably with approvals.
- **What they use today:** Temporal, Inngest, Trigger.dev and Restate for
  durability; Arcade and Composio for tool access and permissions.
- **Fit:** this group spreads the word and brings GitHub stars, but they spend
  little. They need an MCP server so their agents can call the engine, which
  does not exist yet.

### 3.4 Individuals and small teams automating their own work

- **Who:** the Zapier, Make and n8n Cloud audience.
- **Fit:** poor. See section 5.

---

## 4. Ways to make money

Ranked by how soon they bring money in and how well they fit.

### 4.1 Paid design partnerships (first 3 to 6 months)

Find three to five software companies of type 3.1. Charge each a fixed fee to
get the engine into their product, with you doing most of the work. That
brings in money, produces real requests to test compiles against, and creates
case studies.

- **Price:** a fixed project fee, then a monthly support fee.
- **Why first:** it needs no cloud, billing system or sales team. It answers
  the most important open question: do real users' sentences become correct
  workflows?

### 4.2 Open core: a free engine plus a paid licence (from month 3)

The engine stays MIT. Features that only a business embedding the engine for
many customers needs go in a separate `ee/` directory under a commercial
licence. Activepieces does this with an MIT core and a commercial enterprise
directory. Windmill does it with an AGPL core and a paid enterprise edition.

| Free (MIT) | Paid licence |
|---|---|
| Compiler, checks, review, preview, durable runs, approvals, retry | Single sign-on and user provisioning for the engine's operators |
| Tenants, roles, platform key, audit log | Exporting the audit log to the customer's own systems; legal hold |
| OpenAPI, MCP and Activepieces tools | A separate model and API key per tenant, and token cost per tenant |
| Local models and bring-your-own key | Approval routing: who to ask, and who to escalate to after a delay |
| Command line, Go client, OpenAPI contract | White-label console that end users can see |
| | High-availability setup guide, support with a response-time promise, priority fixes |

- **Price (my proposal):**
  - $1,000 to $2,500 a month for a company embedding it, priced by the number
    of active tenants.
  - Enterprise pricing for companies above that size.
  - Cyclr's published prices start at $1,595 a month, and n8n's embed licence
    is reported at about $50,000 a year, so this undercuts both.
- **Licence choice:** keep the core MIT. A more restrictive licence (BSL, FSL,
  AGPL) protects against a cloud provider reselling the engine, but it also
  scares away exactly the companies that would embed it. n8n's licence is the
  reason its embed customers have to sign a separate agreement. Being freely
  embeddable is our selling point, so do not give it up.

### 4.3 A hosted cloud (from month 6 to 12)

The same engine, run by us. Customers bring their own model key, or pay us a
margin on model usage.

- **Price:** usage-based, the way Temporal charges, with a monthly minimum per
  plan.
  - Temporal Cloud charges $25 to $50 per million actions.
  - Its Essentials plan is the greater of $100 a month or 5% of usage.
  - Ours would charge per compile, which is the expensive part because it
    calls a model, and per thousand steps run.
- **Cost to watch:** we would run on Temporal Cloud or on our own Temporal
  cluster. Every step of a run produces several Temporal actions, so our price
  per step has to cover Temporal's price per action.
- **Why not first:** running a cloud means being on call, billing, security
  certification and support. That is a lot for one person, and it competes
  with the design partners for time.

### 4.4 Selling to a larger company

This is not a plan, but it is a realistic outcome. Recent deals show who buys
this kind of company:

- Workday bought Pipedream (3,000 connectors) in 2025.
- Workday also bought Flowise, an AI workflow builder.
- Factory bought HumanLayer (approvals for AI agents).
- SAP invested in n8n to build it into SAP's own AI product.

Buyers are paying for integration and agent infrastructure. A working engine
with a few paying embed customers and a known GitHub project is the kind of
asset they buy.

### 4.5 What not to do

- **A template marketplace.** Low value and lots of moderation. Activepieces'
  700 pieces already cover the long tail of integrations.
- **Charging per seat to end users.** The value is in what gets automated, not
  in how many people log in.
- **Going after Zapier's audience.** See below.

---

## 5. Why not compete with Zapier, Make or n8n for end users

- **They are large and established.**
  - Zapier: about $310M a year in revenue as of 2023–2024, by third-party
    estimate.
  - n8n: crossed €100M a year in 2026, according to its investor.
  - Both already offer "describe it, get a workflow".
- **That audience buys a visual editor and 7,000 ready-made integrations.**
  apex-engine has neither, and building them would take years.
- **Even a big company has walked away from a visual agent builder.** OpenAI
  launched a visual Agent Builder in October 2025 and is shutting it down on
  30 November 2026, pointing users to code. A visual editor alone is not a
  durable advantage.
- **Where we are different, and where it matters most, is safety:** checked
  graphs, preview, approvals, crash recovery. That matters most to a business
  putting automation in front of its own customers. Sell there.

---

## 6. Competitors, with what they earn and how

### 6.1 Automation platforms (the end-user market)

| Product | Size and money | How it earns | Licence | AI workflow building |
|---|---|---|---|---|
| **Zapier** | ~$310M a year (2023–24 estimate); ~$400M for 2025 (estimate). Not disclosed | Tasks per month, by plan | Closed | Copilot builds workflows from a sentence; AI steps are billed as tasks |
| **n8n** | Valued at $5.2B (May 2026, SAP). Over €100M a year (investor claim) | Cloud plans, Enterprise, embed licence (~$50k a year, reported) | Sustainable Use License: source available, embedding restricted | AI Workflow Builder; reviewers call it a fast draft that still needs fixing |
| **Make** | Owned by Celonis | Credits (moved from operations in Aug 2025) | Closed | AI agents in public beta from April 2026 |
| **Workato** | Last priced round $5.7B (2021); ARR grew 35% in fiscal 2026 | Enterprise contracts | Closed | Pushing hard on AI agents; bought XMAD.ai and DeepConverse |
| **Activepieces** | About $1.7M a year (Dec 2024 estimate); about $1.6M seed from Y Combinator | Cloud credits, Enterprise | MIT core plus commercial enterprise directory | Has AI features. We reuse its 700 MIT integrations |
| **Gumloop** | $50M Series B (Benchmark, March 2026), ~$70M raised | Subscription | Closed | Describe an agent in words, then a visual canvas |
| **Relay.app** | $8M to $30M+ raised (sources differ) | Subscription | Closed | Workflows in plain language, with human review on any AI step |
| **Pipedream** | Bought by Workday (announced Nov 2025) | Was usage-based | Closed | Now part of Workday's agent platform |

### 6.2 Embedding in other companies' products (our main target market)

| Product | Size and money | Price | Can it build from a sentence? |
|---|---|---|---|
| **Paragon** | Series B, about $34M raised | By quote, per connected customer | No. Integrations are written as code |
| **Prismatic** | Not found | By quote, per installation | No. Built by engineers in a low-code tool |
| **Cyclr** | Not found | $1,595 to $7,195 a month published; MCP product from $999 a month | No |
| **Workato Embedded, Tray Embedded** | Part of larger companies | Enterprise | Partly, through their own AI features |
| **n8n Embed** | Part of n8n | ~$50,000 a year (reported) | Yes, through n8n's AI builder, with n8n's reliability problems |

Most of this market sells **integrations a company's engineers build once and
customers switch on**. None of these let the customer **describe a new
automation**. That gap is apex-engine's opening.

### 6.3 Engines that make workflows survive crashes (what we build on, or compete with)

| Product | Size and money | Price | Licence | Relationship to us |
|---|---|---|---|---|
| **Temporal** | $12.55B valuation (Sept 2026); over $250M a year | Cloud: $25 to $50 per million actions, plus plan minimums | MIT | We build on it. Its customers include OpenAI Codex, Replit and Lovable |
| **Inngest** | Not found | Plans vary by source, from $25 to $499 a month | Source available (SSPL, per one source) | A competitor for developers. Code only |
| **Trigger.dev** | Series A | Plans from $10 to $50 a month | Apache 2.0 | A competitor for developers. Code only |
| **Restate** | Not found | Cloud from $75 a month | BSL | A competitor for developers. Code only |
| **Windmill** | Seed | Enterprise from about $120 a month plus per-user prices | AGPLv3 plus enterprise | Scripts and flows, partly code |

All of these need a programmer to write the workflow. apex-engine compiles it
from a sentence and adds checks, review and preview on top of Temporal. To
Temporal we are a layer above it, not a rival. That makes Temporal a possible
partner or buyer.

### 6.4 Tools and safety for AI agents (neighbours)

| Product | Money | What it does |
|---|---|---|
| **Arcade.dev** | $60M Series A (June 2026), $72M total | Login and permissions for the actions agents take |
| **Composio** | $29M or more | Ready-made tools for agents |
| **HumanLayer** | Bought by Factory | Human approval for AI agent actions |
| **Dify** | ~156,000 GitHub stars (Sept 2026) | Open-source platform for building AI apps and workflows |

They show that investors pay for **making agents safe to let loose on real
systems**. Our approvals, preview and per-tenant roles are the same idea
applied to workflows.

---

## 7. Our advantages, honestly

1. **It refuses broken graphs instead of returning them.**
   - Every graph passes type, reference, data-shape and cycle checks against
     the real API definitions. Failures go back to the model with the reason.
   - On the 22-request test set with a small local model, this took success
     from 6 to 22 of 22.
   - The same cases were used to write the checks, so that is not a fair
     benchmark (see `engineproduct.md`). But it is the problem the market
     complains about most.
2. **It explains a graph in plain words, and no model writes the
   explanation.** The explanation is generated from the graph itself, so it
   cannot disagree with what will run. I found no competitor that does this.
3. **It previews a run on real data and changes nothing.**
   - Reads happen; writes are listed, not sent.
   - The user agrees to "refund ord_1 and ord_3", not to "refund late orders".
   - I found no competitor that offers this built in.
4. **Runs cannot be lost.**
   - It is built on Temporal, the engine OpenAI, Replit and Lovable use for
     their agents.
   - A crash does not repeat finished steps.
   - A failed run can be continued without redoing what succeeded.
5. **Approvals are designed for business use.**
   - Each wait has its own token, so a repeated click cannot approve the next
     item.
   - The approver's name is recorded on the decision.
   - An approval rule can require a person before any delete.
6. **It is built to sit inside another product.**
   - Customers are tenants with no setup.
   - End users' names and roles flow through from the host product.
   - It cleans up when a customer leaves, limits each customer's load, and
     deletes old data on a schedule.
7. **Its licence allows embedding.** MIT, unlike n8n's licence. This is an
   advantage over the most popular open-source rival.
8. **It has 700 integrations without writing them,** through Activepieces'
   MIT-licensed pieces, and it imports any OpenAPI spec or MCP server.
9. **You can choose the model.** Gemini, Anthropic, OpenAI, any
   OpenAI-compatible endpoint, or a local Ollama model, so data can stay
   inside the company.

---

## 8. Our disadvantages, honestly

1. **Compile quality on a model a business would use has not been measured.**
   All tests ran on a local 8-billion-parameter model. This is the claim the
   whole product rests on.
2. **One person.** A buyer embedding infrastructure asks who fixes it at 3am.
   Design partners and a support contract are the answer at first.
3. **It is heavy to try.**
   - It needs Postgres and Temporal. n8n and Activepieces start with one
     Docker command.
   - `./apex up` exists, but the full Docker stack has not been tested end to
     end.
4. **No visual editor.** Plenty of buyers want to see and drag the graph. The
   console only shows it.
5. **Only a Go client.** Most software companies' backends are TypeScript or
   Python.
6. **Worker upgrades require replay compatibility.** A pending approval
   survived a binary replacement. Contributors patch command changes with
   Temporal version markers and replay recorded runs in CI.
7. **The name collides.**
   - "APEX engine" is already a game engine with modding tools on GitHub.
   - Salesforce's programming language is called Apex.
   - Searching for the project will find the wrong things. Rename before
     launch.
8. **Integration depth.** Activepieces pieces cover breadth, but only one ran
   end to end in testing. OAuth has only been tested against a fake provider.
9. **Bigger players can copy the safety features.** n8n, Zapier or Temporal
   could add checks and previews. What protects us is being first, being
   embeddable, and being trusted. It is not a secret technique.

---

## 9. Making it a well-known GitHub project

What has worked for others: the README acts as the landing page; a working demo
in under a minute; a Hacker News launch that leads to GitHub Trending; launching
again and again, not once. The Hacker News bump is large but brief. Skyvern went
from 6,000 to 9,000 stars in four days. What lasts comes from Trending and from
launching repeatedly.

### Before launch

1. **Rename** to something unique that you can get as a domain and on npm.
2. **Run in one command with no keys:**
   - `docker compose up`, then open a page;
   - the sample shop is already loaded and a local model is optional;
   - offer a hosted demo for people who will not install anything.
3. **A 30-second GIF at the top of the README.** Type a sentence, see the plan
   in words, see the preview list "would refund ord_1, ord_3", approve, done.
   That is the moment that impresses people.
4. **One line under the title** that names the problem people recognise:
   "AI-generated workflows that actually run: checked, previewed and approved
   before they touch anything."
5. **A comparison table in the README** against n8n AI Builder, Zapier
   Copilot and plain Temporal: checked graphs, plain-words review, preview,
   approvals, embeddable licence.
6. **Publish the benchmark:**
   - run the test set on Claude, GPT and Gemini;
   - publish it, with the cases written after the checks;
   - developers trust numbers, and nobody else publishes these.

### Launch, and launch again

- **Show HN:** the problem, the GIF, the benchmark, one command to start. Post
  on a weekday morning, US time.
- **Two weeks later:** a write-up of how the checks took a small model from
  6 to 22 of 22. Technical posts like this do well on HN and Reddit
  (r/LocalLLaMA, r/selfhosted, r/golang).
- **The MCP server is its own launch:** "let your agent hand off long jobs
  safely".
- **The TypeScript client is its own launch.**
- **Product Hunt** for the hosted demo. It brings fewer stars (one report says
  200 to 600 from a strong launch) but more of the people who buy.

### Keep it growing

- A small number of excellent examples: refund approvals in a shop, closing
  stale tickets in a help desk, invoice chasing. Each one runs in one command.
- "Good first issue" labels, a contributor guide, and quick replies to issues.
  That responsiveness is what a one-person project can offer.
- Make the Activepieces connection visible ("700 integrations"), and credit
  Activepieces.

---

## 10. Plan for the first 90 days

| Weeks | Product | Business |
|---|---|---|
| 1–2 | Commit the work. Rename. One-command start. GIF. Run the test set on hosted models with new cases | List 30 target software companies (help desk, e-commerce operations, vertical SaaS) |
| 3–4 | TypeScript client. Safe upgrades with runs in progress | Talk to 10 of them. Offer a paid pilot |
| 5–6 | Show HN launch. Publish the benchmark | Sign the first one or two design partners |
| 7–10 | MCP server. Approval routing. Separate model key per tenant | Build design partners' requests into the product. Write the commercial licence terms |
| 11–13 | Second launch (MCP). Start a waitlist for the hosted version | Turn design partners into licence customers. Decide whether to raise money or stay small |

What would prove it is working:

- two paying design partners;
- 1,000 GitHub stars from people outside your network;
- a published compile success rate above 85% on new cases with a hosted
  model.

If the success rate stays well below that, fix compile quality before selling
anything else.

---

## Appendix: apex-engine and Kubeflow Pipelines

### How similar are they?

**They have the same overall design but serve different purposes, and share no
code.** Both take a description of work, turn it into a checked graph of steps,
store it, and hand it to a separate engine that runs each step durably. They
differ in who writes the graph, what a step is, and what the work is for.

| | Kubeflow Pipelines | apex-engine |
|---|---|---|
| **For** | Machine learning engineers: data preparation, training, evaluation, deploying models | Businesses and the products they build: calling their own APIs, with approvals |
| **The graph is written in** | Python, using the KFP SDK | An English sentence, compiled by a language model |
| **Compiled to** | A platform-neutral YAML format (the "IR") | A JSON plan, checked against the real API definitions |
| **Checked by** | The SDK compiler: types and the shape of the graph | Types, references, data shapes, cycles, approval rules; failures go back to the model |
| **One step is** | A container, run as its own Kubernetes pod | A function call inside a worker process |
| **Runs on** | Argo Workflows on Kubernetes (Vertex AI Pipelines on Google Cloud) | Temporal: any machine with Postgres and a Temporal server |
| **Service handling requests** | Go API server (gRPC and REST) | Go API server (REST) |
| **Database** | MySQL by default, or PostgreSQL; plus an object store for files | Postgres |
| **Recurring runs** | A Kubernetes controller for scheduled workflows | Temporal schedules |
| **Not redoing work** | A cache that skips steps whose inputs have not changed | Reusing finished steps when a failed run is continued |
| **Tracking what a run produced** | ML Metadata (MLMD) records which data made which model | Each step's output and the audit log |
| **Human approval** | Not built in | Built in |
| **Tenants** | Kubernetes namespaces and profiles | Tenants with roles; platform key |
| **Size** | About 7.5 MB of Go, 7.8 MB of Python and 4.1 MB of TypeScript (GitHub's count; includes generated code). 4,264 stars, started 2018, Apache-2.0 | About 20,000 lines of Go, plus a small Node service and the console |
| **Latest** | 2.18.0, 28 October 2026 | Not released |

A rough measure: **most of the architecture concepts match, almost none of the
infrastructure does, and none of the code does.** If you can explain one, you
can sketch the other on a whiteboard. You could not run one by knowing the
other.

### Will understanding apex-engine help you understand Kubeflow?

**Yes, for the overall design, which is the hard part to grasp. No, for the
Kubernetes machinery, which is most of Kubeflow's code.**

Where your knowledge carries over:

| You understand in apex-engine | It corresponds to in Kubeflow |
|---|---|
| `internal/compiler`: request to checked plan | The KFP SDK compiler (Python, `sdk/`) that turns a pipeline into IR YAML, then `backend/src/v2/compiler/argocompiler` that turns IR into an Argo Workflow |
| `internal/httpapi` + `internal/store` | `backend/src/apiserver` (its `persistence`, `list`, `filter` and `auth` folders) |
| `internal/runtime`: one Temporal workflow driving the graph, one activity per step | `backend/src/v2/driver` (decides what a step gets: inputs, cache, the DAG around it) and the launcher (runs the step inside its container) |
| `ScheduledWorkflow` and schedules | `backend/src/crd/controller/scheduledworkflow` |
| Reusing finished steps on retry | `backend/src/v2/cacheutils` and the driver's cache check |
| `openapi.yaml` with the drift test | `backend/api` (protobuf definitions from which the Go and Python clients are generated) |
| Run and step status in Postgres | The persistence agent (`backend/src/agent`), which watches Argo and writes status back to the database |

What you would have to learn fresh:

- **Kubernetes controllers and custom resources.** Kubeflow is built from
  controllers that watch Kubernetes objects. apex-engine has none.
- **Argo Workflows.** Each step is a pod, data moves through files, and the
  whole run is a Kubernetes object. Temporal instead replays code
  deterministically. These are different ways of thinking about how a step
  survives a crash.
- **ML Metadata and artifacts.** Recording which dataset produced which model
  and with what metrics. apex-engine has nothing like it.
- **The Python SDK and its components.** Decorators, typed inputs and outputs,
  containerised components.
- **Protobuf and gRPC** as the API contract, instead of a hand-written OpenAPI
  file.

**Reading order if you want to learn Kubeflow using apex-engine as your
guide:**

1. `sdk/python/kfp/compiler`, next to our `internal/compiler`.
2. `api/v2alpha1/pipeline_spec.proto`. This is their plan format, the
   counterpart to our plan JSON.
3. `backend/src/v2/compiler/argocompiler`. This is where their plan becomes
   something the engine runs. In apex-engine that step is just starting a
   Temporal workflow.
4. `backend/src/v2/driver`, next to our `internal/runtime/workflow.go`.
5. `backend/src/apiserver`, next to our `internal/httpapi`.
6. Leave the controllers and MLMD for last.

---

## Sources

Kubeflow Pipelines
- [kubeflow/pipelines on GitHub](https://github.com/kubeflow/pipelines), [releases](https://github.com/kubeflow/pipelines/releases), [backend README](https://github.com/kubeflow/pipelines/blob/master/backend/README.md)
- [KFP overview](https://www.kubeflow.org/docs/components/pipelines/overview/), [migrating from SDK v1](https://kubeflow.org/docs/components/pipelines/v2/migration)
- [What's new in Kubeflow Pipelines v2 (Google Cloud)](https://cloud.google.com/blog/products/ai-machine-learning/whats-new-in-kubeflow-pipelines-v2/)
- [Kubeflow vs Argo (Valohai)](https://valohai.com/blog/kubeflow-vs-argo/), [Kubeflow vs Argo Workflows (Pipekit)](https://pipekit.io/blog/kubeflow-vs-argo-workflows), [Temporal alternatives (ZenML)](https://www.zenml.io/blog/temporal-alternatives)

Automation platforms and money
- n8n: [Sacra](https://sacra.com/c/n8n/), [okara.ai](https://okara.ai/blog/n8n-revenue-growth), [Sustainable Use License FAQ](https://docs.n8n.io/faircode-license), [n8n community on SaaS use](https://community.n8n.io/t/n8n-as-a-saas-backend-how-much-does-the-license-allow/238586), [commercial use guide (Japanese)](https://www.ai-souken.com/article/n8n-commercial-use-guide)
- Zapier: [Sacra](https://sacra.com/research/zapier-netflix-productivity/), [Latka](https://getlatka.com/companies/zapier)
- Workato: [Series E (2021)](https://www.businesswire.com/news/home/20211110005457/en/Workato-Raises-%24200-Million-Series-E-at-a-%245.7-Billion-Valuation-to-Accelerate-Record-Growth-and-Capitalize-on-Surging-Demand-for-Enterprise-Automation), [FY26 momentum](https://www.businesswire.com/news/home/20260227618624/en/Workato-Reports-Record-Momentum-Defining-the-Future-of-Orchestration-for-the-Agentic-Era), [TrueUp](https://www.trueup.io/co/workato)
- Make and Zapier AI features: [Latenode](https://latenode.com/blog/platform-comparisons-alternatives/zapier-alternatives/make-vs-zapier-similarities-and-differences-of-automation-tools), [Composio comparison](https://composio.dev/content/ai-agent-integration-platforms-ipaas-zapier-agent-native)
- Activepieces: [Latka](https://getlatka.com/companies/activepieces), [VCBacked](https://www.vcbacked.co/company/activepieces)
- Gumloop: [SiliconANGLE](https://siliconangle.com/2026/03/13/gumloop-reels-50m-ai-automation-platform/); Relay.app and Lindy: [Whalesync](https://www.whalesync.com/blog/best-ai-workflow-automation-tools-2025)
- Workday and Pipedream: [Reworked](https://www.reworked.co/talent-management/workday-buys-low-code-ai-integration-platform-pipedream/), [Techzine](https://www.techzine.eu/news/applications/136504/workday-acquires-pipedream-gaining-3000-integrations-with-it/)
- OpenAI Agent Builder shutting down: [cosupport.ai](https://cosupport.ai/articles/openai-agent-builder-agentkit-architecture), [AgentKit launch](https://openai.com/index/introducing-agentkit/)
- Dify: [ghtrends](https://ghtrends.dev/langgenius/dify/)

Embedding in other products
- [Paragon guide](https://www.useparagon.com/blog/embedded-ipaas-complete-guide), [Cyclr guide](https://cyclr.com/blog/best-embedded-ipaas-for-b2b-saas), [Tray list](https://tray.ai/blog/best-embedded-ipaas/), [Guideflow](https://www.guideflow.com/blog/embedded-integration-platforms), [Prismatic alternatives (viaSocket)](https://viasocket.com/blog/prismatic-alternatives)

Engines that survive crashes
- Temporal: [SiliconANGLE, $12.55B](https://siliconangle.com/2026/09/14/software-reliability-specialist-temporal-valued-at-12-55b-in-550m-round/), [TNW](https://thenextweb.com/news/temporal-550m-series-e-12-55bn-valuation-lightspeed), [pricing](https://temporal.io/pricing), [Cloud pricing docs](https://docs.temporal.io/cloud/pricing), [agents case study (ZenML)](https://www.zenml.io/llmops-database/building-reliable-production-ai-agents-with-durable-execution-infrastructure)
- Inngest, Trigger.dev, Restate: [Upstash comparison](https://upstash.com/blog/durable-workflow-engines-compared-every-major-option-in-2026), [PkgPulse](https://www.pkgpulse.com/guides/hatchet-vs-trigger-dev-v3-vs-inngest-durable-workflows-2026), [ZenML Inngest alternatives](https://www.zenml.io/blog/inngest-alternatives)
- Windmill: [pricing](https://www.windmill.dev/pricing), [Automation Atlas](https://automationatlas.io/answers/windmill-pricing-explained-2026/)

Tools and safety for AI agents
- [Arcade $60M](https://www.arcade.dev/blog/arcade-series-a/), [Composio Series A](https://composio.dev/blog/series-a), [HumanLayer (Y Combinator)](https://www.ycombinator.com/companies/humanlayer)

Complaints about AI-built workflows
- [n8n community: "AI generated n8n workflows always break"](https://community.n8n.io/t/ai-generated-n8n-workflows-always-break-i-built-an-open-source-fix/300587), [n8n AI Workflow Builder review](https://growwstacks.com/blog/n8n-ai-workflow-builder-review/), [stress test podcast](https://metacast.app/podcast/ai-fire-daily/YfRCG9xp/230-max-i-stress-tested-n8n-s-ai-workflow-builder-here-s-the-brutal-truth/6vd5vHjb)

Licences
- [Sentry: Functional Source License](https://blog.sentry.io/introducing-the-functional-source-license-freedom-without-free-riding/), [Armin Ronacher on FSL and AGPL](https://lucumr.pocoo.org/2024/9/23/fsl-agpl-open-source-businesses/), [TechCrunch timeline](https://techcrunch.com/2024/12/15/open-source-companies-that-go-proprietary-a-timeline)

GitHub growth
- [Skyvern: 10,000 stars](https://www.skyvern.com/blog/how-we-got-to-10-000-stars-why-you-should-be-launching-as-often-as-you-can/), [PostHog after the HN launch](https://posthog.com/blog/after-the-hn-launch), [ToolJet: 10,000 stars](https://blog.tooljet.com/zero-to-10-000-stargazers-on-github/), [HN launch study (arXiv)](https://arxiv.org/abs/2511.04453v1)
