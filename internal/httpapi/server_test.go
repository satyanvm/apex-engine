package httpapi

import (
	"testing"
	"time"
)

func TestParseKeys(t *testing.T) {
	got, err := ParseKeys(" abcdefghijklmnop:acme , qrstuvwxyz012345:beta ")
	if err != nil || got["abcdefghijklmnop"] != "acme" || got["qrstuvwxyz012345"] != "beta" {
		t.Fatalf("got %v, %v", got, err)
	}
	for _, bad := range []string{"", "short:acme", "abcdefghijklmnop", "abcdefghijklmnop:"} {
		if _, err := ParseKeys(bad); err == nil {
			t.Errorf("%q should be rejected", bad)
		}
	}
}

func TestLimiter(t *testing.T) {
	l := newLimiter(2, 40*time.Millisecond)
	if !l.allow("a") || !l.allow("a") {
		t.Fatal("first two should pass")
	}
	if l.allow("a") {
		t.Fatal("third should be limited")
	}
	if !l.allow("b") {
		t.Fatal("tenants are limited separately")
	}
	time.Sleep(60 * time.Millisecond)
	if !l.allow("a") {
		t.Fatal("the window should have passed")
	}
}

func TestValidHeaderName(t *testing.T) {
	for name, want := range map[string]bool{"Authorization": true, "X-Api-Key": true, "": false, "a b": false, "a:b": false, "a\nb": false} {
		if got := validHeaderName(name); got != want {
			t.Errorf("%q = %v", name, got)
		}
	}
}
