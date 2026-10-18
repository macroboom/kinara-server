package tmdb

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestClientAuthAndRetry(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if n == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		if r.URL.Path != "/search/movie" || r.URL.Query().Get("query") != "Night Shift" ||
			r.URL.Query().Get("year") != "2023" {
			t.Errorf("unexpected request %s", r.URL)
		}
		if r.URL.Query().Get("api_key") != "abc" {
			t.Errorf("missing api key")
		}
		w.Write([]byte(`{"results":[{"id":42,"title":"Night Shift","release_date":"2023-05-01"}]}`))
	}))
	defer srv.Close()

	c := New("abc")
	c.BaseURL = srv.URL
	res, err := c.SearchMovie(context.Background(), "Night Shift", 2023)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 || res[0].ID != 42 || YearOf(res[0].ReleaseDate) != 2023 {
		t.Errorf("got %+v", res)
	}
	if calls.Load() != 2 {
		t.Errorf("want a retry after 429, calls = %d", calls.Load())
	}
}

func TestBearerToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer eyJtoken" || r.URL.Query().Get("api_key") != "" {
			t.Errorf("bad auth: %q %q", r.Header.Get("Authorization"), r.URL.RawQuery)
		}
		w.Write([]byte(`{"id":7,"name":"Small Hours"}`))
	}))
	defer srv.Close()
	c := New("eyJtoken")
	c.BaseURL = srv.URL
	d, err := c.TV(context.Background(), 7)
	if err != nil || d.Name != "Small Hours" {
		t.Fatalf("got %+v %v", d, err)
	}
}

func TestStatusError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"status_message":"Invalid API key"}`))
	}))
	defer srv.Close()
	c := New("bad")
	c.BaseURL = srv.URL
	_, err := c.Movie(context.Background(), 1)
	se, ok := err.(*StatusError)
	if !ok || se.Code != 401 {
		t.Fatalf("want 401 StatusError, got %v", err)
	}
}

func TestRecommendationsFallsBackToSimilar(t *testing.T) {
	var asked []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = append(asked, r.URL.Path)
		if strings.HasSuffix(r.URL.Path, "/recommendations") {
			w.Write([]byte(`{"page":1,"total_pages":1,"results":[]}`))
			return
		}
		w.Write([]byte(`{"page":1,"total_pages":3,"results":[{"id":7,"name":"Alike"}]}`))
	}))
	defer srv.Close()
	c := New("k")
	c.BaseURL = srv.URL
	p, err := c.Recommendations(context.Background(), "tv", 42, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(asked) != 2 || asked[0] != "/tv/42/recommendations" || asked[1] != "/tv/42/similar" {
		t.Fatalf("asked %v", asked)
	}
	if len(p.Results) != 1 || p.Results[0].MediaType != "tv" || p.Results[0].ID != 7 {
		t.Fatalf("results %+v", p.Results)
	}
}
