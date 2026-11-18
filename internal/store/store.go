// Package store is the engine's Postgres persistence.
package store

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/satyanvm/apex-engine/internal/compiler"
	"github.com/satyanvm/apex-engine/internal/secret"
)

//go:embed migrations/*.sql
var migrations embed.FS

// ErrNotFound means the row is absent or belongs to another tenant.
var ErrNotFound = errors.New("not found")

// ErrConflict means a unique constraint was hit.
var ErrConflict = errors.New("already exists")

// Store wraps the pool and the credential box.
type Store struct {
	Pool *pgxpool.Pool
	Box  *secret.Box
}

// Connect opens the pool and applies migrations under an advisory lock, so
// several replicas can start at once.
func Connect(ctx context.Context, url string, box *secret.Box) (*Store, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, err
	}
	s := &Store{Pool: pool, Box: box}
	if err := s.migrate(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return s, nil
}

func (s *Store) migrate(ctx context.Context) error {
	conn, err := s.Pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock(7263548)`); err != nil {
		return err
	}
	defer func() { _, _ = conn.Exec(context.Background(), `SELECT pg_advisory_unlock(7263548)`) }()
	entries, err := migrations.ReadDir("migrations")
	if err != nil {
		return err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	for _, name := range names {
		sql, err := migrations.ReadFile("migrations/" + name)
		if err != nil {
			return err
		}
		if _, err := conn.Exec(ctx, string(sql)); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}
	return nil
}

// NewID returns prefix_<random hex>.
func NewID(prefix string) string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return prefix + "_" + hex.EncodeToString(b)
}

func isUnique(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "23505"
}

/* --------------------------------- catalogs -------------------------------- */

// Catalog is one imported tool source.
type Catalog struct {
	ID                string   `json:"id"`
	Tenant            string   `json:"-"`
	Name              string   `json:"name"`
	Slug              string   `json:"-"`
	Kind              string   `json:"kind"`
	BaseURL           string   `json:"baseUrl"`
	NodeCount         int      `json:"nodeCount"`
	HasCredentials    bool     `json:"hasCredentials"`
	CredentialHeaders []string `json:"credentialHeaders"`
	// Auth says how the engine authenticates to this tool source: none, headers,
	// oauth2, oauth2_pending (consent not finished) or piece.
	Auth      string                 `json:"auth"`
	CreatedAt time.Time              `json:"createdAt"`
	Nodes     []compiler.CatalogNode `json:"nodes,omitempty"`
}

func aad(tenant, id string) []byte { return []byte(tenant + "\x00" + id) }

// CreateCatalog inserts a catalog with optional credentials.
func (s *Store) CreateCatalog(ctx context.Context, c *Catalog, creds *Credentials) error {
	nodes, err := json.Marshal(c.Nodes)
	if err != nil {
		return err
	}
	sealed, names, kind, err := s.sealCredentials(c.Tenant, c.ID, creds)
	if err != nil {
		return err
	}
	err = s.Pool.QueryRow(ctx, `INSERT INTO catalogs (id,tenant,name,slug,kind,base_url,nodes,credentials,credential_headers,auth_kind)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) RETURNING created_at`,
		c.ID, c.Tenant, c.Name, c.Slug, c.Kind, c.BaseURL, nodes, sealed, names, kind).Scan(&c.CreatedAt)
	if isUnique(err) {
		return ErrConflict
	}
	c.CredentialHeaders, c.HasCredentials, c.NodeCount, c.Auth = names, kind != "", len(c.Nodes), authLabel(kind)
	return err
}

func authLabel(kind string) string {
	if kind == "" {
		return "none"
	}
	return kind
}

const catalogCols = `id,tenant,name,slug,kind,base_url,jsonb_array_length(nodes),credential_headers,created_at,auth_kind`

func scanCatalog(row pgx.Row) (*Catalog, error) {
	var c Catalog
	var kind string
	err := row.Scan(&c.ID, &c.Tenant, &c.Name, &c.Slug, &c.Kind, &c.BaseURL, &c.NodeCount, &c.CredentialHeaders, &c.CreatedAt, &kind)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	c.HasCredentials, c.Auth = kind != "", authLabel(kind)
	if c.CredentialHeaders == nil {
		c.CredentialHeaders = []string{}
	}
	return &c, nil
}

// ListCatalogs returns a tenant's catalogs without nodes.
func (s *Store) ListCatalogs(ctx context.Context, tenant string) ([]*Catalog, error) {
	rows, err := s.Pool.Query(ctx, `SELECT `+catalogCols+` FROM catalogs WHERE tenant=$1 ORDER BY created_at DESC`, tenant)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Catalog{}
	for rows.Next() {
		c, err := scanCatalog(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// GetCatalog returns a catalog with its nodes.
func (s *Store) GetCatalog(ctx context.Context, tenant, id string) (*Catalog, error) {
	c, err := scanCatalog(s.Pool.QueryRow(ctx, `SELECT `+catalogCols+` FROM catalogs WHERE tenant=$1 AND id=$2`, tenant, id))
	if err != nil {
		return nil, err
	}
	var raw []byte
	if err := s.Pool.QueryRow(ctx, `SELECT nodes FROM catalogs WHERE id=$1`, id).Scan(&raw); err != nil {
		return nil, err
	}
	return c, json.Unmarshal(raw, &c.Nodes)
}

// GetCatalogs loads several catalogs; any missing id is an error naming it.
func (s *Store) GetCatalogs(ctx context.Context, tenant string, ids []string) ([]*Catalog, error) {
	out := make([]*Catalog, 0, len(ids))
	for _, id := range ids {
		c, err := s.GetCatalog(ctx, tenant, id)
		if errors.Is(err, ErrNotFound) {
			return nil, fmt.Errorf("catalog %s: %w", id, ErrNotFound)
		}
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, nil
}

// DeleteCatalog removes a catalog.
func (s *Store) DeleteCatalog(ctx context.Context, tenant, id string) error {
	tag, err := s.Pool.Exec(ctx, `DELETE FROM catalogs WHERE tenant=$1 AND id=$2`, tenant, id)
	if err == nil && tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return err
}

// BuildPlannerCatalog merges the built-ins with the given catalogs. Large APIs
// are split into one group per tag, so the map phase reads focused lists.
func BuildPlannerCatalog(catalogs []*Catalog) *compiler.Catalog {
	cat := compiler.BuiltinCatalog()
	for _, c := range catalogs {
		groups := map[string][]compiler.CatalogNode{}
		if len(c.Nodes) > 40 {
			for _, n := range c.Nodes {
				g, _ := n.Exec["group"].(string)
				groups[g] = append(groups[g], n)
			}
		} else {
			groups[""] = c.Nodes
		}
		for g, nodes := range groups {
			key, label := c.Slug, c.Name
			if g != "" {
				key, label = c.Slug+"/"+g, c.Name+": "+g
			}
			cat.AddCluster(key, label, c.Name+" ("+c.Kind+")", nodes)
		}
	}
	return cat
}

/* ----------------------------------- runs ---------------------------------- */

// Node statuses.
const (
	StatusPending = "pending"
	StatusRunning = "running"
	StatusWaiting = "waiting_approval"
	StatusSuccess = "succeeded"
	StatusFailed  = "failed"
	StatusSkipped = "skipped"
	RunQueued     = "queued"
	RunCancelled  = "cancelled"
)

// Run is one execution of a DAG.
type Run struct {
	ID         string         `json:"id"`
	Status     string         `json:"status"`
	CreatedAt  time.Time      `json:"createdAt"`
	FinishedAt *time.Time     `json:"finishedAt"`
	Error      *string        `json:"error"`
	Input      map[string]any `json:"input"`
	Nodes      []NodeRun      `json:"nodes,omitempty"`
	WorkflowID string         `json:"-"`

	// Name labels the run; SavedWorkflow is the workflow it came from, if any;
	// Source says what started it: api, webhook or schedule.
	Name          string   `json:"name"`
	SavedWorkflow *string  `json:"workflowId"`
	Source        string   `json:"source"`
	CatalogIDs    []string `json:"catalogIds"`
	// Dag is the graph that ran. Only a single-run read carries it.
	Dag []compiler.GraphNode `json:"dag,omitempty"`
	// ResumeOf is the failed run this one continues, if any.
	ResumeOf *string `json:"resumeOf"`
	// StartedBy is the API key name, "webhook" or "schedule".
	StartedBy string `json:"startedBy"`
	// FailedItems counts loop items that failed in a loop set to continue.
	FailedItems int     `json:"failedItems"`
	IdemRoot    *string `json:"-"`
	// Preview marks a run that made its reads and only recorded its writes.
	Preview bool `json:"preview"`
}

// RunMeta describes where a run came from.
type RunMeta struct {
	Name          string
	SavedWorkflow string
	Source        string
	// CallbackURL receives this run's events. It is not stored on the run.
	CallbackURL string
	// ResumeOf and IdemRoot are set when the run continues a failed one.
	ResumeOf, IdemRoot string
	StartedBy          string
	// Preview makes the run record its writes instead of sending them.
	Preview bool
}

// NodeRun is one node's execution state.
type NodeRun struct {
	Key        string     `json:"key"`
	Type       string     `json:"type"`
	Name       string     `json:"name"`
	Status     string     `json:"status"`
	StartedAt  *time.Time `json:"startedAt"`
	FinishedAt *time.Time `json:"finishedAt"`
	Output     any        `json:"output"`
	Error      *string    `json:"error"`
	Attempts   int        `json:"attempts"`
	// ItemsSucceeded and ItemsFailed count this node's loop items, when it is
	// inside a loop.
	ItemsSucceeded int `json:"itemsSucceeded,omitempty"`
	ItemsFailed    int `json:"itemsFailed,omitempty"`
	// Reused means the result was carried over from the run this one resumes.
	Reused bool `json:"reused,omitempty"`
}

// CreateRun inserts a run and its node rows in one transaction. With an
// idempotency key that was already used it returns the earlier run and created=false.
func (s *Store) CreateRun(ctx context.Context, tenant, id, workflowID, idemKey string, nodes []compiler.GraphNode, input map[string]any, catalogIDs []string, meta RunMeta) (*Run, bool, error) {
	dag, err := json.Marshal(nodes)
	if err != nil {
		return nil, false, err
	}
	in, err := json.Marshal(input)
	if err != nil {
		return nil, false, err
	}
	var key *string
	if idemKey != "" {
		key = &idemKey
	}
	if catalogIDs == nil {
		catalogIDs = []string{}
	}
	if meta.Source == "" {
		meta.Source = "api"
	}
	var saved *string
	if meta.SavedWorkflow != "" {
		saved = &meta.SavedWorkflow
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	var resumeOf, idemRoot *string
	if meta.ResumeOf != "" {
		resumeOf = &meta.ResumeOf
	}
	if meta.IdemRoot != "" {
		idemRoot = &meta.IdemRoot
	}
	_, err = tx.Exec(ctx, `INSERT INTO runs (id,tenant,status,dag,input,catalog_ids,workflow_id,idempotency_key,name,workflow_ref,source,resume_of,idem_root,started_by,preview) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)`,
		id, tenant, RunQueued, dag, in, catalogIDs, workflowID, key, meta.Name, saved, meta.Source, resumeOf, idemRoot, meta.StartedBy, meta.Preview)
	if isUnique(err) {
		var existing string
		if err := s.Pool.QueryRow(ctx, `SELECT id FROM runs WHERE tenant=$1 AND idempotency_key=$2`, tenant, idemKey).Scan(&existing); err != nil {
			return nil, false, err
		}
		r, err := s.GetRun(ctx, tenant, existing)
		return r, false, err
	}
	if err != nil {
		return nil, false, err
	}
	for i, n := range nodes {
		if _, err := tx.Exec(ctx, `INSERT INTO node_runs (run_id,key,position,type,name,status) VALUES ($1,$2,$3,$4,$5,$6)`,
			id, n.Key, i, n.Type, n.Name, StatusPending); err != nil {
			return nil, false, err
		}
	}
	if _, err := tx.Exec(ctx, bumpSQL, "runs_started", tenant, meta.Source, 1); err != nil {
		return nil, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, false, err
	}
	r, err := s.GetRun(ctx, tenant, id)
	return r, true, err
}

const runCols = `id,status,created_at,finished_at,error,input,workflow_id,name,workflow_ref,source,catalog_ids,resume_of,idem_root,started_by,preview,
	(SELECT count(*) FROM node_item_runs i WHERE i.run_id=runs.id AND i.status='failed')`

func scanRun(row pgx.Row) (*Run, error) {
	var (
		r  Run
		in []byte
	)
	err := row.Scan(&r.ID, &r.Status, &r.CreatedAt, &r.FinishedAt, &r.Error, &in, &r.WorkflowID, &r.Name, &r.SavedWorkflow, &r.Source, &r.CatalogIDs,
		&r.ResumeOf, &r.IdemRoot, &r.StartedBy, &r.Preview, &r.FailedItems)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &r, json.Unmarshal(in, &r.Input)
}

// GetRun returns a run with its nodes.
func (s *Store) GetRun(ctx context.Context, tenant, id string) (*Run, error) {
	r, err := scanRun(s.Pool.QueryRow(ctx, `SELECT `+runCols+` FROM runs WHERE tenant=$1 AND id=$2`, tenant, id))
	if err != nil {
		return nil, err
	}
	var dag []byte
	if err := s.Pool.QueryRow(ctx, `SELECT dag FROM runs WHERE id=$1`, id).Scan(&dag); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(dag, &r.Dag); err != nil {
		return nil, err
	}
	rows, err := s.Pool.Query(ctx, `SELECT key,type,name,status,started_at,finished_at,output,error,attempts,reused FROM node_runs WHERE run_id=$1 ORDER BY position`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	r.Nodes = []NodeRun{}
	for rows.Next() {
		var (
			n   NodeRun
			out []byte
		)
		if err := rows.Scan(&n.Key, &n.Type, &n.Name, &n.Status, &n.StartedAt, &n.FinishedAt, &out, &n.Error, &n.Attempts, &n.Reused); err != nil {
			return nil, err
		}
		if out != nil {
			if err := json.Unmarshal(out, &n.Output); err != nil {
				return nil, err
			}
		}
		r.Nodes = append(r.Nodes, n)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	counts, err := s.Pool.Query(ctx, `SELECT key, count(*) FILTER (WHERE status='succeeded'), count(*) FILTER (WHERE status='failed') FROM node_item_runs WHERE run_id=$1 GROUP BY key`, id)
	if err != nil {
		return nil, err
	}
	defer counts.Close()
	for counts.Next() {
		var (
			key      string
			ok, fail int
		)
		if err := counts.Scan(&key, &ok, &fail); err != nil {
			return nil, err
		}
		for i := range r.Nodes {
			if r.Nodes[i].Key == key {
				r.Nodes[i].ItemsSucceeded, r.Nodes[i].ItemsFailed = ok, fail
			}
		}
	}
	return r, counts.Err()
}

// ListRuns returns a tenant's recent runs without nodes. An empty status or
// workflow means no filter on it.
func (s *Store) ListRuns(ctx context.Context, tenant string, limit int, status, workflow string) ([]*Run, error) {
	rows, err := s.Pool.Query(ctx, `SELECT `+runCols+` FROM runs WHERE tenant=$1 AND ($3='' OR status=$3) AND ($4='' OR workflow_ref=$4) ORDER BY created_at DESC LIMIT $2`, tenant, limit, status, workflow)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Run{}
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// MarkRunStarted moves a queued run to running.
func (s *Store) MarkRunStarted(ctx context.Context, runID string) error {
	_, err := s.Pool.Exec(ctx, `UPDATE runs SET status='running' WHERE id=$1 AND status='queued'`, runID)
	return err
}

// RefreshRunStatus sets running or waiting_approval from the node rows.
func (s *Store) RefreshRunStatus(ctx context.Context, runID string) error {
	_, err := s.Pool.Exec(ctx, `UPDATE runs SET status = CASE WHEN EXISTS (SELECT 1 FROM node_runs WHERE run_id=$1 AND status='waiting_approval')
		THEN 'waiting_approval' ELSE 'running' END WHERE id=$1 AND finished_at IS NULL`, runID)
	return err
}

// NodeRef names a node of a run, and the loop item when it is inside a loop.
type NodeRef struct {
	RunID, Key string
	HasItem    bool
	Index      int
}

// NodeStart records an attempt beginning.
func (s *Store) NodeStart(ctx context.Context, ref NodeRef) error {
	if _, err := s.Pool.Exec(ctx, `UPDATE node_runs SET status='running', started_at=COALESCE(started_at, now()), attempts=attempts+1, error=NULL WHERE run_id=$1 AND key=$2`, ref.RunID, ref.Key); err != nil {
		return err
	}
	if !ref.HasItem {
		return nil
	}
	_, err := s.Pool.Exec(ctx, `INSERT INTO node_item_runs (run_id,key,idx,status,started_at,attempts) VALUES ($1,$2,$3,'running',now(),1)
		ON CONFLICT (run_id,key,idx) DO UPDATE SET status='running', attempts=node_item_runs.attempts+1, error=NULL`, ref.RunID, ref.Key, ref.Index)
	return err
}

// NodeReuse records a result carried over from the run being resumed.
func (s *Store) NodeReuse(ctx context.Context, ref NodeRef, output map[string]any) error {
	out, err := json.Marshal(output)
	if err != nil {
		return err
	}
	if _, err := s.Pool.Exec(ctx, `UPDATE node_runs SET status='succeeded', output=$3, error=NULL, reused=true, started_at=COALESCE(started_at, now()), finished_at=now() WHERE run_id=$1 AND key=$2`, ref.RunID, ref.Key, out); err != nil {
		return err
	}
	if !ref.HasItem {
		return nil
	}
	_, err = s.Pool.Exec(ctx, `INSERT INTO node_item_runs (run_id,key,idx,status,started_at,finished_at,output,attempts) VALUES ($1,$2,$3,'succeeded',now(),now(),$4,0)
		ON CONFLICT (run_id,key,idx) DO UPDATE SET status='succeeded', output=$4, error=NULL, finished_at=now()`, ref.RunID, ref.Key, ref.Index, out)
	return err
}

// NodeFinish records a node's terminal or waiting state.
func (s *Store) NodeFinish(ctx context.Context, ref NodeRef, status string, output any, errMsg string) error {
	var out []byte
	if output != nil {
		var err error
		if out, err = json.Marshal(output); err != nil {
			return err
		}
	}
	var msg *string
	if errMsg != "" {
		msg = &errMsg
	}
	finished := "now()"
	if status == StatusWaiting {
		finished = "NULL"
	}
	if _, err := s.Pool.Exec(ctx, `UPDATE node_runs SET status=$3, output=COALESCE($4, output), error=$5, finished_at=`+finished+` WHERE run_id=$1 AND key=$2`, ref.RunID, ref.Key, status, out, msg); err != nil {
		return err
	}
	if !ref.HasItem {
		return nil
	}
	_, err := s.Pool.Exec(ctx, `INSERT INTO node_item_runs (run_id,key,idx,status,started_at,finished_at,output,error) VALUES ($1,$2,$3,$4,now(),`+finished+`,$5,$6)
		ON CONFLICT (run_id,key,idx) DO UPDATE SET status=$4, output=COALESCE($5, node_item_runs.output), error=$6, finished_at=`+finished, ref.RunID, ref.Key, ref.Index, status, out, msg)
	return err
}

// FinishRun closes a run; nodes that never finished become skipped.
func (s *Store) FinishRun(ctx context.Context, runID, status, errMsg string) error {
	var msg *string
	if errMsg != "" {
		msg = &errMsg
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(ctx, `UPDATE node_runs SET status='skipped', finished_at=now() WHERE run_id=$1 AND status IN ('pending','running','waiting_approval')`, runID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE node_item_runs SET status='skipped', finished_at=now() WHERE run_id=$1 AND status IN ('running','waiting_approval')`, runID); err != nil {
		return err
	}
	// Only the first close counts, so a retried activity does not count a run twice.
	var tenant string
	err = tx.QueryRow(ctx, `UPDATE runs SET status=$2, error=$3, finished_at=now() WHERE id=$1 AND finished_at IS NULL RETURNING tenant`, runID, status, msg).Scan(&tenant)
	if errors.Is(err, pgx.ErrNoRows) {
		return tx.Commit(ctx)
	}
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, bumpSQL, "runs_finished", tenant, status, 1); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

/* --------------------------------- schedules ------------------------------- */

// Schedule is a recurring run.
type Schedule struct {
	ID         string    `json:"id"`
	Cron       string    `json:"cron"`
	CreatedAt  time.Time `json:"createdAt"`
	Paused     bool      `json:"paused"`
	WorkflowID *string   `json:"workflowId"`
}

// CreateSchedule inserts a schedule row.
func (s *Store) CreateSchedule(ctx context.Context, tenant string, sc *Schedule) error {
	return s.Pool.QueryRow(ctx, `INSERT INTO schedules (id,tenant,cron,workflow_ref) VALUES ($1,$2,$3,$4) RETURNING created_at`, sc.ID, tenant, sc.Cron, sc.WorkflowID).Scan(&sc.CreatedAt)
}

// ListSchedules returns a tenant's schedules.
func (s *Store) ListSchedules(ctx context.Context, tenant string) ([]*Schedule, error) {
	rows, err := s.Pool.Query(ctx, `SELECT id,cron,created_at,workflow_ref FROM schedules WHERE tenant=$1 ORDER BY created_at DESC`, tenant)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Schedule{}
	for rows.Next() {
		var sc Schedule
		if err := rows.Scan(&sc.ID, &sc.Cron, &sc.CreatedAt, &sc.WorkflowID); err != nil {
			return nil, err
		}
		out = append(out, &sc)
	}
	return out, rows.Err()
}

// DeleteSchedule removes a schedule row.
func (s *Store) DeleteSchedule(ctx context.Context, tenant, id string) error {
	tag, err := s.Pool.Exec(ctx, `DELETE FROM schedules WHERE tenant=$1 AND id=$2`, tenant, id)
	if err == nil && tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return err
}
