package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// An incoming hook's token isn't written to the request log.
func TestLoggedPathHidesSecrets(t *testing.T) {
	mux := http.NewServeMux()
	var got []string
	h := func(w http.ResponseWriter, r *http.Request) { got = append(got, loggedPath(r)) }
	mux.HandleFunc("POST /api/v1/hooks/in/{token}", h)
	mux.HandleFunc("GET /api/v1/items/{id}", h)
	for _, req := range []*http.Request{
		httptest.NewRequest("POST", "/api/v1/hooks/in/in_supersecret", nil),
		httptest.NewRequest("GET", "/api/v1/items/abc", nil),
	} {
		mux.ServeHTTP(httptest.NewRecorder(), req)
	}
	if len(got) != 2 || got[0] != "/api/v1/hooks/in/{token}" || got[1] != "/api/v1/items/abc" {
		t.Fatalf("logged %v", got)
	}
}
