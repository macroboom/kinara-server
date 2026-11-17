package arr

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"time"
)

// ErrQbitRefused: qBittorrent turned down the user name or password.
// ErrQbitBanned: it has blocked the address after too many of those. Asking
// again doesn't help (and adds to the count), so callers stop.
var (
	ErrQbitRefused = errors.New("qBittorrent turned down the user name or password (Tools, Options, Web UI)")
	ErrQbitBanned  = errors.New("qBittorrent has blocked Kinara's address after too many failed sign-ins; restart it (docker restart qbittorrent) to clear that")
)

// Qbit talks to qBittorrent's web interface (API v2).
type Qbit struct {
	BaseURL  string
	Username string
	Password string

	client *http.Client
}

func (q *Qbit) base() string {
	b := strings.TrimRight(strings.TrimSpace(q.BaseURL), "/")
	if b != "" && !strings.HasPrefix(b, "http://") && !strings.HasPrefix(b, "https://") {
		b = "http://" + b
	}
	return b
}

// login signs in (keeping the session cookie for the calls after).
func (q *Qbit) login(ctx context.Context) error {
	if q.base() == "" {
		return errors.New("no address set")
	}
	jar, _ := cookiejar.New(nil)
	q.client = &http.Client{Timeout: 15 * time.Second, Jar: jar}
	form := url.Values{"username": {q.Username}, "password": {q.Password}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, q.base()+"/api/v2/auth/login", strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	q.headers(req)
	resp, err := q.client.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return fmt.Errorf("couldn't reach it: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	text := strings.TrimSpace(string(body))
	// Signed in: "Ok." with 200 up to qBittorrent 5.1, an empty 204 from
	// 5.2 on (both set the session cookie). Turned down: "Fails." with 200,
	// or 401 from 5.2 on.
	switch {
	case resp.StatusCode == http.StatusNoContent, resp.StatusCode == http.StatusOK && text == "Ok.":
		return nil
	case resp.StatusCode == http.StatusForbidden:
		return ErrQbitBanned
	case text == "Fails.":
		return ErrQbitRefused
	case resp.StatusCode == http.StatusUnauthorized:
		// A 401 without "Fails." is its Host header or cross-site check.
		return errors.New("qBittorrent turned the request away (401) before checking the password: its Web UI's Host header validation or CSRF protection")
	case resp.StatusCode == http.StatusOK:
		return fmt.Errorf("unexpected answer to signing in: %q", text)
	}
	return fmt.Errorf("answered %d", resp.StatusCode)
}

func (q *Qbit) call(ctx context.Context, method, path string, form url.Values) (string, error) {
	if q.client == nil {
		if err := q.login(ctx); err != nil {
			return "", err
		}
	}
	var rd io.Reader
	if form != nil {
		rd = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, q.base()+"/api/v2/"+path, rd)
	if err != nil {
		return "", err
	}
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	q.headers(req)
	resp, err := q.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("qBittorrent answered %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return string(body), nil
}

// Version signs in and returns qBittorrent's version ("v5.0.2").
func (q *Qbit) Version(ctx context.Context) (string, error) {
	if err := q.login(ctx); err != nil {
		return "", err
	}
	v, err := q.call(ctx, http.MethodGet, "app/version", nil)
	return strings.TrimSpace(v), err
}

// Prepare sets where downloads go and adds the radarr and sonarr
// categories (each in its own folder under it).
func (q *Qbit) Prepare(ctx context.Context, savePath string) error {
	if savePath != "" {
		prefs := `{"save_path":"` + jsonEscape(savePath) + `"}`
		if _, err := q.call(ctx, http.MethodPost, "app/setPreferences", url.Values{"json": {prefs}}); err != nil {
			return err
		}
	}
	base := ""
	if savePath != "" {
		base = strings.TrimRight(savePath, "/") + "/"
	}
	return q.Categories(ctx, base)
}

// Categories adds the radarr and sonarr categories, each saving in its own
// folder under base (which ends with its separator; "" for qBittorrent's
// default). It leaves qBittorrent's own settings alone: for an owner's own
// copy, which has their other downloads too.
func (q *Qbit) Categories(ctx context.Context, base string) error {
	for _, cat := range []string{Radarr, Sonarr} {
		form := url.Values{"category": {cat}}
		if base != "" {
			form.Set("savePath", base+cat)
		}
		// 409: it's there already.
		if _, err := q.call(ctx, http.MethodPost, "torrents/createCategory", form); err != nil && !strings.Contains(err.Error(), "409") {
			return err
		}
	}
	return nil
}

// headers gets past qBittorrent's two checks on where a request comes
// from. Its Host check turns away (401) a Host header naming a port other
// than the one qBittorrent listens on, which is what Docker's port mapping
// ("8090:8080") produces; without a port there's nothing to compare. Its
// cross-site check compares Referer or Origin with Host; an API client
// sends neither, and with neither it doesn't check.
func (q *Qbit) headers(req *http.Request) {
	if h := req.URL.Hostname(); h != "" {
		if strings.Contains(h, ":") {
			h = "[" + h + "]"
		}
		req.Host = h
	}
}

func jsonEscape(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`)
	return r.Replace(s)
}

// QbitProbe looks for qBittorrent's Web UI at an address without signing
// in. found: something there answers like qBittorrent. open: it lets
// Kinara in without a password (its "bypass authentication for clients on
// localhost" setting, or a whitelisted subnet).
func QbitProbe(ctx context.Context, baseURL string) (found, open bool) {
	q := &Qbit{BaseURL: baseURL}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, q.base()+"/api/v2/app/version", nil)
	if err != nil {
		return false, false
	}
	q.headers(req)
	resp, err := (&http.Client{Timeout: 3 * time.Second}).Do(req)
	if err != nil {
		return false, false
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
	text := strings.TrimSpace(string(body))
	switch {
	case resp.StatusCode == http.StatusOK && strings.HasPrefix(text, "v"):
		return true, true
	case resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusUnauthorized:
		// Signed out: make sure it's qBittorrent, not something else on the port.
		return pageTitled(ctx, q.base(), "qbittorrent"), false
	}
	return false, false
}
