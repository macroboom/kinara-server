package arr

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
)

// Finding Radarr, Sonarr and Prowlarr already on this computer, and their
// API keys, so the owner doesn't have to copy them.

// Identify says whether the app of a kind answers at an address, without a
// key: its web page names it.
func Identify(ctx context.Context, kind, baseURL string) bool {
	return pageTitled(ctx, strings.TrimRight(baseURL, "/"), kind)
}

// pageTitled: the page at an address has a title naming the app (Radarr,
// Sonarr and Prowlarr serve their web app there, signed in or not).
func pageTitled(ctx context.Context, base, name string) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/", nil)
	if err != nil {
		return false
	}
	resp, err := (&http.Client{Timeout: 3 * time.Second}).Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	m := titleRE.FindSubmatch(body)
	return m != nil && strings.Contains(strings.ToLower(string(m[1])), strings.ToLower(name))
}

var (
	titleRE  = regexp.MustCompile(`(?is)<title>(.*?)</title>`)
	apiKeyRE = regexp.MustCompile(`<ApiKey>\s*([0-9A-Za-z]+)\s*</ApiKey>`)
)

// ConfigPaths are where an app installed on this computer keeps its
// settings file (config.xml, with its API key). Tests replace it.
var ConfigPaths = func(kind string) []string {
	name := strings.ToUpper(kind[:1]) + kind[1:]
	home, _ := os.UserHomeDir()
	switch runtime.GOOS {
	case "windows":
		return []string{filepath.Join(os.Getenv("ProgramData"), name, "config.xml")}
	case "darwin":
		return []string{filepath.Join(home, ".config", name, "config.xml")}
	}
	return []string{
		filepath.Join("/var/lib", kind, "config.xml"),
		filepath.Join(home, ".config", name, "config.xml"),
		filepath.Join("/home", kind, ".config", name, "config.xml"),
	}
}

// ConfigKeys are the API keys in the settings files of an app installed on
// this computer (Kinara can read them when it runs as a service).
func ConfigKeys(kind string) []string {
	var keys []string
	for _, p := range ConfigPaths(kind) {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		if m := apiKeyRE.FindSubmatch(b); m != nil {
			keys = append(keys, string(m[1]))
		}
	}
	return keys
}
