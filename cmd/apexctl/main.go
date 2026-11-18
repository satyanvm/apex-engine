// Command apexctl drives an apex-engine server from a terminal or CI.
//
// Plans are files: compile one, review it, commit it, run it. Results are JSON
// on stdout, progress goes to stderr, and exit codes mean something.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/satyanvm/apex-engine/client"
)

// Exit codes. A script can branch on these without reading the output.
const (
	exitOK      = 0
	exitFailed  = 1 // the request failed, or the run failed or was cancelled
	exitUsage   = 2
	exitWaiting = 3 // --stop-at-approval: the run is waiting for a person
	exitPartial = 4 // the run finished, but some loop items failed
)

const usage = `apexctl drives an apex-engine server.

Environment: APEX_URL (default http://localhost:8700), APEX_API_KEY.
With a platform key, also APEX_TENANT, and optionally APEX_ACTOR and APEX_ROLE.

  apexctl status
  apexctl demo                 install the sample tools and workflow
  apexctl compile "<prompt>" [--tools a,b] [--name n] [-o plan.json]
  apexctl compile "<change>" --base plan.json [-o plan.json]
                               change an existing plan; the rest of it is kept
  apexctl validate plan.json [--tools a,b]
  apexctl review plan.json [--tools a,b]
                               what the plan does in plain words, and what it
                               changes with nobody asked
  apexctl preview plan.json [--tools a,b] [--input k=v]... [--timeout 10m]
                               do every read, send no write, and list what a
                               real run would do
  apexctl run plan.json [--tools a,b] [--input k=v]... [--name n] [--callback url]
                        [--idempotency-key k] [--wait] [--stop-at-approval] [--timeout 10m]
  apexctl runs [--status s] [--workflow id] [--limit n]
  apexctl get <run>
  apexctl wait <run> [--stop-at-approval] [--timeout 10m]
  apexctl approve <run> <step> [--reject] [--note text]
  apexctl cancel <run>
  apexctl retry <run> [--wait] [--stop-at-approval] [--timeout 10m]
                               continue a failed run without redoing what succeeded
  apexctl items <run> <step> [--status failed]
                               the loop items of one step

  apexctl tools
  apexctl tools add <name> (--openapi url|file | --mcp url | --piece name) [--base-url u]
                           [--header 'K: V']... [--auth value]
  apexctl tools pieces [query]      Activepieces pieces that can be added
  apexctl tools auth <id> [--header 'K: V']... [--query k=v]... [--piece value]
                          [--oauth client_credentials|refresh_token --token-url u --client-id i
                           --client-secret s --scope s --audience a --refresh-token t]
  apexctl tools oauth <id> --auth-url u --token-url u --client-id i [--client-secret s]
                           [--scope s] [--param k=v]...   prints the consent address
  apexctl tools rm <id>

  apexctl whoami
  apexctl keys
  apexctl keys add <name> --role viewer|approver|editor|operator|admin
  apexctl keys rm <id>
  apexctl audit [--action run.] [--actor name] [--target id] [--limit n]

A secret value may be written as @file to read it from a file, or env:NAME to read
it from the environment, so it stays out of your shell history.

  apexctl workflows
  apexctl workflows save plan.json [--tools a,b] [--name n] [--callback url] [--id existing]
  apexctl workflows run <id> [--input k=v]... [--wait] [--stop-at-approval] [--timeout 10m]
  apexctl workflows rm <id>

Exit codes: 0 succeeded, 1 failed, 2 bad usage, 3 waiting for approval,
4 finished but some loop items failed.
`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stdout, os.Stderr))
}

type app struct {
	c        *client.Client
	out, log io.Writer
}

type usageError string

func (e usageError) Error() string { return string(e) }

func run(ctx context.Context, args []string, out, log io.Writer) int {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		fmt.Fprint(out, usage)
		return exitOK
	}
	base := os.Getenv("APEX_URL")
	if base == "" {
		base = "http://localhost:8700"
	}
	key := os.Getenv("APEX_API_KEY")
	if key == "" {
		fmt.Fprintln(log, "apexctl: set APEX_API_KEY")
		return exitUsage
	}
	c := client.New(base, key)
	c.Tenant, c.Actor, c.Role = os.Getenv("APEX_TENANT"), os.Getenv("APEX_ACTOR"), os.Getenv("APEX_ROLE")
	a := &app{c: c, out: out, log: log}
	code, err := a.dispatch(ctx, args[0], args[1:])
	var bad usageError
	switch {
	case errors.As(err, &bad):
		fmt.Fprintf(log, "apexctl: %s\n\n%s", err, usage)
		return exitUsage
	case err != nil:
		fmt.Fprintf(log, "apexctl: %s\n", err)
		return exitFailed
	}
	return code
}

func (a *app) dispatch(ctx context.Context, cmd string, args []string) (int, error) {
	switch cmd {
	case "status":
		st, err := a.c.Status(ctx)
		return a.print(st, err)
	case "demo":
		id, err := a.c.InstallDemo(ctx)
		return a.print(map[string]string{"workflowId": id}, err)
	case "compile":
		return a.compile(ctx, args)
	case "review":
		return a.review(ctx, args)
	case "preview":
		return a.preview(ctx, args)
	case "validate":
		return a.validate(ctx, args)
	case "run":
		return a.startRun(ctx, args)
	case "runs":
		return a.listRuns(ctx, args)
	case "get":
		if len(args) != 1 {
			return 0, usageError("get needs a run id")
		}
		r, err := a.c.GetRun(ctx, args[0])
		return a.print(r, err)
	case "wait":
		return a.waitCmd(ctx, args)
	case "approve":
		return a.approve(ctx, args)
	case "cancel":
		if len(args) != 1 {
			return 0, usageError("cancel needs a run id")
		}
		return exitOK, a.c.CancelRun(ctx, args[0])
	case "retry":
		return a.retry(ctx, args)
	case "items":
		return a.items(ctx, args)
	case "whoami":
		me, err := a.c.Whoami(ctx)
		return a.print(me, err)
	case "keys":
		return a.keys(ctx, args)
	case "audit":
		return a.audit(ctx, args)
	case "tools":
		return a.tools(ctx, args)
	case "workflows":
		return a.workflows(ctx, args)
	}
	return 0, usageError("unknown command " + cmd)
}

func (a *app) print(v any, err error) (int, error) {
	if err != nil {
		return 0, err
	}
	enc := json.NewEncoder(a.out)
	enc.SetIndent("", "  ")
	return exitOK, enc.Encode(v)
}

// parse lets flags come before or after positional arguments.
func parse(fs *flag.FlagSet, args []string) ([]string, error) {
	fs.SetOutput(io.Discard)
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, usageError(err.Error())
		}
		if fs.NArg() == 0 {
			return positional, nil
		}
		positional = append(positional, fs.Arg(0))
		args = fs.Args()[1:]
	}
}

type multi []string

func (m *multi) String() string     { return strings.Join(*m, ",") }
func (m *multi) Set(v string) error { *m = append(*m, v); return nil }

// inputs turns k=v pairs into a run input. A value that parses as JSON keeps its
// type, so count=3 is a number.
func inputs(pairs []string) (map[string]any, error) {
	out := map[string]any{}
	for _, p := range pairs {
		k, v, ok := strings.Cut(p, "=")
		if !ok || k == "" {
			return nil, usageError("--input must be key=value, got " + p)
		}
		var parsed any
		if json.Unmarshal([]byte(v), &parsed) == nil {
			out[k] = parsed
		} else {
			out[k] = v
		}
	}
	return out, nil
}

// resolveTools maps comma-separated names or ids to catalog ids. An empty list
// means every tool when all is true, and none otherwise.
func (a *app) resolveTools(ctx context.Context, list string, all bool) ([]string, error) {
	if list == "" && !all {
		return nil, nil
	}
	cats, err := a.c.ListCatalogs(ctx)
	if err != nil {
		return nil, err
	}
	ids := []string{}
	if list == "" {
		for _, c := range cats {
			ids = append(ids, c.ID)
		}
		return ids, nil
	}
next:
	for _, want := range strings.Split(list, ",") {
		want = strings.TrimSpace(want)
		for _, c := range cats {
			if c.ID == want || strings.EqualFold(c.Name, want) {
				ids = append(ids, c.ID)
				continue next
			}
		}
		return nil, fmt.Errorf("no tool named %q (see apexctl tools)", want)
	}
	return ids, nil
}

// planFile is what compile writes and run reads: a plan plus the tools it needs.
type planFile struct {
	client.Plan
	CatalogIDs []string `json:"catalogIds"`
}

// readPlan also accepts the {"dag":...,"catalogIds":...} shape of POST /v1/runs.
func readPlan(path string) (*planFile, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var wrapped struct {
		DAG        *client.DAG `json:"dag"`
		CatalogIDs []string    `json:"catalogIds"`
		Name       string      `json:"name"`
	}
	if err := json.Unmarshal(raw, &wrapped); err != nil {
		return nil, fmt.Errorf("%s is not valid JSON: %w", path, err)
	}
	if wrapped.DAG != nil {
		return &planFile{Plan: client.Plan{Name: wrapped.Name, Nodes: wrapped.DAG.Nodes, DefaultInput: wrapped.DAG.DefaultInput}, CatalogIDs: wrapped.CatalogIDs}, nil
	}
	var p planFile
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, err
	}
	if len(p.Nodes) == 0 {
		return nil, fmt.Errorf("%s has no nodes", path)
	}
	return &p, nil
}

func (a *app) compile(ctx context.Context, args []string) (int, error) {
	fs := flag.NewFlagSet("compile", flag.ContinueOnError)
	tools := fs.String("tools", "", "")
	name := fs.String("name", "", "")
	output := fs.String("o", "", "")
	basePath := fs.String("base", "", "")
	pos, err := parse(fs, args)
	if err != nil {
		return 0, err
	}
	if len(pos) != 1 {
		return 0, usageError("compile needs one prompt, in quotes")
	}
	req := client.CompileRequest{Prompt: pos[0], Name: *name}
	var ids []string
	if *basePath != "" {
		base, err := readPlan(*basePath)
		if err != nil {
			return 0, err
		}
		if ids, err = a.planTools(ctx, *tools, base); err != nil {
			return 0, err
		}
		req.Base = &base.Plan
	} else if ids, err = a.resolveTools(ctx, *tools, true); err != nil {
		return 0, err
	}
	req.CatalogIDs = ids
	res, err := a.c.CompileStream(ctx, req, func(st client.Stage) {
		if st.Status != "start" {
			fmt.Fprintf(a.log, "%-9s %-7s %s\n", st.Stage, st.Status, st.Detail)
		}
	})
	if err != nil {
		return 0, err
	}
	if d := res.Diff; d != nil {
		fmt.Fprintf(a.log, "changed: %d added %v, %d removed %v, %d changed %v, %d kept\n",
			len(d.Added), d.Added, len(d.Removed), d.Removed, len(d.Changed), d.Changed, d.Unchanged)
	}
	a.printReview(a.log, &res.Review)
	file := planFile{Plan: res.Plan, CatalogIDs: ids}
	if *output == "" {
		return a.print(file, nil)
	}
	raw, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return 0, err
	}
	if err := os.WriteFile(*output, append(raw, '\n'), 0o644); err != nil {
		return 0, err
	}
	fmt.Fprintf(a.log, "wrote %s (%d steps)\n", *output, len(res.Plan.Nodes))
	return exitOK, nil
}

// planTools prefers an explicit --tools over what the file records.
func (a *app) planTools(ctx context.Context, list string, p *planFile) ([]string, error) {
	if list != "" {
		return a.resolveTools(ctx, list, false)
	}
	return p.CatalogIDs, nil
}

func (a *app) validate(ctx context.Context, args []string) (int, error) {
	fs := flag.NewFlagSet("validate", flag.ContinueOnError)
	tools := fs.String("tools", "", "")
	pos, err := parse(fs, args)
	if err != nil {
		return 0, err
	}
	if len(pos) != 1 {
		return 0, usageError("validate needs a plan file")
	}
	plan, err := readPlan(pos[0])
	if err != nil {
		return 0, err
	}
	ids, err := a.planTools(ctx, *tools, plan)
	if err != nil {
		return 0, err
	}
	problems, err := a.c.Validate(ctx, plan.DAG(), ids)
	if err != nil {
		return 0, err
	}
	if len(problems) > 0 {
		for _, p := range problems {
			fmt.Fprintln(a.out, p)
		}
		return exitFailed, nil
	}
	fmt.Fprintln(a.out, "valid")
	return exitOK, nil
}

// printReview writes a plan's plain-language description.
func (a *app) printReview(w io.Writer, rv *client.Review) {
	if rv == nil || len(rv.Steps) == 0 {
		return
	}
	fmt.Fprintln(w, "what it does:")
	for i, s := range rv.Steps {
		fmt.Fprintf(w, "  %2d. %s\n", i+1, s.Text)
		if s.Detail != "" {
			fmt.Fprintf(w, "        %s\n", s.Detail)
		}
	}
	fmt.Fprintf(w, "%d read(s), %d change(s), %d approval(s)\n", rv.Reads, rv.Changes, rv.Approvals)
	for _, warn := range rv.Warnings {
		fmt.Fprintf(w, "  ! %s\n", warn)
	}
}

func (a *app) review(ctx context.Context, args []string) (int, error) {
	fs := flag.NewFlagSet("review", flag.ContinueOnError)
	tools := fs.String("tools", "", "")
	pos, err := parse(fs, args)
	if err != nil {
		return 0, err
	}
	if len(pos) != 1 {
		return 0, usageError("review needs a plan file")
	}
	plan, err := readPlan(pos[0])
	if err != nil {
		return 0, err
	}
	ids, err := a.planTools(ctx, *tools, plan)
	if err != nil {
		return 0, err
	}
	rv, problems, err := a.c.Review(ctx, plan.DAG(), ids)
	if err != nil {
		return 0, err
	}
	if len(problems) > 0 {
		for _, p := range problems {
			fmt.Fprintln(a.out, p)
		}
		return exitFailed, nil
	}
	a.printReview(a.out, rv)
	return exitOK, nil
}

// preview runs a plan with every write held back, then lists what a real run
// would have done.
func (a *app) preview(ctx context.Context, args []string) (int, error) {
	fs := flag.NewFlagSet("preview", flag.ContinueOnError)
	tools := fs.String("tools", "", "")
	timeout := fs.Duration("timeout", 10*time.Minute, "")
	var pairs multi
	fs.Var(&pairs, "input", "")
	pos, err := parse(fs, args)
	if err != nil {
		return 0, err
	}
	if len(pos) != 1 {
		return 0, usageError("preview needs a plan file")
	}
	plan, err := readPlan(pos[0])
	if err != nil {
		return 0, err
	}
	in, err := inputs(pairs)
	if err != nil {
		return 0, err
	}
	ids, err := a.planTools(ctx, *tools, plan)
	if err != nil {
		return 0, err
	}
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	r, err := a.c.StartRun(ctx, client.RunRequest{DAG: plan.DAG(), CatalogIDs: ids, Input: in, Name: plan.Name, Preview: true})
	if err != nil {
		return 0, err
	}
	if r, err = a.c.WaitRun(ctx, r.ID, client.WaitOptions{Poll: 500 * time.Millisecond}); err != nil {
		return 0, err
	}
	pv, err := a.c.Preview(ctx, r.ID)
	if err != nil {
		return 0, err
	}
	if r.Status != client.StatusSucceeded {
		msg := r.Status
		if r.Error != nil {
			msg += ": " + *r.Error
		}
		fmt.Fprintf(a.log, "the preview stopped early (%s); a real run would stop there too\n", msg)
	}
	if len(pv.Actions) == 0 {
		fmt.Fprintln(a.out, "a run would change nothing")
	}
	for _, act := range pv.Actions {
		item := ""
		if act.Item != nil {
			item = fmt.Sprintf(" [item %d]", *act.Item)
		}
		d := act.Detail
		switch act.Kind {
		case "call":
			line := fmt.Sprintf("would call  %s%s:", act.Name, item)
			if d["method"] != nil {
				line += fmt.Sprintf(" %v %v", d["method"], d["url"])
			} else {
				line += fmt.Sprintf(" %v / %v", d["tool"], d["operation"])
			}
			for _, k := range []string{"body", "arguments"} {
				if d[k] != nil {
					raw, _ := json.Marshal(d[k])
					line += " " + string(raw)
				}
			}
			fmt.Fprintln(a.out, line)
		case "approval":
			fmt.Fprintf(a.out, "would ask   %s%s: %v\n", act.Name, item, d["message"])
		case "wait":
			fmt.Fprintf(a.out, "would wait  %s%s: %v seconds\n", act.Name, item, d["seconds"])
		default:
			fmt.Fprintf(a.out, "cannot say  %s%s: %v\n", act.Name, item, d["reason"])
		}
	}
	fmt.Fprintf(a.log, "%d call(s), %d question(s), %d wait(s), %d step(s) the preview cannot predict; nothing was changed (run %s)\n",
		pv.Counts["call"], pv.Counts["approval"], pv.Counts["wait"], pv.Counts["unknown"], r.ID)
	if r.Status != client.StatusSucceeded {
		return exitFailed, nil
	}
	return exitOK, nil
}

type waitFlags struct {
	wait, stop *bool
	timeout    *time.Duration
}

func addWait(fs *flag.FlagSet) waitFlags {
	return waitFlags{wait: fs.Bool("wait", false, ""), stop: fs.Bool("stop-at-approval", false, ""), timeout: fs.Duration("timeout", 0, "")}
}

func (a *app) startRun(ctx context.Context, args []string) (int, error) {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	tools := fs.String("tools", "", "")
	name := fs.String("name", "", "")
	callback := fs.String("callback", "", "")
	idem := fs.String("idempotency-key", "", "")
	var pairs multi
	fs.Var(&pairs, "input", "")
	w := addWait(fs)
	pos, err := parse(fs, args)
	if err != nil {
		return 0, err
	}
	if len(pos) != 1 {
		return 0, usageError("run needs a plan file")
	}
	plan, err := readPlan(pos[0])
	if err != nil {
		return 0, err
	}
	in, err := inputs(pairs)
	if err != nil {
		return 0, err
	}
	ids, err := a.planTools(ctx, *tools, plan)
	if err != nil {
		return 0, err
	}
	if *name == "" {
		*name = plan.Name
	}
	r, err := a.c.StartRun(ctx, client.RunRequest{DAG: plan.DAG(), CatalogIDs: ids, Input: in, IdempotencyKey: *idem, Name: *name, CallbackURL: *callback})
	if err != nil {
		return 0, err
	}
	return a.finish(ctx, r, w)
}

func (a *app) finish(ctx context.Context, r *client.Run, w waitFlags) (int, error) {
	if !*w.wait && !*w.stop {
		return a.print(r, nil)
	}
	fmt.Fprintf(a.log, "run %s started\n", r.ID)
	return a.follow(ctx, r.ID, *w.stop, *w.timeout)
}

// follow waits for a run, telling a person on stderr what it is waiting for.
func (a *app) follow(ctx context.Context, id string, stopAtApproval bool, timeout time.Duration) (int, error) {
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	seen := map[string]bool{}
	for {
		r, err := a.c.GetRun(ctx, id)
		if err != nil {
			return 0, err
		}
		if r.Done() {
			code, err := a.print(r, nil)
			if err != nil {
				return code, err
			}
			if r.Status != client.StatusSucceeded {
				msg := r.Status
				if r.Error != nil {
					msg += ": " + *r.Error
				}
				fmt.Fprintf(a.log, "run %s %s\n", r.ID, msg)
				if r.Status == "failed" || r.Status == "cancelled" {
					fmt.Fprintf(a.log, "  continue it without redoing what succeeded: apexctl retry %s\n", r.ID)
				}
				return exitFailed, nil
			}
			if r.FailedItems > 0 {
				fmt.Fprintf(a.log, "run %s finished, but %d loop item(s) failed\n", r.ID, r.FailedItems)
				for _, n := range r.Nodes {
					if n.ItemsFailed > 0 {
						fmt.Fprintf(a.log, "  apexctl items %s %s --status failed\n", r.ID, n.Key)
					}
				}
				return exitPartial, nil
			}
			return exitOK, nil
		}
		for _, n := range r.Waiting() {
			message := ""
			if out, ok := n.Output.(map[string]any); ok {
				message, _ = out["message"].(string)
			}
			if tag := n.Key + "\x00" + message; !seen[tag] {
				seen[tag] = true
				fmt.Fprintf(a.log, "waiting on %s: %s\n  apexctl approve %s %s\n", n.Key, message, r.ID, n.Key)
			}
		}
		if stopAtApproval && r.Status == client.StatusWaiting {
			code, err := a.print(r, nil)
			if err != nil {
				return code, err
			}
			return exitWaiting, nil
		}
		select {
		case <-ctx.Done():
			return 0, fmt.Errorf("stopped waiting for run %s: %w", id, ctx.Err())
		case <-time.After(pollEvery):
		}
	}
}

var pollEvery = time.Second

func (a *app) waitCmd(ctx context.Context, args []string) (int, error) {
	fs := flag.NewFlagSet("wait", flag.ContinueOnError)
	w := addWait(fs)
	pos, err := parse(fs, args)
	if err != nil {
		return 0, err
	}
	if len(pos) != 1 {
		return 0, usageError("wait needs a run id")
	}
	return a.follow(ctx, pos[0], *w.stop, *w.timeout)
}

func (a *app) listRuns(ctx context.Context, args []string) (int, error) {
	fs := flag.NewFlagSet("runs", flag.ContinueOnError)
	status := fs.String("status", "", "")
	workflow := fs.String("workflow", "", "")
	limit := fs.Int("limit", 50, "")
	if _, err := parse(fs, args); err != nil {
		return 0, err
	}
	runs, err := a.c.ListRuns(ctx, client.RunFilter{Limit: *limit, Status: *status, Workflow: *workflow})
	return a.print(runs, err)
}

func (a *app) approve(ctx context.Context, args []string) (int, error) {
	fs := flag.NewFlagSet("approve", flag.ContinueOnError)
	reject := fs.Bool("reject", false, "")
	note := fs.String("note", "", "")
	pos, err := parse(fs, args)
	if err != nil {
		return 0, err
	}
	if len(pos) != 2 {
		return 0, usageError("approve needs a run id and a step key")
	}
	return exitOK, a.c.Decide(ctx, pos[0], pos[1], !*reject, *note)
}

func (a *app) tools(ctx context.Context, args []string) (int, error) {
	if len(args) == 0 {
		cats, err := a.c.ListCatalogs(ctx)
		return a.print(cats, err)
	}
	switch args[0] {
	case "add":
		return a.addTool(ctx, args[1:])
	case "pieces":
		q := ""
		if len(args) > 1 {
			q = args[1]
		}
		pieces, err := a.c.ListPieces(ctx, q)
		return a.print(pieces, err)
	case "auth":
		return a.toolAuth(ctx, args[1:])
	case "oauth":
		return a.toolOAuth(ctx, args[1:])
	case "rm":
		if len(args) != 2 {
			return 0, usageError("tools rm needs an id")
		}
		return exitOK, a.c.DeleteCatalog(ctx, args[1])
	}
	return 0, usageError("unknown tools command " + args[0])
}

func (a *app) addTool(ctx context.Context, args []string) (int, error) {
	fs := flag.NewFlagSet("tools add", flag.ContinueOnError)
	openapi := fs.String("openapi", "", "")
	mcp := fs.String("mcp", "", "")
	base := fs.String("base-url", "", "")
	piece := fs.String("piece", "", "")
	pieceAuth := fs.String("auth", "", "")
	var headerArgs multi
	fs.Var(&headerArgs, "header", "")
	pos, err := parse(fs, args)
	if err != nil {
		return 0, err
	}
	sources := 0
	for _, v := range []string{*openapi, *mcp, *piece} {
		if v != "" {
			sources++
		}
	}
	if len(pos) != 1 || sources != 1 {
		return 0, usageError("tools add needs a name and exactly one of --openapi, --mcp or --piece")
	}
	headers, err := headerMap(headerArgs)
	if err != nil {
		return 0, err
	}
	req := client.CatalogRequest{Name: pos[0], BaseURL: *base}
	switch {
	case *piece != "":
		req.Source = client.CatalogSource{Type: "piece", Piece: *piece}
		if *pieceAuth != "" {
			v, err := secretValue(*pieceAuth)
			if err != nil {
				return 0, err
			}
			req.Source.Auth = jsonOrText(v)
		}
	case *mcp != "":
		req.Source = client.CatalogSource{Type: "mcp", URL: *mcp, Headers: headers}
	case strings.HasPrefix(*openapi, "http://") || strings.HasPrefix(*openapi, "https://"):
		req.Source = client.CatalogSource{Type: "openapi", URL: *openapi}
	default:
		raw, err := os.ReadFile(*openapi)
		if err != nil {
			return 0, err
		}
		req.Source = client.CatalogSource{Type: "openapi", Spec: string(raw)}
	}
	cat, err := a.c.CreateCatalog(ctx, req)
	if err != nil {
		return 0, err
	}
	// MCP headers were stored at import; OpenAPI credentials are set separately.
	if len(headers) > 0 && *mcp == "" && *piece == "" {
		if err := a.c.SetCredentials(ctx, cat.ID, headers); err != nil {
			return 0, fmt.Errorf("imported %s, but storing its credentials failed: %w", cat.ID, err)
		}
		cat.HasCredentials = true
	}
	return a.print(cat, nil)
}

func (a *app) workflows(ctx context.Context, args []string) (int, error) {
	if len(args) == 0 {
		list, err := a.c.ListWorkflows(ctx)
		return a.print(list, err)
	}
	switch args[0] {
	case "save":
		return a.saveWorkflow(ctx, args[1:])
	case "run":
		return a.runWorkflow(ctx, args[1:])
	case "rm":
		if len(args) != 2 {
			return 0, usageError("workflows rm needs an id")
		}
		return exitOK, a.c.DeleteWorkflow(ctx, args[1])
	}
	return 0, usageError("unknown workflows command " + args[0])
}

func (a *app) saveWorkflow(ctx context.Context, args []string) (int, error) {
	fs := flag.NewFlagSet("workflows save", flag.ContinueOnError)
	tools := fs.String("tools", "", "")
	name := fs.String("name", "", "")
	callback := fs.String("callback", "", "")
	id := fs.String("id", "", "")
	pos, err := parse(fs, args)
	if err != nil {
		return 0, err
	}
	if len(pos) != 1 {
		return 0, usageError("workflows save needs a plan file")
	}
	plan, err := readPlan(pos[0])
	if err != nil {
		return 0, err
	}
	ids, err := a.planTools(ctx, *tools, plan)
	if err != nil {
		return 0, err
	}
	if *name == "" {
		*name = plan.Name
	}
	wf := client.Workflow{Name: *name, Description: plan.Description, Nodes: plan.Nodes, DefaultInput: plan.DefaultInput, CatalogIDs: ids, CallbackURL: *callback}
	if *id != "" {
		out, err := a.c.UpdateWorkflow(ctx, *id, wf)
		return a.print(out, err)
	}
	out, err := a.c.CreateWorkflow(ctx, wf)
	return a.print(out, err)
}

func (a *app) runWorkflow(ctx context.Context, args []string) (int, error) {
	fs := flag.NewFlagSet("workflows run", flag.ContinueOnError)
	idem := fs.String("idempotency-key", "", "")
	callback := fs.String("callback", "", "")
	var pairs multi
	fs.Var(&pairs, "input", "")
	w := addWait(fs)
	pos, err := parse(fs, args)
	if err != nil {
		return 0, err
	}
	if len(pos) != 1 {
		return 0, usageError("workflows run needs an id")
	}
	in, err := inputs(pairs)
	if err != nil {
		return 0, err
	}
	r, err := a.c.RunWorkflow(ctx, pos[0], client.WorkflowRunRequest{Input: in, IdempotencyKey: *idem, CallbackURL: *callback})
	if err != nil {
		return 0, err
	}
	return a.finish(ctx, r, w)
}

/* ------------------------------ secrets on flags ---------------------------- */

// secretValue reads a flag value that may point at a file (@path) or an
// environment variable (env:NAME), so a secret need not be typed inline.
func secretValue(v string) (string, error) {
	switch {
	case strings.HasPrefix(v, "@"):
		raw, err := os.ReadFile(v[1:])
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(string(raw)), nil
	case strings.HasPrefix(v, "env:"):
		got, ok := os.LookupEnv(v[4:])
		if !ok {
			return "", usageError("environment variable " + v[4:] + " is not set")
		}
		return got, nil
	}
	return v, nil
}

// jsonOrText passes a JSON value through as JSON and anything else as a string.
func jsonOrText(v string) any {
	var parsed any
	if t := strings.TrimSpace(v); (strings.HasPrefix(t, "{") || strings.HasPrefix(t, "[")) && json.Unmarshal([]byte(t), &parsed) == nil {
		return parsed
	}
	return v
}

func headerMap(args []string) (map[string]string, error) {
	out := map[string]string{}
	for _, h := range args {
		k, v, ok := strings.Cut(h, ":")
		if !ok {
			return nil, usageError("--header must be 'Name: value'")
		}
		val, err := secretValue(strings.TrimSpace(v))
		if err != nil {
			return nil, err
		}
		out[strings.TrimSpace(k)] = val
	}
	return out, nil
}

func pairMap(flagName string, args []string) (map[string]string, error) {
	out := map[string]string{}
	for _, p := range args {
		k, v, ok := strings.Cut(p, "=")
		if !ok || k == "" {
			return nil, usageError("--" + flagName + " must be key=value, got " + p)
		}
		val, err := secretValue(v)
		if err != nil {
			return nil, err
		}
		out[k] = val
	}
	return out, nil
}

/* ------------------------------- tool credentials --------------------------- */

func (a *app) toolAuth(ctx context.Context, args []string) (int, error) {
	fs := flag.NewFlagSet("tools auth", flag.ContinueOnError)
	var headerArgs, queryArgs multi
	fs.Var(&headerArgs, "header", "")
	fs.Var(&queryArgs, "query", "")
	piece := fs.String("piece", "", "")
	grant := fs.String("oauth", "", "")
	tokenURL := fs.String("token-url", "", "")
	clientID := fs.String("client-id", "", "")
	clientSecret := fs.String("client-secret", "", "")
	scope := fs.String("scope", "", "")
	audience := fs.String("audience", "", "")
	refresh := fs.String("refresh-token", "", "")
	pos, err := parse(fs, args)
	if err != nil {
		return 0, err
	}
	if len(pos) != 1 {
		return 0, usageError("tools auth needs a tool id")
	}
	var creds client.Credentials
	if creds.Headers, err = headerMap(headerArgs); err != nil {
		return 0, err
	}
	if creds.Query, err = pairMap("query", queryArgs); err != nil {
		return 0, err
	}
	if *piece != "" {
		v, err := secretValue(*piece)
		if err != nil {
			return 0, err
		}
		creds.Piece = jsonOrText(v)
	}
	if *grant != "" {
		secret, err := secretValue(*clientSecret)
		if err != nil {
			return 0, err
		}
		token, err := secretValue(*refresh)
		if err != nil {
			return 0, err
		}
		creds.OAuth = &client.OAuthClient{Grant: *grant, TokenURL: *tokenURL, ClientID: *clientID, ClientSecret: secret, Scope: *scope, Audience: *audience, RefreshToken: token}
	}
	cat, err := a.c.SetAuth(ctx, pos[0], creds)
	return a.print(cat, err)
}

func (a *app) toolOAuth(ctx context.Context, args []string) (int, error) {
	fs := flag.NewFlagSet("tools oauth", flag.ContinueOnError)
	authURL := fs.String("auth-url", "", "")
	tokenURL := fs.String("token-url", "", "")
	clientID := fs.String("client-id", "", "")
	clientSecret := fs.String("client-secret", "", "")
	scope := fs.String("scope", "", "")
	var paramArgs multi
	fs.Var(&paramArgs, "param", "")
	pos, err := parse(fs, args)
	if err != nil {
		return 0, err
	}
	if len(pos) != 1 || *authURL == "" || *tokenURL == "" || *clientID == "" {
		return 0, usageError("tools oauth needs a tool id, --auth-url, --token-url and --client-id")
	}
	secret, err := secretValue(*clientSecret)
	if err != nil {
		return 0, err
	}
	params, err := pairMap("param", paramArgs)
	if err != nil {
		return 0, err
	}
	start, err := a.c.StartOAuth(ctx, pos[0], client.OAuthClient{AuthURL: *authURL, TokenURL: *tokenURL, ClientID: *clientID, ClientSecret: secret, Scope: *scope, AuthParams: params})
	if err != nil {
		return 0, err
	}
	fmt.Fprintf(a.log, "Register this redirect address with the provider, if you have not:\n  %s\nThen open the authorizeUrl below in a browser. It is good for %d minutes.\n", start.RedirectURL, start.ExpiresInSeconds/60)
	return a.print(start, nil)
}

/* ----------------------------- resume, items, access ------------------------ */

func (a *app) retry(ctx context.Context, args []string) (int, error) {
	fs := flag.NewFlagSet("retry", flag.ContinueOnError)
	w := addWait(fs)
	pos, err := parse(fs, args)
	if err != nil {
		return 0, err
	}
	if len(pos) != 1 {
		return 0, usageError("retry needs a run id")
	}
	r, err := a.c.RetryRun(ctx, pos[0])
	if err != nil {
		return 0, err
	}
	return a.finish(ctx, r, w)
}

func (a *app) items(ctx context.Context, args []string) (int, error) {
	fs := flag.NewFlagSet("items", flag.ContinueOnError)
	status := fs.String("status", "", "")
	pos, err := parse(fs, args)
	if err != nil {
		return 0, err
	}
	if len(pos) != 2 {
		return 0, usageError("items needs a run id and a step key")
	}
	items, err := a.c.ListItems(ctx, pos[0], pos[1], *status)
	if items == nil {
		items = []client.ItemRun{}
	}
	return a.print(items, err)
}

func (a *app) keys(ctx context.Context, args []string) (int, error) {
	if len(args) == 0 {
		keys, err := a.c.ListKeys(ctx)
		return a.print(keys, err)
	}
	switch args[0] {
	case "add":
		fs := flag.NewFlagSet("keys add", flag.ContinueOnError)
		role := fs.String("role", "", "")
		pos, err := parse(fs, args[1:])
		if err != nil {
			return 0, err
		}
		if len(pos) != 1 || *role == "" {
			return 0, usageError("keys add needs a name and --role")
		}
		k, err := a.c.CreateKey(ctx, pos[0], *role)
		if err == nil {
			fmt.Fprintln(a.log, "This is the only time the key is shown. Store it now.")
		}
		return a.print(k, err)
	case "rm":
		if len(args) != 2 {
			return 0, usageError("keys rm needs an id")
		}
		return exitOK, a.c.RevokeKey(ctx, args[1])
	}
	return 0, usageError("unknown keys command " + args[0])
}

func (a *app) audit(ctx context.Context, args []string) (int, error) {
	fs := flag.NewFlagSet("audit", flag.ContinueOnError)
	var f client.AuditFilter
	fs.StringVar(&f.Action, "action", "", "")
	fs.StringVar(&f.Actor, "actor", "", "")
	fs.StringVar(&f.Target, "target", "", "")
	fs.IntVar(&f.Limit, "limit", 50, "")
	if _, err := parse(fs, args); err != nil {
		return 0, err
	}
	entries, err := a.c.ListAudit(ctx, f)
	if entries == nil {
		entries = []client.AuditEntry{}
	}
	return a.print(entries, err)
}
