package catalog

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/satyanvm/apex-engine/internal/compiler"
)

// PieceProp is one input of a piece action, or one field of its connection.
type PieceProp struct {
	Name        string `json:"name"`
	DisplayName string `json:"displayName"`
	Description string `json:"description"`
	Type        string `json:"type"`
	Required    bool   `json:"required"`
	Options     []struct {
		Label string `json:"label"`
		Value any    `json:"value"`
	} `json:"options"`
}

// PieceAuth describes the connection a piece needs.
type PieceAuth struct {
	Type        string      `json:"type"` // NONE, SECRET_TEXT, CUSTOM_AUTH, BASIC_AUTH or OAUTH2
	Description string      `json:"description"`
	Props       []PieceProp `json:"props"`
	AuthURL     string      `json:"authUrl"`
	TokenURL    string      `json:"tokenUrl"`
	Scope       []string    `json:"scope"`
}

// PieceAction is one thing a piece can do.
type PieceAction struct {
	Name        string      `json:"name"`
	DisplayName string      `json:"displayName"`
	Description string      `json:"description"`
	Props       []PieceProp `json:"props"`
}

// PieceMeta is an Activepieces piece as the pieces runner describes it.
type PieceMeta struct {
	Name        string        `json:"name"`
	Package     string        `json:"package"`
	Version     string        `json:"version"`
	DisplayName string        `json:"displayName"`
	Description string        `json:"description"`
	Categories  []string      `json:"categories"`
	Auth        PieceAuth     `json:"auth"`
	Actions     []PieceAction `json:"actions"`
	ActionCount int           `json:"actionCount"`
}

func piecesGet(ctx context.Context, base, token, path string, into any) error {
	ctx, cancel := context.WithTimeout(ctx, 40*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(base, "/")+path, nil)
	if err != nil {
		return err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	// The runner is the operator's own service on a private address, so this
	// is not a user-supplied URL and does not go through netguard.
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("pieces runner unreachable: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(raw, &e) == nil && e.Error != "" {
			return fmt.Errorf("%s", e.Error)
		}
		return fmt.Errorf("pieces runner answered HTTP %d", resp.StatusCode)
	}
	return json.Unmarshal(raw, into)
}

// ListPieces returns the pieces the runner has installed, without their actions.
func ListPieces(ctx context.Context, base, token string) ([]PieceMeta, error) {
	var out struct {
		Pieces []PieceMeta `json:"pieces"`
	}
	err := piecesGet(ctx, base, token, "/pieces", &out)
	return out.Pieces, err
}

// FetchPiece returns one piece with its actions.
func FetchPiece(ctx context.Context, base, token, name string) (*PieceMeta, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, fmt.Errorf("source.piece is required, for example \"slack\"")
	}
	var meta PieceMeta
	if err := piecesGet(ctx, base, token, "/pieces/"+url.PathEscape(name), &meta); err != nil {
		return nil, err
	}
	return &meta, nil
}

// pieceArgType maps an Activepieces property type to the type the engine
// coerces a text config value into before calling the action.
func pieceArgType(t string) string {
	switch t {
	case "NUMBER":
		return "number"
	case "CHECKBOX":
		return "boolean"
	case "ARRAY", "MULTI_SELECT_DROPDOWN", "STATIC_MULTI_SELECT_DROPDOWN":
		return "array"
	case "OBJECT", "JSON", "DYNAMIC":
		return "object"
	}
	return "string"
}

// PieceNodes turns a piece's actions into planner nodes.
func PieceNodes(slug string, meta *PieceMeta) []compiler.CatalogNode {
	nodes := make([]compiler.CatalogNode, 0, len(meta.Actions))
	for _, act := range meta.Actions {
		// Every piece carries a raw "custom API call" action. Its inputs are
		// built by the Activepieces editor, and the engine already has tool.http
		// and OpenAPI import for raw calls, so it is left out.
		if act.Name == "custom_api_call" {
			continue
		}
		types := map[string]any{}
		var fields []compiler.CatalogField
		for _, p := range act.Props {
			switch p.Type {
			case "MARKDOWN", "OAUTH2", "SECRET_TEXT", "BASIC_AUTH", "CUSTOM_AUTH":
				continue // display text, or the connection, which is stored separately
			}
			kind := pieceArgType(p.Type)
			types[p.Name] = kind
			help := strings.TrimSpace(p.Description)
			var options []string
			for _, o := range p.Options {
				options = append(options, fmt.Sprint(o.Value))
			}
			switch {
			case len(options) > 0 && len(options) <= 40:
				// offered as a select below
			case p.Type == "DROPDOWN" || p.Type == "MULTI_SELECT_DROPDOWN":
				help = strings.TrimSpace(help + " Takes the id of the chosen " + strings.ToLower(p.DisplayName) + ", not its name.")
				options = nil
			default:
				options = nil
			}
			if kind != "string" {
				help = strings.TrimSpace(help + " (" + kind + ")")
			}
			control := "text"
			if kind == "object" || kind == "array" {
				control = "json"
			} else if len(options) > 0 {
				control = "select"
			}
			label := p.DisplayName
			if label == "" {
				label = p.Name
			}
			fields = append(fields, compiler.CatalogField{Key: p.Name, Label: label, Control: control, Required: p.Required, Help: clip(help, 300), Options: options})
		}
		label := act.DisplayName
		if label == "" {
			label = act.Name
		}
		desc := strings.TrimSpace(act.Description)
		if desc == "" {
			desc = label
		}
		nodes = append(nodes, compiler.CatalogNode{
			ID: "piece." + slug + "." + Slug(act.Name), Label: clip(label, 80), Description: clip(meta.DisplayName+": "+desc, maxDescLen),
			Kind: "action", Runtime: "piece", Config: fields, Outputs: []string{"result"},
			Exec: map[string]any{"piece": meta.Name, "action": act.Name, "version": meta.Version, "types": types},
		})
	}
	return nodes
}
