package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

/* -------------------------------- credentials ------------------------------ */

// Credentials is everything the engine holds to authenticate to one tool
// source. It is sealed as a whole; nothing in it is ever returned by the API.
type Credentials struct {
	// Headers are sent on every request to the source.
	Headers map[string]string `json:"headers,omitempty"`
	// Query parameters are added to every request, for APIs that take a key there.
	Query map[string]string `json:"query,omitempty"`
	// OAuth makes the engine fetch, cache and refresh a bearer token.
	OAuth *OAuth `json:"oauth,omitempty"`
	// Piece is the connection value an Activepieces piece expects.
	Piece json.RawMessage `json:"piece,omitempty"`
}

// OAuth grants.
const (
	GrantClientCredentials = "client_credentials"
	GrantRefreshToken      = "refresh_token"
	GrantAuthorizationCode = "authorization_code"
)

// OAuth is an OAuth 2.0 client configuration plus its current tokens.
type OAuth struct {
	Grant        string `json:"grant"`
	TokenURL     string `json:"tokenUrl"`
	AuthURL      string `json:"authUrl,omitempty"`
	ClientID     string `json:"clientId"`
	ClientSecret string `json:"clientSecret,omitempty"`
	Scope        string `json:"scope,omitempty"`
	Audience     string `json:"audience,omitempty"`
	RedirectURL  string `json:"redirectUrl,omitempty"`
	// ClientAuth is "body" (client id and secret in the form) or "basic".
	ClientAuth string `json:"clientAuth,omitempty"`
	// AuthParams are extra query parameters for the consent page, such as
	// access_type=offline.
	AuthParams map[string]string `json:"authParams,omitempty"`

	RefreshToken string    `json:"refreshToken,omitempty"`
	AccessToken  string    `json:"accessToken,omitempty"`
	ExpiresAt    time.Time `json:"expiresAt,omitempty"`
}

// Connected reports whether a token can be obtained without a person.
func (o *OAuth) Connected() bool {
	switch o.Grant {
	case GrantClientCredentials:
		return true
	default:
		return o.RefreshToken != "" || o.AccessToken != ""
	}
}

func (c *Credentials) empty() bool {
	return c == nil || (len(c.Headers) == 0 && len(c.Query) == 0 && c.OAuth == nil && len(c.Piece) == 0)
}

func (c *Credentials) kind() string {
	switch {
	case c.empty():
		return ""
	case len(c.Piece) > 0:
		return "piece"
	case c.OAuth != nil && c.OAuth.Connected():
		return "oauth2"
	case c.OAuth != nil:
		return "oauth2_pending"
	}
	return "headers"
}

type sealedCredentials struct {
	V int `json:"v"`
	Credentials
}

// sealCredentials returns the sealed blob, the header names (which the API may
// show) and the auth kind.
func (s *Store) sealCredentials(tenant, id string, c *Credentials) ([]byte, []string, string, error) {
	names := []string{}
	if c.empty() {
		return nil, names, "", nil
	}
	for k := range c.Headers {
		names = append(names, k)
	}
	sort.Strings(names)
	raw, err := json.Marshal(sealedCredentials{V: 2, Credentials: *c})
	if err != nil {
		return nil, nil, "", err
	}
	sealed, err := s.Box.Seal(raw, aad(tenant, id))
	return sealed, names, c.kind(), err
}

func (s *Store) openCredentials(tenant, id string, sealed []byte) (*Credentials, error) {
	if len(sealed) == 0 {
		return &Credentials{}, nil
	}
	plain, err := s.Box.Open(sealed, aad(tenant, id))
	if err != nil {
		return nil, fmt.Errorf("open credentials: %w", err)
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(plain, &probe); err != nil {
		return nil, err
	}
	if string(probe["v"]) == "2" {
		var out sealedCredentials
		if err := json.Unmarshal(plain, &out); err != nil {
			return nil, err
		}
		return &out.Credentials, nil
	}
	// Rows written before OAuth existed hold a bare map of headers.
	var headers map[string]string
	if err := json.Unmarshal(plain, &headers); err != nil {
		return nil, err
	}
	return &Credentials{Headers: headers}, nil
}

// Credentials opens a catalog's stored credentials. It never returns nil.
func (s *Store) Credentials(ctx context.Context, tenant, id string) (*Credentials, error) {
	var sealed []byte
	err := s.Pool.QueryRow(ctx, `SELECT credentials FROM catalogs WHERE tenant=$1 AND id=$2`, tenant, id).Scan(&sealed)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return s.openCredentials(tenant, id, sealed)
}

// SetCredentials replaces a catalog's credentials.
func (s *Store) SetCredentials(ctx context.Context, tenant, id string, c *Credentials) error {
	return s.UpdateCredentials(ctx, tenant, id, func(cur *Credentials) error {
		*cur = Credentials{}
		if c != nil {
			*cur = *c
		}
		return nil
	})
}

// UpdateCredentials changes a catalog's credentials while holding its row, so
// two workers refreshing the same OAuth token take turns. That matters when the
// provider rotates refresh tokens: the loser of a race would otherwise store a
// token the provider has already retired.
func (s *Store) UpdateCredentials(ctx context.Context, tenant, id string, fn func(*Credentials) error) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	var sealed []byte
	err = tx.QueryRow(ctx, `SELECT credentials FROM catalogs WHERE tenant=$1 AND id=$2 FOR UPDATE`, tenant, id).Scan(&sealed)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	c, err := s.openCredentials(tenant, id, sealed)
	if err != nil {
		return err
	}
	if err := fn(c); err != nil {
		return err
	}
	out, names, kind, err := s.sealCredentials(tenant, id, c)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE catalogs SET credentials=$3, credential_headers=$4, auth_kind=$5 WHERE tenant=$1 AND id=$2`, tenant, id, out, names, kind); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

/* ------------------------------- OAuth consent ------------------------------ */

// SaveOAuthState records a consent in progress.
func (s *Store) SaveOAuthState(ctx context.Context, state, tenant, catalogID, verifier string, ttl time.Duration) error {
	if _, err := s.Pool.Exec(ctx, `DELETE FROM oauth_states WHERE expires_at < now()`); err != nil {
		return err
	}
	_, err := s.Pool.Exec(ctx, `INSERT INTO oauth_states (state,tenant,catalog_id,verifier,expires_at) VALUES ($1,$2,$3,$4,$5)`,
		state, tenant, catalogID, verifier, time.Now().Add(ttl))
	return err
}

// TakeOAuthState returns and deletes a pending consent. A state works once.
func (s *Store) TakeOAuthState(ctx context.Context, state string) (tenant, catalogID, verifier string, err error) {
	err = s.Pool.QueryRow(ctx, `DELETE FROM oauth_states WHERE state=$1 AND expires_at > now() RETURNING tenant,catalog_id,verifier`, state).Scan(&tenant, &catalogID, &verifier)
	if errors.Is(err, pgx.ErrNoRows) {
		err = ErrNotFound
	}
	return
}

/* ----------------------------- resume and items ----------------------------- */

// PriorOutput returns what a node (or one loop item of it) produced in an
// earlier run of the same tenant, if it succeeded there.
func (s *Store) PriorOutput(ctx context.Context, tenant, prior string, ref NodeRef) (map[string]any, bool, error) {
	q := `SELECT n.output FROM node_runs n JOIN runs r ON r.id=n.run_id WHERE r.tenant=$1 AND n.run_id=$2 AND n.key=$3 AND n.status='succeeded'`
	args := []any{tenant, prior, ref.Key}
	if ref.HasItem {
		q = `SELECT n.output FROM node_item_runs n JOIN runs r ON r.id=n.run_id WHERE r.tenant=$1 AND n.run_id=$2 AND n.key=$3 AND n.idx=$4 AND n.status='succeeded'`
		args = append(args, ref.Index)
	}
	var raw []byte
	err := s.Pool.QueryRow(ctx, q, args...).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	out := map[string]any{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &out); err != nil {
			return nil, false, err
		}
	}
	return out, true, nil
}

// CopyPrior carries the rows of the given nodes, and their loop items, from an
// earlier run into this one. It is used for the body of a loop that had already
// finished.
func (s *Store) CopyPrior(ctx context.Context, runID, prior string, keys []string) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(ctx, `UPDATE node_runs n SET status=p.status, output=p.output, error=p.error, started_at=p.started_at, finished_at=p.finished_at, reused=true
		FROM node_runs p WHERE n.run_id=$1 AND p.run_id=$2 AND n.key=p.key AND n.key = ANY($3)`, runID, prior, keys); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO node_item_runs (run_id,key,idx,status,started_at,finished_at,output,error,attempts)
		SELECT $1,key,idx,status,started_at,finished_at,output,error,0 FROM node_item_runs WHERE run_id=$2 AND key = ANY($3)
		ON CONFLICT (run_id,key,idx) DO NOTHING`, runID, prior, keys); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ItemRun is one loop item of one node.
type ItemRun struct {
	Index      int        `json:"index"`
	Status     string     `json:"status"`
	StartedAt  *time.Time `json:"startedAt"`
	FinishedAt *time.Time `json:"finishedAt"`
	Output     any        `json:"output"`
	Error      *string    `json:"error"`
	Attempts   int        `json:"attempts"`
}

// ListItems returns a node's loop items. An empty status means all of them.
func (s *Store) ListItems(ctx context.Context, tenant, runID, key, status string, limit, offset int) ([]ItemRun, error) {
	rows, err := s.Pool.Query(ctx, `SELECT i.idx,i.status,i.started_at,i.finished_at,i.output,i.error,i.attempts FROM node_item_runs i JOIN runs r ON r.id=i.run_id
		WHERE r.tenant=$1 AND i.run_id=$2 AND i.key=$3 AND ($4='' OR i.status=$4) ORDER BY i.idx LIMIT $5 OFFSET $6`, tenant, runID, key, status, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ItemRun{}
	for rows.Next() {
		var (
			it  ItemRun
			raw []byte
		)
		if err := rows.Scan(&it.Index, &it.Status, &it.StartedAt, &it.FinishedAt, &raw, &it.Error, &it.Attempts); err != nil {
			return nil, err
		}
		if raw != nil {
			if err := json.Unmarshal(raw, &it.Output); err != nil {
				return nil, err
			}
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

/* ---------------------------------- API keys -------------------------------- */

// Roles, from least to most able.
const (
	RoleViewer   = "viewer"   // read
	RoleApprover = "approver" // read, decide approvals
	RoleEditor   = "editor"   // read, build and run
	RoleOperator = "operator" // read, build and run, decide approvals
	RoleAdmin    = "admin"    // everything, including tools, keys and the audit log
)

// ValidRole reports whether role is one the engine knows.
func ValidRole(role string) bool {
	switch role {
	case RoleViewer, RoleApprover, RoleEditor, RoleOperator, RoleAdmin:
		return true
	}
	return false
}

// APIKey is a stored key. The key itself is shown once, when it is created.
type APIKey struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Role       string     `json:"role"`
	Prefix     string     `json:"prefix"`
	CreatedBy  string     `json:"createdBy"`
	CreatedAt  time.Time  `json:"createdAt"`
	LastUsedAt *time.Time `json:"lastUsedAt"`
	RevokedAt  *time.Time `json:"revokedAt"`
}

func hashKey(key string) []byte {
	sum := sha256.Sum256([]byte(key))
	return sum[:]
}

// CreateAPIKey makes a key and returns it in the clear, once.
func (s *Store) CreateAPIKey(ctx context.Context, tenant, name, role, createdBy string) (*APIKey, string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return nil, "", err
	}
	key := "apx_" + hex.EncodeToString(b)
	k := &APIKey{ID: NewID("key"), Name: name, Role: role, Prefix: key[:10], CreatedBy: createdBy}
	err := s.Pool.QueryRow(ctx, `INSERT INTO api_keys (id,tenant,name,role,hash,prefix,created_by) VALUES ($1,$2,$3,$4,$5,$6,$7) RETURNING created_at`,
		k.ID, tenant, name, role, hashKey(key), k.Prefix, createdBy).Scan(&k.CreatedAt)
	if isUnique(err) {
		return nil, "", ErrConflict
	}
	return k, key, err
}

// LookupAPIKey resolves a presented key to its tenant, name and role. A revoked
// or unknown key is ErrNotFound.
func (s *Store) LookupAPIKey(ctx context.Context, key string) (tenant, name, role string, err error) {
	var id string
	err = s.Pool.QueryRow(ctx, `SELECT id,tenant,name,role FROM api_keys WHERE hash=$1 AND revoked_at IS NULL`, hashKey(key)).Scan(&id, &tenant, &name, &role)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", "", ErrNotFound
	}
	if err != nil {
		return "", "", "", err
	}
	// Coarse on purpose: one write a minute per key, not one per request.
	_, _ = s.Pool.Exec(ctx, `UPDATE api_keys SET last_used_at=now() WHERE id=$1 AND (last_used_at IS NULL OR last_used_at < now() - interval '1 minute')`, id)
	return tenant, name, role, nil
}

// ListAPIKeys returns a tenant's keys, newest first.
func (s *Store) ListAPIKeys(ctx context.Context, tenant string) ([]*APIKey, error) {
	rows, err := s.Pool.Query(ctx, `SELECT id,name,role,prefix,created_by,created_at,last_used_at,revoked_at FROM api_keys WHERE tenant=$1 ORDER BY created_at DESC`, tenant)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*APIKey{}
	for rows.Next() {
		var k APIKey
		if err := rows.Scan(&k.ID, &k.Name, &k.Role, &k.Prefix, &k.CreatedBy, &k.CreatedAt, &k.LastUsedAt, &k.RevokedAt); err != nil {
			return nil, err
		}
		out = append(out, &k)
	}
	return out, rows.Err()
}

// RevokeAPIKey stops a key working. The row stays, so the audit log still
// resolves the name.
func (s *Store) RevokeAPIKey(ctx context.Context, tenant, id string) (*APIKey, error) {
	var k APIKey
	err := s.Pool.QueryRow(ctx, `UPDATE api_keys SET revoked_at=COALESCE(revoked_at, now()), name=name WHERE tenant=$1 AND id=$2
		RETURNING id,name,role,prefix,created_by,created_at,last_used_at,revoked_at`, tenant, id).
		Scan(&k.ID, &k.Name, &k.Role, &k.Prefix, &k.CreatedBy, &k.CreatedAt, &k.LastUsedAt, &k.RevokedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &k, err
}

/* ---------------------------------- audit log ------------------------------- */

// AuditEntry is one recorded action.
type AuditEntry struct {
	ID     int64          `json:"id"`
	At     time.Time      `json:"at"`
	Actor  string         `json:"actor"`
	Action string         `json:"action"`
	Target string         `json:"target"`
	Detail map[string]any `json:"detail"`
}

// Audit appends to the tenant's audit log.
func (s *Store) Audit(ctx context.Context, tenant, actor, action, target string, detail map[string]any) error {
	if detail == nil {
		detail = map[string]any{}
	}
	raw, err := json.Marshal(detail)
	if err != nil {
		return err
	}
	_, err = s.Pool.Exec(ctx, `INSERT INTO audit_log (tenant,actor,action,target,detail) VALUES ($1,$2,$3,$4,$5)`, tenant, actor, action, target, raw)
	return err
}

// ListAudit returns entries newest first. before, when positive, pages back
// from that id. Empty action, actor or target mean no filter; action matches as
// a prefix, so "run." selects every run action.
func (s *Store) ListAudit(ctx context.Context, tenant string, limit int, before int64, action, actor, target string) ([]AuditEntry, error) {
	rows, err := s.Pool.Query(ctx, `SELECT id,at,actor,action,target,detail FROM audit_log
		WHERE tenant=$1 AND ($2=0 OR id<$2) AND ($3='' OR action LIKE $3 || '%') AND ($4='' OR actor=$4) AND ($5='' OR target=$5)
		ORDER BY id DESC LIMIT $6`, tenant, before, strings.ReplaceAll(strings.ReplaceAll(action, "%", ""), "_", `\_`), actor, target, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AuditEntry{}
	for rows.Next() {
		var (
			e   AuditEntry
			raw []byte
		)
		if err := rows.Scan(&e.ID, &e.At, &e.Actor, &e.Action, &e.Target, &raw); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(raw, &e.Detail); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

/* ---------------------------------- counters -------------------------------- */

const bumpSQL = `INSERT INTO counters (name,tenant,label,value) VALUES ($1,$2,$3,$4)
	ON CONFLICT (name,tenant,label) DO UPDATE SET value = counters.value + EXCLUDED.value`

// Bump adds n to a counter.
func (s *Store) Bump(ctx context.Context, name, tenant, label string, n int64) error {
	_, err := s.Pool.Exec(ctx, bumpSQL, name, tenant, label, n)
	return err
}

// Counter is one monotonic count.
type Counter struct {
	Name, Tenant, Label string
	Value               int64
}

// Counters returns every counter, in a stable order.
func (s *Store) Counters(ctx context.Context) ([]Counter, error) {
	rows, err := s.Pool.Query(ctx, `SELECT name,tenant,label,value FROM counters ORDER BY name,tenant,label`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Counter
	for rows.Next() {
		var c Counter
		if err := rows.Scan(&c.Name, &c.Tenant, &c.Label, &c.Value); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// Gauge is a current value, read at scrape time.
type Gauge struct {
	Tenant, Label string
	Value         float64
}

// OpenRuns counts unfinished runs by tenant and status.
func (s *Store) OpenRuns(ctx context.Context) ([]Gauge, error) {
	return s.gauges(ctx, `SELECT tenant, status, count(*)::float8 FROM runs WHERE finished_at IS NULL GROUP BY tenant, status ORDER BY tenant, status`)
}

// OldestWaiting returns, per tenant, how long the oldest run awaiting approval
// has been open, in seconds.
func (s *Store) OldestWaiting(ctx context.Context) ([]Gauge, error) {
	return s.gauges(ctx, `SELECT tenant, '', extract(epoch FROM now() - min(created_at))::float8 FROM runs WHERE finished_at IS NULL AND status='waiting_approval' GROUP BY tenant ORDER BY tenant`)
}

func (s *Store) gauges(ctx context.Context, q string) ([]Gauge, error) {
	rows, err := s.Pool.Query(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Gauge
	for rows.Next() {
		var g Gauge
		if err := rows.Scan(&g.Tenant, &g.Label, &g.Value); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

/* ---------------------------------- previews -------------------------------- */

// PlannedAction is one thing a preview run would have done.
type PlannedAction struct {
	Node string `json:"node"`
	Name string `json:"name"`
	Type string `json:"type"`
	// Item is the loop item the action is for; nil outside a loop.
	Item *int `json:"item"`
	// Kind is call, approval, wait or unknown.
	Kind   string         `json:"kind"`
	Detail map[string]any `json:"detail"`
}

// PlannedActions lists what a preview run recorded in place of doing, in graph
// order and then item order.
func (s *Store) PlannedActions(ctx context.Context, tenant, runID string, limit int) ([]PlannedAction, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT n.position, n.key, n.name, n.type, NULL::int AS idx, n.output->'planned'
		  FROM node_runs n JOIN runs r ON r.id=n.run_id
		 WHERE r.tenant=$1 AND n.run_id=$2 AND n.output->>'preview'='true'
		   AND NOT EXISTS (SELECT 1 FROM node_item_runs i WHERE i.run_id=n.run_id AND i.key=n.key)
		UNION ALL
		SELECT n.position, n.key, n.name, n.type, i.idx, i.output->'planned'
		  FROM node_item_runs i JOIN node_runs n ON n.run_id=i.run_id AND n.key=i.key JOIN runs r ON r.id=i.run_id
		 WHERE r.tenant=$1 AND i.run_id=$2 AND i.output->>'preview'='true'
		 ORDER BY 1, 5 NULLS FIRST LIMIT $3`, tenant, runID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PlannedAction{}
	for rows.Next() {
		var (
			a   PlannedAction
			pos int
			raw []byte
		)
		if err := rows.Scan(&pos, &a.Node, &a.Name, &a.Type, &a.Item, &raw); err != nil {
			return nil, err
		}
		a.Detail = map[string]any{}
		if raw != nil {
			if err := json.Unmarshal(raw, &a.Detail); err != nil {
				return nil, err
			}
		}
		a.Kind, _ = a.Detail["kind"].(string)
		delete(a.Detail, "kind")
		out = append(out, a)
	}
	return out, rows.Err()
}

/* ------------------------- limits, retention, offboarding ------------------- */

// OpenRunCount is how many of a tenant's real runs have not finished.
func (s *Store) OpenRunCount(ctx context.Context, tenant string) (int, error) {
	var n int
	err := s.Pool.QueryRow(ctx, `SELECT count(*) FROM runs WHERE tenant=$1 AND finished_at IS NULL`, tenant).Scan(&n)
	return n, err
}

// PurgeRuns deletes finished runs older than the cutoff, with their steps and
// loop items, a batch at a time so a large backlog never holds a long lock. It
// returns how many runs went.
func (s *Store) PurgeRuns(ctx context.Context, olderThan time.Time) (int64, error) {
	var total int64
	for {
		tag, err := s.Pool.Exec(ctx, `DELETE FROM runs WHERE id IN (
			SELECT id FROM runs WHERE finished_at IS NOT NULL AND finished_at < $1 ORDER BY finished_at LIMIT 500)`, olderThan)
		if err != nil {
			return total, err
		}
		total += tag.RowsAffected()
		if tag.RowsAffected() < 500 {
			return total, nil
		}
	}
}

// PurgeAudit deletes audit entries older than the cutoff.
func (s *Store) PurgeAudit(ctx context.Context, olderThan time.Time) (int64, error) {
	tag, err := s.Pool.Exec(ctx, `DELETE FROM audit_log WHERE at < $1`, olderThan)
	return tag.RowsAffected(), err
}

// OpenRun names an unfinished run and the Temporal workflow behind it.
type OpenRun struct{ ID, WorkflowID string }

// OpenRunsOf lists a tenant's unfinished runs.
func (s *Store) OpenRunsOf(ctx context.Context, tenant string) ([]OpenRun, error) {
	rows, err := s.Pool.Query(ctx, `SELECT id, workflow_id FROM runs WHERE tenant=$1 AND finished_at IS NULL`, tenant)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OpenRun
	for rows.Next() {
		var r OpenRun
		if err := rows.Scan(&r.ID, &r.WorkflowID); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// DeleteTenant removes everything a tenant has: tools and their credentials,
// workflows, runs, schedules, keys, counters and the audit log. It returns how
// many rows went from each table.
func (s *Store) DeleteTenant(ctx context.Context, tenant string) (map[string]int64, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	out := map[string]int64{}
	// oauth_states goes with catalogs, steps and loop items with runs.
	for _, table := range []string{"runs", "schedules", "workflows", "catalogs", "api_keys", "counters", "audit_log"} {
		tag, err := tx.Exec(ctx, `DELETE FROM `+table+` WHERE tenant=$1`, tenant)
		if err != nil {
			return nil, err
		}
		out[table] = tag.RowsAffected()
	}
	return out, tx.Commit(ctx)
}
