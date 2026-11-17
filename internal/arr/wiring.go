package arr

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// Wiring the services to each other: root folders, a download client in
// Radarr and Sonarr, and Radarr and Sonarr as apps in Prowlarr (which then
// hands them its indexers). Each step leaves what's there already alone,
// so running it twice changes nothing.

// HealthItem is a warning the service shows on its own System, Status page.
type HealthItem struct {
	Source  string `json:"source"`
	Type    string `json:"type"` // ok, notice, warning, error
	Message string `json:"message"`
	WikiURL string `json:"wikiUrl,omitempty"`
}

// Health lists the service's warnings (no indexers, download client down,
// a root folder missing...).
func (c *Client) Health(ctx context.Context) ([]HealthItem, error) {
	var out []HealthItem
	err := c.do(ctx, http.MethodGet, "health", nil, &out)
	if out == nil {
		out = []HealthItem{}
	}
	return out, err
}

// EnsureRootFolder adds a root folder unless it's there.
func (c *Client) EnsureRootFolder(ctx context.Context, path string) (added bool, err error) {
	var have []RootFolder
	if err := c.do(ctx, http.MethodGet, "rootfolder", nil, &have); err != nil {
		return false, err
	}
	for _, f := range have {
		if strings.TrimRight(f.Path, "/\\") == strings.TrimRight(path, "/\\") {
			return false, nil
		}
	}
	return true, c.do(ctx, http.MethodPost, "rootfolder", map[string]any{"path": path}, nil)
}

// Profiles lists the quality profiles.
func (c *Client) Profiles(ctx context.Context) ([]Profile, error) {
	var out []Profile
	return out, c.do(ctx, http.MethodGet, "qualityprofile", nil, &out)
}

// QbitClient is how Radarr or Sonarr reach qBittorrent.
type QbitClient struct {
	Host     string
	Port     int
	UseSSL   bool
	Username string
	Password string
	// Category files Radarr's or Sonarr's downloads apart ("radarr").
	Category string
}

// EnsureQbittorrent adds qBittorrent as a download client unless one is.
// The one Kinara added before ("qBittorrent") is pointed at q when it
// reaches another (the owner switched to their own copy, say).
func (c *Client) EnsureQbittorrent(ctx context.Context, q QbitClient) (added bool, err error) {
	var have []map[string]any
	if err := c.do(ctx, http.MethodGet, "downloadclient", nil, &have); err != nil {
		return false, err
	}
	categoryField := "movieCategory"
	if c.Kind == Sonarr {
		categoryField = "tvCategory"
	}
	for _, d := range have {
		if !strings.EqualFold(fmt.Sprint(d["implementation"]), "QBittorrent") {
			continue
		}
		if fmt.Sprint(d["name"]) != "qBittorrent" || (fieldValue(d, "host") == q.Host && fmt.Sprint(fieldValue(d, "port")) == fmt.Sprint(q.Port)) {
			return false, nil
		}
		setFields(d, map[string]any{"host": q.Host, "port": q.Port, "useSsl": q.UseSSL, "username": q.Username,
			"password": q.Password, categoryField: q.Category})
		return true, c.do(ctx, http.MethodPut, fmt.Sprintf("downloadclient/%v?forceSave=true", d["id"]), d, nil)
	}
	var schema []map[string]any
	if err := c.do(ctx, http.MethodGet, "downloadclient/schema", nil, &schema); err != nil {
		return false, err
	}
	var dc map[string]any
	for _, s := range schema {
		if strings.EqualFold(fmt.Sprint(s["implementation"]), "QBittorrent") {
			dc = s
			break
		}
	}
	if dc == nil {
		return false, errors.New("this version has no qBittorrent download client")
	}
	setFields(dc, map[string]any{"host": q.Host, "port": q.Port, "useSsl": q.UseSSL, "username": q.Username,
		"password": q.Password, categoryField: q.Category})
	dc["name"] = "qBittorrent"
	dc["enable"] = true
	delete(dc, "id")
	// forceSave: saved even if its own test fails (qBittorrent still starting).
	return true, c.do(ctx, http.MethodPost, "downloadclient?forceSave=true", dc, nil)
}

// ProwlarrApp is Radarr or Sonarr as Prowlarr knows it.
type ProwlarrApp struct {
	// Kind is Radarr or Sonarr.
	Kind string
	// BaseURL is how Prowlarr reaches the app; ProwlarrURL how the app
	// reaches Prowlarr.
	BaseURL     string
	ProwlarrURL string
	APIKey      string
}

// EnsureApp adds Radarr or Sonarr to Prowlarr (full sync: Prowlarr adds and
// keeps its indexers there) unless one of that kind is there.
func (c *Client) EnsureApp(ctx context.Context, app ProwlarrApp) (added bool, err error) {
	impl := "Radarr"
	if app.Kind == Sonarr {
		impl = "Sonarr"
	}
	var have []map[string]any
	if err := c.do(ctx, http.MethodGet, "applications", nil, &have); err != nil {
		return false, err
	}
	for _, a := range have {
		if strings.EqualFold(fmt.Sprint(a["implementation"]), impl) {
			return false, nil
		}
	}
	var schema []map[string]any
	if err := c.do(ctx, http.MethodGet, "applications/schema", nil, &schema); err != nil {
		return false, err
	}
	var a map[string]any
	for _, s := range schema {
		if strings.EqualFold(fmt.Sprint(s["implementation"]), impl) {
			a = s
			break
		}
	}
	if a == nil {
		return false, fmt.Errorf("Prowlarr has no %s app type", impl)
	}
	setFields(a, map[string]any{"prowlarrUrl": app.ProwlarrURL, "baseUrl": app.BaseURL, "apiKey": app.APIKey})
	a["name"] = impl
	a["syncLevel"] = "fullSync"
	delete(a, "id")
	return true, c.do(ctx, http.MethodPost, "applications?forceSave=true", a, nil)
}

// Indexers counts Prowlarr's indexers, and how many are turned on.
func (c *Client) Indexers(ctx context.Context) (total, enabled int, err error) {
	var list []struct {
		Enable bool `json:"enable"`
	}
	if err := c.do(ctx, http.MethodGet, "indexer", nil, &list); err != nil {
		return 0, 0, err
	}
	for _, i := range list {
		if i.Enable {
			enabled++
		}
	}
	return len(list), enabled, nil
}

// ProwlarrCheck connects to Prowlarr: its status and indexers.
func (c *Client) ProwlarrCheck(ctx context.Context) (Status, int, error) {
	var st Status
	if err := c.do(ctx, http.MethodGet, "system/status", nil, &st); err != nil {
		return st, 0, err
	}
	if app := strings.ToLower(st.AppName); app != "" && app != Prowlarr {
		return st, 0, fmt.Errorf("that address is %s, not Prowlarr", st.AppName)
	}
	_, enabled, err := c.Indexers(ctx)
	return st, enabled, err
}

// fieldValue reads a value from a "fields" list by name.
func fieldValue(obj map[string]any, name string) any {
	fields, _ := obj["fields"].([]any)
	for _, f := range fields {
		if m, ok := f.(map[string]any); ok && fmt.Sprint(m["name"]) == name {
			if v, ok := m["value"].(float64); ok && v == float64(int(v)) {
				return int(v)
			}
			return m["value"]
		}
	}
	return nil
}

// EnsureRemotePathMapping maps where a download client on another system
// saves (remotePath, as it writes it) to where this app sees the same
// folder, unless that's there.
func (c *Client) EnsureRemotePathMapping(ctx context.Context, host, remotePath, localPath string) (added bool, err error) {
	var have []struct {
		Host       string `json:"host"`
		RemotePath string `json:"remotePath"`
	}
	if err := c.do(ctx, http.MethodGet, "remotepathmapping", nil, &have); err != nil {
		return false, err
	}
	for _, m := range have {
		if strings.EqualFold(m.Host, host) && strings.EqualFold(m.RemotePath, remotePath) {
			return false, nil
		}
	}
	return true, c.do(ctx, http.MethodPost, "remotepathmapping", map[string]string{"host": host, "remotePath": remotePath, "localPath": localPath}, nil)
}

// setFields sets values in a schema's "fields" list by name.
func setFields(obj map[string]any, values map[string]any) {
	fields, _ := obj["fields"].([]any)
	for _, f := range fields {
		field, ok := f.(map[string]any)
		if !ok {
			continue
		}
		if v, ok := values[fmt.Sprint(field["name"])]; ok {
			field["value"] = v
		}
	}
}

// EnsureFlareSolverr adds FlareSolverr to Prowlarr as an indexer proxy
// unless one is there. Prowlarr only sends indexers through a proxy that
// share a tag with it, so it gets the tag "flaresolverr" (made if missing):
// give that tag to the indexers that sit behind Cloudflare.
func (c *Client) EnsureFlareSolverr(ctx context.Context, host string) (added bool, err error) {
	var have []map[string]any
	if err := c.do(ctx, http.MethodGet, "indexerProxy", nil, &have); err != nil {
		return false, err
	}
	for _, p := range have {
		if strings.EqualFold(fmt.Sprint(p["implementation"]), "FlareSolverr") {
			return false, nil
		}
	}
	tag, err := c.ensureTag(ctx, "flaresolverr")
	if err != nil {
		return false, err
	}
	var schema []map[string]any
	if err := c.do(ctx, http.MethodGet, "indexerProxy/schema", nil, &schema); err != nil {
		return false, err
	}
	var p map[string]any
	for _, s := range schema {
		if strings.EqualFold(fmt.Sprint(s["implementation"]), "FlareSolverr") {
			p = s
			break
		}
	}
	if p == nil {
		return false, errors.New("this Prowlarr has no FlareSolverr proxy type")
	}
	if !strings.HasSuffix(host, "/") {
		host += "/"
	}
	setFields(p, map[string]any{"host": host, "requestTimeout": 60})
	p["name"] = "FlareSolverr"
	p["tags"] = []int{tag}
	delete(p, "id")
	return true, c.do(ctx, http.MethodPost, "indexerProxy?forceSave=true", p, nil)
}

// ensureTag returns the id of a tag, making it if needed.
func (c *Client) ensureTag(ctx context.Context, label string) (int, error) {
	var tags []Tag
	if err := c.do(ctx, http.MethodGet, "tag", nil, &tags); err != nil {
		return 0, err
	}
	for _, t := range tags {
		if strings.EqualFold(t.Label, label) {
			return t.ID, nil
		}
	}
	var made Tag
	err := c.do(ctx, http.MethodPost, "tag", map[string]string{"label": label}, &made)
	return made.ID, err
}
