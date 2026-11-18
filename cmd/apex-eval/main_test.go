package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/satyanvm/apex-engine/client"
)

func get(t *testing.T, srv *httptest.Server, path string) []map[string]any {
	t.Helper()
	resp, err := http.Get(srv.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out []map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestMockBackendPagesFiltersAndRecordsWrites(t *testing.T) {
	rows := []map[string]any{{"id": "a", "status": "late"}, {"id": "b", "status": "ok"}, {"id": "c", "status": "late"}, {"id": "d", "status": "late"}}
	m := &mockBackend{data: map[string]map[string][]map[string]any{"shop": {"orders": rows}}}
	srv := httptest.NewServer(m)
	defer srv.Close()
	if got := get(t, srv, "/shop/orders"); len(got) != 2 {
		t.Errorf("default page size should be 2, got %d", len(got))
	}
	if got := get(t, srv, "/shop/orders?status=late&page=2"); len(got) != 1 || got[0]["id"] != "d" {
		t.Errorf("filter then page: %v", got)
	}
	if got := get(t, srv, "/shop/orders?per_page=100&unknown=x"); len(got) != 4 {
		t.Errorf("an unknown parameter must not filter: %v", got)
	}
	if got := get(t, srv, "/shop/orders?page=9"); len(got) != 0 {
		t.Errorf("past the end: %v", got)
	}
	for _, path := range []string{"/shop/orders/a/refund", "/shop/orders/a/refund"} {
		resp, err := http.Post(srv.URL+path, "application/json", strings.NewReader(`{}`))
		if err != nil || resp.StatusCode != 200 {
			t.Fatalf("post: %v %v", resp, err)
		}
		resp.Body.Close()
	}
	if got := m.writes(); len(got) != 2 || got[0] != "POST /shop/orders/a/refund" {
		t.Errorf("writes = %v", got)
	}
	m.reset()
	if len(m.writes()) != 0 {
		t.Error("reset did not clear the log")
	}
}

func TestDiffCountsRepeats(t *testing.T) {
	missing, unexpected := diff([]string{"POST /a", "POST /a", "POST /b"}, []string{"POST /a", "POST /c"})
	if strings.Join(missing, ",") != "POST /a,POST /b" || strings.Join(unexpected, ",") != "POST /c" {
		t.Errorf("missing=%v unexpected=%v", missing, unexpected)
	}
}

func TestStructureChecks(t *testing.T) {
	var ec evalCase
	_ = json.Unmarshal([]byte(`{"expect":{"uses":["refundOrder"],"notUses":["cancelOrder"],"kinds":["logic.loop"],"approvalBefore":["refundOrder"],"maxNodes":5}}`), &ec)
	n := func(key, typ string, deps ...string) client.Node {
		return client.Node{Key: key, Type: typ, DependsOn: deps}
	}
	good := []client.Node{n("t", "trigger.manual"), n("list", "api.eval_shop.listOrders", "t"), n("each", "logic.loop", "list"),
		n("ok", "logic.approval", "each"), n("refund", "api.eval_shop.refundOrder", "ok")}
	if got := checkStructure(ec, good); len(got) != 0 {
		t.Errorf("good graph: %v", got)
	}
	bad := []client.Node{n("t", "trigger.manual"), n("list", "api.eval_shop.listOrders", "t"), n("ok", "logic.approval", "list"),
		n("refund", "api.eval_shop.refundOrder", "list"), n("cancel", "api.eval_shop.cancelOrder", "ok"), n("x", "agent.llm", "t")}
	got := strings.Join(checkStructure(ec, bad), "; ")
	for _, want := range []string{"uses cancelOrder", "no logic.loop", "more than 5", "without an approval"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %q", want, got)
		}
	}
}
