// Package oauth obtains and refreshes OAuth 2.0 tokens for tool sources.
package oauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/satyanvm/apex-engine/internal/netguard"
	"github.com/satyanvm/apex-engine/internal/store"
)

// Error is a token endpoint failure. Permanent means asking again will not
// help: the grant was refused and a person has to reconnect.
type Error struct {
	Permanent bool
	Msg       string
}

func (e *Error) Error() string { return e.Msg }

func permanent(format string, args ...any) error {
	return &Error{Permanent: true, Msg: fmt.Sprintf(format, args...)}
}

// skew is how long before expiry a token is treated as expired.
const skew = time.Minute

func fresh(o *store.OAuth) bool {
	return o.AccessToken != "" && (o.ExpiresAt.IsZero() || time.Until(o.ExpiresAt) > skew)
}

// Token returns a bearer token for a catalog, fetching or refreshing it when
// the stored one is missing or about to expire. force discards the stored
// token, for when the API has just answered 401 with it.
func Token(ctx context.Context, st *store.Store, tenant, id string, force bool) (string, error) {
	if !force {
		c, err := st.Credentials(ctx, tenant, id)
		if err != nil {
			return "", err
		}
		if c.OAuth == nil {
			return "", permanent("this tool has no OAuth configuration")
		}
		if fresh(c.OAuth) {
			return c.OAuth.AccessToken, nil
		}
	}
	var token string
	stale := ""
	if force {
		if c, err := st.Credentials(ctx, tenant, id); err == nil && c.OAuth != nil {
			stale = c.OAuth.AccessToken
		}
	}
	err := st.UpdateCredentials(ctx, tenant, id, func(c *store.Credentials) error {
		o := c.OAuth
		if o == nil {
			return permanent("this tool has no OAuth configuration")
		}
		// Another worker may have refreshed while this one waited for the row.
		if fresh(o) && (!force || o.AccessToken != stale) {
			token = o.AccessToken
			return nil
		}
		if err := refresh(ctx, o); err != nil {
			return err
		}
		token = o.AccessToken
		return nil
	})
	return token, err
}

// Fetch obtains a token for a configuration that is not stored yet, to check
// the settings work.
func Fetch(ctx context.Context, o *store.OAuth) error { return refresh(ctx, o) }

func refresh(ctx context.Context, o *store.OAuth) error {
	form := url.Values{}
	switch {
	case o.Grant == store.GrantClientCredentials:
		form.Set("grant_type", "client_credentials")
		if o.Scope != "" {
			form.Set("scope", o.Scope)
		}
		if o.Audience != "" {
			form.Set("audience", o.Audience)
		}
	case o.RefreshToken != "":
		form.Set("grant_type", "refresh_token")
		form.Set("refresh_token", o.RefreshToken)
	default:
		return permanent("the access token expired and the provider issued no refresh token; connect this tool again")
	}
	return exchange(ctx, o, form)
}

// ExchangeCode finishes an authorization-code consent.
func ExchangeCode(ctx context.Context, o *store.OAuth, code, verifier string) error {
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", o.RedirectURL)
	if verifier != "" {
		form.Set("code_verifier", verifier)
	}
	return exchange(ctx, o, form)
}

// exchange posts to the token endpoint and stores what comes back in o.
func exchange(ctx context.Context, o *store.OAuth, form url.Values) error {
	basic := o.ClientAuth == "basic"
	status, body, err := post(ctx, o, form, basic)
	if err != nil {
		return err
	}
	// Providers disagree on where the client credentials go. When the first
	// placement is refused as an unknown client, try the other once.
	if (status == http.StatusUnauthorized || status == http.StatusBadRequest) && strings.Contains(string(body), "invalid_client") && o.ClientSecret != "" {
		if s2, b2, err2 := post(ctx, o, form, !basic); err2 == nil && s2 < 300 {
			status, body = s2, b2
			if basic {
				o.ClientAuth = "body"
			} else {
				o.ClientAuth = "basic"
			}
		}
	}
	fields := parse(body)
	if status >= 500 || status == http.StatusTooManyRequests {
		return &Error{Msg: fmt.Sprintf("token endpoint answered HTTP %d", status)}
	}
	if status >= 300 || fields["error"] != "" {
		reason := fields["error"]
		if d := fields["error_description"]; d != "" {
			reason += ": " + d
		}
		if reason == "" {
			reason = fmt.Sprintf("HTTP %d", status)
		}
		return permanent("the OAuth provider refused the token request (%s); check the client settings or connect this tool again", reason)
	}
	if fields["access_token"] == "" {
		return permanent("the OAuth provider's answer had no access_token")
	}
	o.AccessToken = fields["access_token"]
	if rt := fields["refresh_token"]; rt != "" {
		o.RefreshToken = rt // providers that rotate refresh tokens retire the old one
	}
	o.ExpiresAt = time.Time{}
	if secs, err := strconv.ParseFloat(fields["expires_in"], 64); err == nil && secs > 0 {
		o.ExpiresAt = time.Now().Add(time.Duration(secs) * time.Second).UTC()
	}
	return nil
}

func post(ctx context.Context, o *store.OAuth, form url.Values, basic bool) (int, []byte, error) {
	f := url.Values{}
	for k, v := range form {
		f[k] = v
	}
	if !basic {
		f.Set("client_id", o.ClientID)
		if o.ClientSecret != "" {
			f.Set("client_secret", o.ClientSecret)
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.TokenURL, strings.NewReader(f.Encode()))
	if err != nil {
		return 0, nil, permanent("invalid token URL: %s", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	if basic {
		req.SetBasicAuth(url.QueryEscape(o.ClientID), url.QueryEscape(o.ClientSecret))
	}
	resp, err := netguard.Client(20*time.Second, false).Do(req)
	if err != nil {
		if strings.Contains(err.Error(), "refusing to connect to private address") {
			return 0, nil, permanent("%s", err)
		}
		return 0, nil, &Error{Msg: "token endpoint unreachable: " + err.Error()}
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 256<<10))
	if err != nil {
		return 0, nil, &Error{Msg: err.Error()}
	}
	return resp.StatusCode, body, nil
}

// parse reads a token response, which is JSON by the standard and
// form-encoded from a few providers.
func parse(body []byte) map[string]string {
	out := map[string]string{}
	var obj map[string]any
	if json.Unmarshal(body, &obj) == nil {
		for k, v := range obj {
			switch t := v.(type) {
			case string:
				out[k] = t
			case float64:
				out[k] = strconv.FormatFloat(t, 'f', -1, 64)
			}
		}
		return out
	}
	if vals, err := url.ParseQuery(strings.TrimSpace(string(body))); err == nil {
		for k := range vals {
			out[k] = vals.Get(k)
		}
	}
	return out
}

// NewVerifier returns a PKCE verifier and its S256 challenge.
func NewVerifier() (verifier, challenge string) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	verifier = base64.RawURLEncoding.EncodeToString(b)
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(sum[:])
}

// NewState returns an unguessable state value.
func NewState() string {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// AuthorizeURL builds the consent page address.
func AuthorizeURL(o *store.OAuth, state, challenge string) (string, error) {
	u, err := url.Parse(o.AuthURL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return "", errors.New("authUrl must be a valid http(s) url")
	}
	q := u.Query()
	q.Set("response_type", "code")
	q.Set("client_id", o.ClientID)
	q.Set("redirect_uri", o.RedirectURL)
	q.Set("state", state)
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	if o.Scope != "" {
		q.Set("scope", o.Scope)
	}
	for k, v := range o.AuthParams {
		q.Set(k, v)
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}
