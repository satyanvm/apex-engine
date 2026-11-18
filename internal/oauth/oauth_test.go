package oauth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/satyanvm/apex-engine/internal/store"
)

func tokenServer(t *testing.T, h func(form url.Values, r *http.Request) (int, string)) *httptest.Server {
	t.Helper()
	t.Setenv("APEX_ALLOW_PRIVATE_NETWORK", "1")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		code, body := h(r.PostForm, r)
		w.WriteHeader(code)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestClientCredentials(t *testing.T) {
	srv := tokenServer(t, func(f url.Values, _ *http.Request) (int, string) {
		if f.Get("grant_type") != "client_credentials" || f.Get("client_id") != "id" || f.Get("client_secret") != "sec" || f.Get("scope") != "read" || f.Get("audience") != "api" {
			return 400, `{"error":"invalid_request"}`
		}
		return 200, `{"access_token":"tok","expires_in":3600,"token_type":"Bearer"}`
	})
	o := &store.OAuth{Grant: store.GrantClientCredentials, TokenURL: srv.URL, ClientID: "id", ClientSecret: "sec", Scope: "read", Audience: "api"}
	if err := Fetch(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	if o.AccessToken != "tok" || time.Until(o.ExpiresAt) < 50*time.Minute || !fresh(o) {
		t.Errorf("o = %+v", o)
	}
}

func TestRefreshKeepsARotatedRefreshToken(t *testing.T) {
	srv := tokenServer(t, func(f url.Values, _ *http.Request) (int, string) {
		if f.Get("grant_type") != "refresh_token" || f.Get("refresh_token") != "r1" {
			return 400, `{"error":"invalid_grant"}`
		}
		return 200, `{"access_token":"a2","refresh_token":"r2","expires_in":"60"}`
	})
	o := &store.OAuth{Grant: store.GrantRefreshToken, TokenURL: srv.URL, ClientID: "id", RefreshToken: "r1"}
	if err := Fetch(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	if o.AccessToken != "a2" || o.RefreshToken != "r2" {
		t.Errorf("o = %+v", o)
	}
	if fresh(o) {
		t.Error("a token with a minute left should be treated as expired")
	}
}

func TestARefusedGrantIsPermanentAndAnOutageIsNot(t *testing.T) {
	refused := tokenServer(t, func(url.Values, *http.Request) (int, string) {
		return 400, `{"error":"invalid_grant","error_description":"Token has been revoked"}`
	})
	err := Fetch(context.Background(), &store.OAuth{Grant: store.GrantRefreshToken, TokenURL: refused.URL, ClientID: "id", RefreshToken: "r"})
	oe, ok := err.(*Error)
	if !ok || !oe.Permanent || !strings.Contains(oe.Msg, "revoked") {
		t.Errorf("err = %v", err)
	}
	down := tokenServer(t, func(url.Values, *http.Request) (int, string) { return 503, `busy` })
	err = Fetch(context.Background(), &store.OAuth{Grant: store.GrantClientCredentials, TokenURL: down.URL, ClientID: "id"})
	if oe, ok := err.(*Error); !ok || oe.Permanent {
		t.Errorf("an outage should be retried, got %v", err)
	}
	if err := Fetch(context.Background(), &store.OAuth{Grant: store.GrantAuthorizationCode, TokenURL: down.URL, ClientID: "id"}); err == nil {
		t.Error("with no refresh token there is nothing to refresh with")
	}
}

func TestFallsBackToBasicClientAuthentication(t *testing.T) {
	srv := tokenServer(t, func(f url.Values, r *http.Request) (int, string) {
		if user, pass, ok := r.BasicAuth(); ok && user == "id" && pass == "sec" && f.Get("client_secret") == "" {
			return 200, `{"access_token":"tok"}`
		}
		return 401, `{"error":"invalid_client"}`
	})
	o := &store.OAuth{Grant: store.GrantClientCredentials, TokenURL: srv.URL, ClientID: "id", ClientSecret: "sec"}
	if err := Fetch(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	if o.ClientAuth != "basic" || o.AccessToken != "tok" {
		t.Errorf("o = %+v", o)
	}
	if !fresh(o) {
		t.Error("a token with no stated lifetime is used until the API refuses it")
	}
}

func TestFormEncodedTokenResponse(t *testing.T) {
	srv := tokenServer(t, func(url.Values, *http.Request) (int, string) {
		return 200, `access_token=gho_x&scope=repo&token_type=bearer`
	})
	o := &store.OAuth{TokenURL: srv.URL, ClientID: "id", RedirectURL: "https://e/oauth/callback"}
	if err := ExchangeCode(context.Background(), o, "code", "verifier"); err != nil || o.AccessToken != "gho_x" {
		t.Errorf("o=%+v err=%v", o, err)
	}
}

func TestAuthorizeURL(t *testing.T) {
	verifier, challenge := NewVerifier()
	if len(verifier) < 43 || challenge == verifier {
		t.Fatalf("verifier=%q challenge=%q", verifier, challenge)
	}
	o := &store.OAuth{AuthURL: "https://idp.example/authorize?tenant=x", ClientID: "id", RedirectURL: "https://e/oauth/callback", Scope: "a b",
		AuthParams: map[string]string{"access_type": "offline"}}
	raw, err := AuthorizeURL(o, "st", challenge)
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(raw)
	q := u.Query()
	for k, want := range map[string]string{"tenant": "x", "response_type": "code", "client_id": "id", "redirect_uri": "https://e/oauth/callback",
		"state": "st", "code_challenge": challenge, "code_challenge_method": "S256", "scope": "a b", "access_type": "offline"} {
		if q.Get(k) != want {
			t.Errorf("%s = %q, want %q", k, q.Get(k), want)
		}
	}
	if _, err := AuthorizeURL(&store.OAuth{AuthURL: "javascript:alert(1)"}, "s", "c"); err == nil {
		t.Error("a non-http authUrl must be refused")
	}
}
