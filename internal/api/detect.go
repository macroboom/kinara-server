package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"kinara/internal/arr"
	"kinara/internal/store"
)

// Finding the download apps already on this computer. If Radarr, Sonarr,
// Prowlarr or qBittorrent answer on their usual ports, the page offers to
// connect those (with the keys Kinara could find, so there's often nothing
// to type); if none do, it offers the Docker setup instead.

func (s *Server) detectRoutes(mux *http.ServeMux) {
	mux.Handle("GET /api/v1/arr/detect", s.admin(s.getDetect))
	mux.Handle("POST /api/v1/arr/detect/connect", s.admin(s.connectFound))
}

// foundApp is one app as the page shows it.
type foundApp struct {
	Kind    string `json:"kind"` // radarr, sonarr, prowlarr or qbit
	Name    string `json:"name"`
	Found   bool   `json:"found"`
	URL     string `json:"url,omitempty"`
	Version string `json:"version,omitempty"`
	// KeyFound: Kinara has a key or password that works (from the app's
	// own settings file, its setup script or its saved connection), or the
	// app needs none: nothing to type.
	KeyFound bool `json:"keyFound"`
	// Stack: one Kinara's setup script started.
	Stack bool `json:"stack,omitempty"`
}

// detected is a found app with what Kinara signs in with (never sent).
type detected struct {
	foundApp
	key, user, pass string
}

// The ports each app uses when installed with its defaults (qBittorrent:
// Kinara's setup's, then its own).
var detectPorts = map[string][]int{
	arr.Radarr:   {arr.PortRadarr},
	arr.Sonarr:   {arr.PortSonarr},
	arr.Prowlarr: {arr.PortProwlarr},
	"qbit":       {arr.PortQbit, 8080},
}

func (s *Server) detectApps(ctx context.Context) []detected {
	a := s.arrSettings(ctx)
	kinds := []struct{ kind, name string }{{arr.Radarr, "Radarr"}, {arr.Sonarr, "Sonarr"}, {arr.Prowlarr, "Prowlarr"}, {"qbit", "qBittorrent"}}
	out := make([]detected, len(kinds))
	var wg sync.WaitGroup
	for i, k := range kinds {
		out[i] = detected{foundApp: foundApp{Kind: k.kind, Name: k.name}}
		wg.Add(1)
		go func(d *detected) {
			defer wg.Done()
			cctx, cancel := context.WithTimeout(ctx, 8*time.Second)
			defer cancel()
			for _, port := range detectPorts[d.Kind] {
				base := stackURL(port)
				if d.Kind == "qbit" {
					if s.probeQbit(cctx, a, base, d) {
						return
					}
					continue
				}
				if s.probeArr(cctx, a, base, d) {
					return
				}
			}
		}(&out[i])
	}
	wg.Wait()
	return out
}

// probeArr looks for Radarr, Sonarr or Prowlarr at an address and tries the
// keys Kinara knows of: its setup script's, the one saved for it, and the
// ones in the app's own settings file.
func (s *Server) probeArr(ctx context.Context, a store.ArrSettings, base string, d *detected) bool {
	var keys []string
	stackKey := ""
	if st := a.Stack; st != nil {
		stackKey = map[string]string{arr.Radarr: st.RadarrKey, arr.Sonarr: st.SonarrKey, arr.Prowlarr: st.ProwlarrKey}[d.Kind]
		keys = append(keys, stackKey)
	}
	switch d.Kind {
	case arr.Radarr:
		keys = append(keys, a.Radarr.APIKey)
	case arr.Sonarr:
		keys = append(keys, a.Sonarr.APIKey)
	case arr.Prowlarr:
		keys = append(keys, a.Prowlarr.APIKey)
	}
	keys = append(keys, arr.ConfigKeys(d.Kind)...)
	for _, k := range keys {
		if k == "" {
			continue
		}
		c := &arr.Client{Kind: d.Kind, BaseURL: base, APIKey: k, HTTP: &http.Client{Timeout: 4 * time.Second}}
		var st arr.Status
		var err error
		if d.Kind == arr.Prowlarr {
			st, _, err = c.ProwlarrCheck(ctx)
		} else {
			var o arr.Options
			o, err = c.Check(ctx)
			st = o.Status
		}
		var ae *arr.Error
		if err == nil {
			d.Found, d.URL, d.Version, d.KeyFound, d.key = true, base, st.Version, true, k
			d.Stack = k == stackKey
			return true
		}
		if !errors.As(err, &ae) {
			// Nothing answers here (or not this app): no point trying more keys.
			break
		}
	}
	if arr.Identify(ctx, d.Kind, base) {
		d.Found, d.URL = true, base
		return true
	}
	return false
}

// probeQbit looks for qBittorrent's Web UI at an address: open to Kinara
// with no password, or with the setup script's or the saved one.
func (s *Server) probeQbit(ctx context.Context, a store.ArrSettings, base string, d *detected) bool {
	found, open := arr.QbitProbe(ctx, base)
	if !found {
		return false
	}
	d.Found, d.URL = true, base
	if open {
		d.KeyFound = true
		return true
	}
	type login struct{ user, pass string }
	var tries []login
	if st := a.Stack; st != nil {
		tries = append(tries, login{st.QbitUser, st.QbitPass})
	}
	if a.Qbit.Username != "" {
		tries = append(tries, login{a.Qbit.Username, a.Qbit.Password})
	}
	for i, l := range tries {
		v, err := (&arr.Qbit{BaseURL: base, Username: l.user, Password: l.pass}).Version(ctx)
		if err == nil {
			d.Version, d.KeyFound, d.user, d.pass = v, true, l.user, l.pass
			d.Stack = a.Stack != nil && i == 0
			return true
		}
		// A turned-down password counts toward qBittorrent blocking the
		// address: one wrong try is the most Kinara makes.
		if errors.Is(err, arr.ErrQbitRefused) || errors.Is(err, arr.ErrQbitBanned) {
			break
		}
	}
	return true
}

func (s *Server) getDetect(w http.ResponseWriter, r *http.Request, u store.User) {
	found := s.detectApps(r.Context())
	apps := make([]foundApp, len(found))
	anyFound := false
	for i, d := range found {
		apps[i] = d.foundApp
		anyFound = anyFound || d.Found
	}
	writeJSON(w, http.StatusOK, map[string]any{"apps": apps, "any": anyFound, "inDocker": inDocker()})
}

// connectFound connects the apps found on this computer: with the keys
// Kinara found, or the ones typed for apps whose key it couldn't. Then it
// links them to each other.
func (s *Server) connectFound(w http.ResponseWriter, r *http.Request, u store.User) {
	var in struct {
		// Keys typed for Radarr, Sonarr and Prowlarr (by kind).
		Keys map[string]string `json:"keys"`
		Qbit struct {
			Username string `json:"username"`
			Password string `json:"password"`
		} `json:"qbit"`
	}
	if r.ContentLength != 0 {
		if err := readJSON(r, &in); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	found := s.detectApps(ctx)
	a := s.arrSettings(ctx)
	var out steps
	for _, d := range found {
		if !d.Found {
			continue
		}
		switch d.Kind {
		case arr.Radarr, arr.Sonarr:
			key := strings.TrimSpace(in.Keys[d.Kind])
			if key == "" {
				key = d.key
			}
			if key == "" {
				out.add(d.Name, false, errors.New("its API key is needed (Settings, General, Security)"))
				continue
			}
			c := &arr.Client{Kind: d.Kind, BaseURL: d.URL, APIKey: key}
			o, err := c.Check(ctx)
			if err != nil {
				out.add(d.Name+" at "+d.URL, false, err)
				continue
			}
			srv := &a.Radarr
			if d.Kind == arr.Sonarr {
				srv = &a.Sonarr
			}
			srv.Enabled, srv.URL, srv.APIKey = true, d.URL, key
			if srv.ProfileID == 0 {
				srv.ProfileID = pickProfile(o.Profiles)
			}
			if srv.RootFolder == "" && len(o.RootFolders) > 0 {
				srv.RootFolder = o.RootFolders[0].Path
			}
			note := ""
			if srv.RootFolder == "" {
				// A new install has no folder for new titles yet. On the same
				// computer, Kinara's own library folder is the one.
				kind := store.KindMovies
				if d.Kind == arr.Sonarr {
					kind = store.KindTV
				}
				if dir := s.libraryFolder(ctx, kind); dir != "" {
					if _, err := c.EnsureRootFolder(ctx, dir); err == nil {
						srv.RootFolder, note = dir, "new titles go to "+dir+", Kinara's library folder"
					}
				}
			}
			if srv.RootFolder == "" {
				out = append(out, stepView{Title: d.Name + " at " + d.URL, State: "failed",
					Note: "it has no folder for new titles yet: add one in " + d.Name + " (Settings, Media Management, Root Folders), then press Connect again"})
				continue
			}
			out = append(out, stepView{Title: d.Name + " at " + d.URL, State: "done", Note: note})
		case arr.Prowlarr:
			key := strings.TrimSpace(in.Keys[d.Kind])
			if key == "" {
				key = d.key
			}
			if key == "" {
				out.add(d.Name, false, errors.New("its API key is needed (Settings, General, Security)"))
				continue
			}
			if _, _, err := (&arr.Client{Kind: arr.Prowlarr, BaseURL: d.URL, APIKey: key}).ProwlarrCheck(ctx); err != nil {
				out.add(d.Name+" at "+d.URL, false, err)
				continue
			}
			a.Prowlarr = store.ArrLink{URL: d.URL, APIKey: key}
			out.add(d.Name+" at "+d.URL, true, nil)
		case "qbit":
			user, pass := d.user, d.pass
			if in.Qbit.Password != "" || in.Qbit.Username != "" {
				user, pass = strings.TrimSpace(in.Qbit.Username), in.Qbit.Password
			}
			if !d.KeyFound || in.Qbit.Password != "" {
				if _, err := (&arr.Qbit{BaseURL: d.URL, Username: user, Password: pass}).Version(ctx); err != nil {
					out.add(d.Name+" at "+d.URL, false, err)
					continue
				}
			}
			a.Qbit = store.QbitLink{URL: d.URL, Username: user, Password: pass}
			out.add(d.Name+" at "+d.URL, true, nil)
		}
	}
	if len(out) == 0 {
		writeErr(w, http.StatusConflict, "none of the apps were found on this computer")
		return
	}
	if err := s.Store.PutArrSettings(ctx, a); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	out = append(out, s.linkApps(ctx, linkInput{})...)
	ok := true
	for _, st := range out {
		if st.State == "failed" {
			ok = false
		}
	}
	s.Log.Info("connected the download apps found", "by", u.Username, "ok", ok)
	s.arrKick()
	writeJSON(w, http.StatusOK, map[string]any{"steps": out, "ok": ok})
}

// pickProfile prefers 1080p, else the first.
func pickProfile(ps []arr.Profile) int {
	for _, p := range ps {
		if strings.EqualFold(p.Name, "HD-1080p") || strings.EqualFold(p.Name, "HD - 720p/1080p") {
			return p.ID
		}
	}
	if len(ps) > 0 {
		return ps[0].ID
	}
	return 0
}

// libraryFolder is the first folder of Kinara's first library of a kind.
func (s *Server) libraryFolder(ctx context.Context, kind string) string {
	libs, err := s.Store.ListLibraries(ctx)
	if err != nil {
		return ""
	}
	for _, l := range libs {
		if l.Kind == kind && l.Path != "" {
			return l.Path
		}
	}
	return ""
}
