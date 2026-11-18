package httpapi

import (
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"gopkg.in/yaml.v3"
)

// The spec is hand-written, so this keeps it honest: every route the server
// answers must be documented, and nothing documented may be missing.
func TestOpenAPIDocumentsEveryRoute(t *testing.T) {
	var spec struct {
		Paths map[string]map[string]any `yaml:"paths"`
	}
	if err := yaml.Unmarshal(openAPISpec, &spec); err != nil {
		t.Fatal(err)
	}
	documented := map[string]bool{}
	for path, ops := range spec.Paths {
		for method := range ops {
			if method != "parameters" {
				documented[strings.ToUpper(method)+" "+path] = true
			}
		}
	}
	handler := (&Server{Log: slog.Default()}).Handler(10)
	served := map[string]bool{}
	err := chi.Walk(handler.(chi.Routes), func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		served[method+" "+strings.TrimSuffix(route, "/")] = true
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for route := range served {
		if !documented[route] {
			t.Errorf("served but not documented: %s", route)
		}
	}
	for route := range documented {
		if !served[route] {
			t.Errorf("documented but not served: %s", route)
		}
	}
}

func TestConsoleIsOffUnlessAskedFor(t *testing.T) {
	handler := (&Server{Log: slog.Default()}).Handler(10)
	err := chi.Walk(handler.(chi.Routes), func(_, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		if route == "/*" {
			t.Errorf("the console route is mounted by default")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
