package client

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestVerifySignature(t *testing.T) {
	key := []byte("0123456789abcdef")
	body := []byte(`{"event":"run.finished"}`)
	mac := hmac.New(sha256.New, key)
	mac.Write(body)
	good := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	secret := hex.EncodeToString(key)
	if !VerifySignature(secret, body, good) {
		t.Error("a correct signature was rejected")
	}
	if VerifySignature(secret, []byte(`{"event":"changed"}`), good) {
		t.Error("a changed body was accepted")
	}
	if VerifySignature("not hex", body, good) {
		t.Error("a malformed secret was accepted")
	}
}

func TestErrorsCarryProblems(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"error":{"code":"invalid_dag","message":"failed","problems":["a","b"]}}`))
	}))
	defer srv.Close()
	_, err := New(srv.URL, "k").StartRun(context.Background(), RunRequest{})
	var e *Error
	if !errors.As(err, &e) || e.Code != "invalid_dag" || len(e.Problems) != 2 || e.Status != 422 {
		t.Fatalf("err = %#v", err)
	}
}
