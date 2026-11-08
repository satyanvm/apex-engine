package netguard

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"
)

func TestBlocked(t *testing.T) {
	for addr, want := range map[string]bool{
		"127.0.0.1": true, "10.1.2.3": true, "192.168.0.1": true, "172.16.0.9": true,
		"169.254.169.254": true, "100.64.0.1": true, "0.0.0.0": true, "::1": true,
		"fd00::1": true, "fe80::1": true, "::ffff:127.0.0.1": true,
		"8.8.8.8": false, "1.1.1.1": false, "2606:4700:4700::1111": false,
	} {
		if got := Blocked(netip.MustParseAddr(addr)); got != want {
			t.Errorf("Blocked(%s) = %v, want %v", addr, got, want)
		}
	}
}

func TestClientRefusesLoopback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("the request reached the server")
	}))
	defer srv.Close()
	req, _ := http.NewRequestWithContext(context.Background(), "GET", srv.URL, nil)
	_, err := Client(2*time.Second, true).Do(req)
	if err == nil || !strings.Contains(err.Error(), "private address") {
		t.Fatalf("want a private-address refusal, got %v", err)
	}
}

func TestNoRedirectClientKeepsFirstResponse(t *testing.T) {
	t.Setenv("APEX_ALLOW_PRIVATE_NETWORK", "1")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/elsewhere", http.StatusFound)
	}))
	defer srv.Close()
	resp, err := Client(2*time.Second, false).Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("status = %d, want 302", resp.StatusCode)
	}
}
