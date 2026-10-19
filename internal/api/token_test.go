package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestTokenFromPrefersExplicitTokens(t *testing.T) {
	r := httptest.NewRequest("GET", "/api/v1/images/x/poster?token=fresh", nil)
	r.AddCookie(&http.Cookie{Name: cookieName, Value: "stale"})
	if got := tokenFrom(r, true); got != "fresh" {
		t.Fatalf("a URL token beats a leftover cookie: %q", got)
	}
	if got := tokenFrom(r, false); got != "stale" {
		t.Fatalf("routes that don't take URL tokens still use the cookie: %q", got)
	}
	r.Header.Set("Authorization", "Bearer header")
	if got := tokenFrom(r, true); got != "header" {
		t.Fatalf("the header beats both: %q", got)
	}
	r = httptest.NewRequest("GET", "/api/v1/images/x/poster", nil)
	r.AddCookie(&http.Cookie{Name: cookieName, Value: "only"})
	if got := tokenFrom(r, true); got != "only" {
		t.Fatalf("no URL token: the cookie: %q", got)
	}
}
