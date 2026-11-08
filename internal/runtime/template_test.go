package runtime

import (
	"reflect"
	"strings"
	"testing"
)

func scope() Scope {
	return Scope{
		Input: map[string]any{"topic": "ai", "n": float64(3)},
		Outputs: map[string]map[string]any{
			"fetch": {"status": float64(200), "body": map[string]any{"items": []any{"a", "b"}, "ok": true}, "raw": `{"id":7}`},
		},
	}
}

func TestRender(t *testing.T) {
	s := scope()
	got, err := s.Render(map[string]any{
		"text": "about {{input.topic}} ({{input.n}})",
		"list": "{{outputs.fetch.body.items}}",
		"deep": []any{"{{outputs.fetch.body.items.1}}"},
		"json": "{{json outputs.fetch.body.items}}",
		"url":  "q={{urlencode input.topic}}&r={{outputs.fetch.raw.id}}",
		"num":  float64(1),
	})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"text": "about ai (3)",
		"list": []any{"a", "b"}, // a lone reference keeps its type
		"deep": []any{"b"},
		"json": `["a","b"]`,
		"url":  "q=ai&r=7",
		"num":  float64(1),
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got  %#v\nwant %#v", got, want)
	}
}

func TestRenderMissingReferenceFails(t *testing.T) {
	for _, tpl := range []string{"{{input.nope}}", "x {{outputs.other.body}}", "{{outputs.fetch.body.items.9}}"} {
		if _, err := scope().Render(tpl); err == nil || !strings.Contains(err.Error(), "reference not found") {
			t.Errorf("%s: err = %v", tpl, err)
		}
	}
}

func TestEvalCondition(t *testing.T) {
	s := scope()
	for expr, want := range map[string]bool{
		"outputs.fetch.status == 200":  true,
		"outputs.fetch.status != 200":  false,
		"outputs.fetch.status >= 500":  false,
		"input.n < 10":                 true,
		`input.topic == "ai"`:          true,
		"input.topic == 'ml'":          false,
		"outputs.fetch.body.ok":        true,
		"{{outputs.fetch.status}} > 1": false, // braces are stripped; the bare ref side still resolves
	} {
		if expr == "{{outputs.fetch.status}} > 1" {
			want = true
		}
		got, err := s.EvalCondition(expr)
		if err != nil {
			t.Errorf("%s: %v", expr, err)
		} else if got != want {
			t.Errorf("%s = %v, want %v", expr, got, want)
		}
	}
	for _, bad := range []string{"", "input.missing == 1", "outputs.fetch.body > 3"} {
		if _, err := s.EvalCondition(bad); err == nil {
			t.Errorf("%q should fail", bad)
		}
	}
}
