package api

import (
	"context"
	"errors"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"kinora/internal/auth"
	"kinora/internal/store"
)

// API keys: let a script use a few endpoints without signing in. A key is
// "kn_live_" and 32 random characters; only its hash is stored.

// API key scopes.
const (
	scopeLibraryRead    = "library:read"
	scopeLibraryScan    = "library:scan"
	scopeStatusWrite    = "status:write"
	scopeActivityRead   = "activity:read"
	scopeWebhooksManage = "webhooks:manage"
)

const keyPrefix = "kn_live_"

// keyRoutes are the only endpoints a key reaches, and the scope each needs.
// Anything else (accounts, library settings, server settings, keys
// themselves) refuses keys.
var keyRoutes = map[string]string{
	"GET /api/v1/libraries":            scopeLibraryRead,
	"GET /api/v1/items":                scopeLibraryRead,
	"GET /api/v1/items/{id}":           scopeLibraryRead,
	"GET /api/v1/items/{id}/children":  scopeLibraryRead,
	"POST /api/v1/libraries/{id}/scan": scopeLibraryScan,
	"POST /api/v1/status":              scopeStatusWrite,
	"GET /api/v1/playback":             scopeActivityRead,

	"GET /api/v1/integrations/events":                                         scopeWebhooksManage,
	"GET /api/v1/integrations/webhooks":                                       scopeWebhooksManage,
	"POST /api/v1/integrations/webhooks":                                      scopeWebhooksManage,
	"PUT /api/v1/integrations/webhooks/{id}":                                  scopeWebhooksManage,
	"DELETE /api/v1/integrations/webhooks/{id}":                               scopeWebhooksManage,
	"POST /api/v1/integrations/webhooks/{id}/test":                            scopeWebhooksManage,
	"POST /api/v1/integrations/webhooks/{id}/secret":                          scopeWebhooksManage,
	"GET /api/v1/integrations/webhooks/{id}/deliveries":                       scopeWebhooksManage,
	"POST /api/v1/integrations/webhooks/{id}/deliveries/{delivery}/redeliver": scopeWebhooksManage,
	"GET /api/v1/integrations/incoming":                                       scopeWebhooksManage,
	"POST /api/v1/integrations/incoming":                                      scopeWebhooksManage,
	"PUT /api/v1/integrations/incoming/{id}":                                  scopeWebhooksManage,
	"DELETE /api/v1/integrations/incoming/{id}":                               scopeWebhooksManage,
	"POST /api/v1/integrations/incoming/{id}/token":                           scopeWebhooksManage,
	"GET /api/v1/integrations/incoming/{id}/received":                         scopeWebhooksManage,
	"GET /api/v1/ontheway":                                                    scopeWebhooksManage,
	"DELETE /api/v1/ontheway/{id}":                                            scopeWebhooksManage,
}

func validScope(s string) bool {
	for _, sc := range scopeCatalogue {
		if sc.ID == s {
			return true
		}
	}
	return false
}

type keyCtxKey struct{}

// keyFrom is the API key a request was made with, or nil.
func keyFrom(r *http.Request) *store.APIKey {
	k, _ := r.Context().Value(keyCtxKey{}).(*store.APIKey)
	return k
}

// fromHomeNetwork: a loopback or private address, and not through the
// remote tunnel or the cloud.
func fromHomeNetwork(r *http.Request) bool {
	if fromInternet(r) {
		return false
	}
	ip := net.ParseIP(clientIP(r))
	return ip != nil && (ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast())
}

// serveAPIKey handles a request made with an API key: it checks the key,
// where it came from and its scope, then runs h as the key. It returns
// false (doing nothing) when the request has no API key.
func (s *Server) serveAPIKey(w http.ResponseWriter, r *http.Request, h userHandler) bool {
	header := r.Header.Get("Authorization")
	if !strings.HasPrefix(header, "Bearer "+keyPrefix) {
		return false
	}
	k, err := s.Store.APIKeyByHash(r.Context(), hashKey(strings.TrimSpace(strings.TrimPrefix(header, "Bearer "))))
	now := time.Now()
	if err != nil || (k.ExpiresAt > 0 && k.ExpiresAt <= now.Unix()) {
		writeErr(w, http.StatusUnauthorized, "this API key isn't valid: it was revoked or has expired")
		return true
	}
	if k.Network == "local" && !fromHomeNetwork(r) {
		writeErr(w, http.StatusForbidden, "this API key only works from the server's home network")
		return true
	}
	scope, ok := keyRoutes[r.Pattern]
	if !ok {
		writeErr(w, http.StatusForbidden, "API keys can't use this; sign in instead")
		return true
	}
	if !slices.Contains(k.Scopes, scope) {
		writeErr(w, http.StatusForbidden, "this API key can't do that: it needs the "+scope+" permission")
		return true
	}
	s.touchKey(k.ID, now)
	// The key acts with owner rights, but only on the routes above.
	u := store.User{ID: 0, Username: "API key: " + k.Name, IsAdmin: true}
	r = r.WithContext(context.WithValue(r.Context(), keyCtxKey{}, &k))
	h(w, r, u)
	return true
}

func hashKey(key string) string { return auth.HashToken(key) }

// touchKey records use, at most once a minute per key.
func (s *Server) touchKey(id int64, now time.Time) {
	s.integ.mu.Lock()
	if s.integ.keyUsed == nil {
		s.integ.keyUsed = map[int64]int64{}
	}
	last := s.integ.keyUsed[id]
	fresh := now.Unix()-last >= 60
	if fresh {
		s.integ.keyUsed[id] = now.Unix()
	}
	s.integ.mu.Unlock()
	if fresh {
		_ = s.Store.TouchAPIKey(s.baseCtx(), id, now.Unix())
	}
}

type keyView struct {
	ID         int64    `json:"id"`
	Name       string   `json:"name"`
	Prefix     string   `json:"prefix"`
	Scopes     []string `json:"scopes"`
	Network    string   `json:"network"`
	CreatedAt  int64    `json:"createdAt"`
	LastUsedAt int64    `json:"lastUsedAt,omitempty"`
	ExpiresAt  int64    `json:"expiresAt,omitempty"`
	// Key is the whole key, only in the answer that made it.
	Key string `json:"key,omitempty"`
}

func keyViewOf(k store.APIKey) keyView {
	return keyView{ID: k.ID, Name: k.Name, Prefix: k.Prefix, Scopes: k.Scopes, Network: k.Network,
		CreatedAt: k.CreatedAt, LastUsedAt: k.LastUsedAt, ExpiresAt: k.ExpiresAt}
}

func (s *Server) listKeys(w http.ResponseWriter, r *http.Request, u store.User) {
	keys, err := s.Store.ListAPIKeys(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]keyView, 0, len(keys))
	for _, k := range keys {
		out = append(out, keyViewOf(k))
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}

type keyInput struct {
	Name        string   `json:"name"`
	Scopes      []string `json:"scopes"`
	Network     string   `json:"network"`
	ExpiresDays int      `json:"expiresDays"`
}

func (in *keyInput) check() error {
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" || len(in.Name) > 80 {
		return errors.New("give the key a name (up to 80 characters)")
	}
	if len(in.Scopes) == 0 {
		return errors.New("choose at least one thing the key can do")
	}
	scopes := []string{}
	for _, sc := range in.Scopes {
		if !validScope(sc) {
			return errors.New(strconv.Quote(sc) + " isn't a permission a key can have")
		}
		if !slices.Contains(scopes, sc) {
			scopes = append(scopes, sc)
		}
	}
	in.Scopes = scopes
	if in.Network == "" {
		in.Network = "local"
	}
	if in.Network != "local" && in.Network != "any" {
		return errors.New(`network is "local" or "any"`)
	}
	switch in.ExpiresDays {
	case 0, 30, 90, 365:
	default:
		return errors.New("expiresDays is 0 (never), 30, 90 or 365")
	}
	return nil
}

// createKey makes a key and answers with the whole key, the only time it
// is shown.
func (s *Server) createKey(w http.ResponseWriter, r *http.Request, u store.User) {
	var in keyInput
	if err := readJSON(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := in.check(); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	full := randomToken(keyPrefix, 32)
	k := store.APIKey{Name: in.Name, Prefix: full[:12], Hash: hashKey(full), Scopes: in.Scopes, Network: in.Network}
	if in.ExpiresDays > 0 {
		k.ExpiresAt = time.Now().Add(time.Duration(in.ExpiresDays) * 24 * time.Hour).Unix()
	}
	if err := s.Store.CreateAPIKey(r.Context(), &k); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	v := keyViewOf(k)
	v.Key = full
	writeJSON(w, http.StatusCreated, v)
}

func (s *Server) deleteKey(w http.ResponseWriter, r *http.Request, u store.User) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err == nil {
		err = s.Store.DeleteAPIKey(r.Context(), id)
	}
	if err != nil {
		writeErr(w, http.StatusNotFound, "no such key")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
