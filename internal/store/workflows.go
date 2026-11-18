package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/satyanvm/apex-engine/internal/compiler"
)

// Workflow is a saved graph. The webhook path is only present while the webhook
// is on, and only the owning tenant can read it.
type Workflow struct {
	ID           string               `json:"id"`
	Tenant       string               `json:"-"`
	Name         string               `json:"name"`
	Description  string               `json:"description"`
	Prompt       string               `json:"prompt"`
	Nodes        []compiler.GraphNode `json:"nodes,omitempty"`
	DefaultInput map[string]any       `json:"defaultInput"`
	CatalogIDs   []string             `json:"catalogIds"`
	CallbackURL  string               `json:"callbackUrl"`
	WebhookPath  *string              `json:"webhookPath"`
	CreatedAt    time.Time            `json:"createdAt"`
	UpdatedAt    time.Time            `json:"updatedAt"`
	LastRun      *WorkflowLastRun     `json:"lastRun,omitempty"`
}

// WorkflowLastRun summarizes a workflow's most recent run for the list view.
type WorkflowLastRun struct {
	ID        string    `json:"id"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"createdAt"`
}

const workflowCols = `id,tenant,name,description,prompt,nodes,default_input,catalog_ids,callback_url,webhook_token,created_at,updated_at`

func scanWorkflow(row pgx.Row) (*Workflow, error) {
	var (
		w            Workflow
		nodes, input []byte
		token        *string
	)
	err := row.Scan(&w.ID, &w.Tenant, &w.Name, &w.Description, &w.Prompt, &nodes, &input, &w.CatalogIDs, &w.CallbackURL, &token, &w.CreatedAt, &w.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(nodes, &w.Nodes); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(input, &w.DefaultInput); err != nil {
		return nil, err
	}
	if token != nil {
		path := "/hooks/" + *token
		w.WebhookPath = &path
	}
	return &w, nil
}

func marshalGraph(w *Workflow) (nodes, input []byte, err error) {
	if nodes, err = json.Marshal(w.Nodes); err != nil {
		return nil, nil, err
	}
	if w.DefaultInput == nil {
		w.DefaultInput = map[string]any{}
	}
	input, err = json.Marshal(w.DefaultInput)
	return nodes, input, err
}

// CreateWorkflow inserts a workflow and fills its timestamps.
func (s *Store) CreateWorkflow(ctx context.Context, w *Workflow) error {
	nodes, input, err := marshalGraph(w)
	if err != nil {
		return err
	}
	if w.CatalogIDs == nil {
		w.CatalogIDs = []string{}
	}
	return s.Pool.QueryRow(ctx, `INSERT INTO workflows (id,tenant,name,description,prompt,nodes,default_input,catalog_ids,callback_url)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9) RETURNING created_at,updated_at`,
		w.ID, w.Tenant, w.Name, w.Description, w.Prompt, nodes, input, w.CatalogIDs, w.CallbackURL).Scan(&w.CreatedAt, &w.UpdatedAt)
}

// UpdateWorkflow replaces a workflow's editable fields.
func (s *Store) UpdateWorkflow(ctx context.Context, w *Workflow) error {
	nodes, input, err := marshalGraph(w)
	if err != nil {
		return err
	}
	if w.CatalogIDs == nil {
		w.CatalogIDs = []string{}
	}
	tag, err := s.Pool.Exec(ctx, `UPDATE workflows SET name=$3, description=$4, prompt=$5, nodes=$6, default_input=$7, catalog_ids=$8, callback_url=$9, updated_at=now()
		WHERE tenant=$1 AND id=$2`, w.Tenant, w.ID, w.Name, w.Description, w.Prompt, nodes, input, w.CatalogIDs, w.CallbackURL)
	if err == nil && tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return err
}

// GetWorkflow returns one workflow of a tenant.
func (s *Store) GetWorkflow(ctx context.Context, tenant, id string) (*Workflow, error) {
	return scanWorkflow(s.Pool.QueryRow(ctx, `SELECT `+workflowCols+` FROM workflows WHERE tenant=$1 AND id=$2`, tenant, id))
}

// WorkflowByWebhook finds the workflow a webhook token belongs to.
func (s *Store) WorkflowByWebhook(ctx context.Context, token string) (*Workflow, error) {
	return scanWorkflow(s.Pool.QueryRow(ctx, `SELECT `+workflowCols+` FROM workflows WHERE webhook_token=$1`, token))
}

// ListWorkflows returns a tenant's workflows with their latest run.
func (s *Store) ListWorkflows(ctx context.Context, tenant string) ([]*Workflow, error) {
	rows, err := s.Pool.Query(ctx, `SELECT `+workflowCols+` FROM workflows WHERE tenant=$1 ORDER BY updated_at DESC LIMIT 200`, tenant)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Workflow{}
	for rows.Next() {
		w, err := scanWorkflow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	for _, w := range out {
		var last WorkflowLastRun
		err := s.Pool.QueryRow(ctx, `SELECT id,status,created_at FROM runs WHERE tenant=$1 AND workflow_ref=$2 ORDER BY created_at DESC LIMIT 1`, tenant, w.ID).
			Scan(&last.ID, &last.Status, &last.CreatedAt)
		if err == nil {
			w.LastRun = &last
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return nil, err
		}
	}
	return out, nil
}

// DeleteWorkflow removes a workflow. Its runs stay, as history.
func (s *Store) DeleteWorkflow(ctx context.Context, tenant, id string) error {
	tag, err := s.Pool.Exec(ctx, `DELETE FROM workflows WHERE tenant=$1 AND id=$2`, tenant, id)
	if err == nil && tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return err
}

// SetWebhookToken turns a workflow's webhook on, replacing any old token, or off
// when token is nil.
func (s *Store) SetWebhookToken(ctx context.Context, tenant, id string, token *string) error {
	tag, err := s.Pool.Exec(ctx, `UPDATE workflows SET webhook_token=$3 WHERE tenant=$1 AND id=$2`, tenant, id, token)
	if err == nil && tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return err
}

// WorkflowSchedules lists the schedules attached to a workflow.
func (s *Store) WorkflowSchedules(ctx context.Context, tenant, id string) ([]*Schedule, error) {
	all, err := s.ListSchedules(ctx, tenant)
	if err != nil {
		return nil, err
	}
	out := []*Schedule{}
	for _, sc := range all {
		if sc.WorkflowID != nil && *sc.WorkflowID == id {
			out = append(out, sc)
		}
	}
	return out, nil
}
