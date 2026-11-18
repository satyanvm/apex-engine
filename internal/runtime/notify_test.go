package runtime

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/satyanvm/apex-engine/internal/compiler"
)

func TestNotifyApprovalPostsSlackStyleMessageWithLink(t *testing.T) {
	var got map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &got)
	}))
	defer srv.Close()
	a := &Activities{NotifyURL: srv.URL, PublicURL: "https://apex.example.com/"}
	in := NodeInput{RunID: "run_1", Node: compiler.GraphNode{Key: "k", Name: "Ask a human"}}
	a.notifyApproval(context.Background(), in, strings.Repeat("x", 900))
	text := got["text"]
	if !strings.Contains(text, "Ask a human") || !strings.Contains(text, "https://apex.example.com/#/runs/run_1") {
		t.Errorf("text = %q", text)
	}
	if len(text) > 620 {
		t.Errorf("message was not capped: %d bytes", len(text))
	}
}

func TestNotifyApprovalIsOffByDefault(t *testing.T) {
	(&Activities{}).notifyApproval(context.Background(), NodeInput{}, "m") // must not panic or dial
}
