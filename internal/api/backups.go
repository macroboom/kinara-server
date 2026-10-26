package api

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"kinara/internal/backup"
	"kinara/internal/store"
)

// Backups: every night (unless the owner turns it off) the server writes
// a backup of its database and identity to the backups folder, keeping the
// last few. The Manage page lists them, makes one now, downloads them, and
// restores one, from the list or uploaded. Restoring stages the backup and
// restarts the server, which puts it in place before opening the database
// (see internal/backup).

// maxUpload caps an uploaded backup.
const maxUpload = 8 << 30

func (s *Server) backupRoutes(mux *http.ServeMux) {
	mux.Handle("GET /api/v1/backups", s.admin(s.listBackups))
	mux.Handle("POST /api/v1/backups", s.admin(s.createBackup))
	mux.Handle("PUT /api/v1/backups/settings", s.admin(s.putBackupSettings))
	mux.Handle("POST /api/v1/backups/restore", s.admin(s.uploadRestore))
	// A browser download can't send a header, so this one takes ?token=.
	mux.Handle("GET /api/v1/backups/{name}", s.authenticate(s.downloadBackup, true, true))
	mux.Handle("DELETE /api/v1/backups/{name}", s.admin(s.deleteBackup))
	mux.Handle("POST /api/v1/backups/{name}/restore", s.admin(s.restoreBackup))
}

// backupDir is the backups folder the settings name, else DATA/backups.
func (s *Server) backupDir(b store.BackupSettings) string {
	if b.Dir != "" {
		return b.Dir
	}
	return filepath.Join(s.DataDir, "backups")
}

type backupsView struct {
	Settings store.BackupSettings `json:"settings"`
	// Dir is the folder in use (the default when Settings.Dir is empty).
	Dir     string        `json:"dir"`
	Backups []backup.Info `json:"backups"`
	// NextAt is Unix seconds of the next nightly backup (0 when off).
	NextAt int64 `json:"nextAt"`
	// Error is why the folder couldn't be read.
	Error string `json:"error,omitempty"`
	// Restarting is set once a restore is staged.
	Restarting bool `json:"restarting,omitempty"`
}

func (s *Server) listBackups(w http.ResponseWriter, r *http.Request, u store.User) {
	if s.Backups == nil {
		writeErr(w, http.StatusNotImplemented, "backups aren't available on this server")
		return
	}
	b, err := s.Store.GetBackupSettings(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	v := backupsView{Settings: b, Dir: s.backupDir(b), Backups: []backup.Info{}, Restarting: s.restarting.Load()}
	if list, err := backup.List(v.Dir); err != nil {
		v.Error = "Couldn't read the backups folder: " + err.Error()
	} else {
		v.Backups = list
	}
	if b.Auto {
		v.NextAt = nextNightly(time.Now()).Unix()
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) createBackup(w http.ResponseWriter, r *http.Request, u store.User) {
	if s.Backups == nil {
		writeErr(w, http.StatusNotImplemented, "backups aren't available on this server")
		return
	}
	b, _ := s.Store.GetBackupSettings(r.Context())
	info, err := s.Backups.Create(r.Context(), s.backupDir(b), backup.ReasonManual)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	backup.Prune(s.backupDir(b), b.Keep, s.Log)
	writeJSON(w, http.StatusCreated, info)
}

func (s *Server) putBackupSettings(w http.ResponseWriter, r *http.Request, u store.User) {
	b, err := s.Store.GetBackupSettings(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := readJSON(r, &b); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if b.Keep < 1 || b.Keep > 365 {
		writeErr(w, http.StatusBadRequest, "keep between 1 and 365 backups")
		return
	}
	b.Dir = strings.TrimSpace(b.Dir)
	if b.Dir != "" {
		if !filepath.IsAbs(b.Dir) {
			writeErr(w, http.StatusBadRequest, "the backups folder must be a full path on the server's computer")
			return
		}
		b.Dir = filepath.Clean(b.Dir)
		if err := writable(b.Dir); err != nil {
			writeErr(w, http.StatusBadRequest, "Kinara can't write to that folder: "+err.Error())
			return
		}
	}
	if err := s.Store.PutBackupSettings(r.Context(), b); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, b)
}

// writable makes the folder if needed and checks a file can be written in it.
func writable(dir string) error {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".kinara-check-")
	if err != nil {
		return err
	}
	name := f.Name()
	f.Close()
	return os.Remove(name)
}

// backupPath resolves {name} to a backup in the folder, or answers 404.
func (s *Server) backupPath(w http.ResponseWriter, r *http.Request) (string, bool) {
	name := r.PathValue("name")
	if !backup.ValidName(name) {
		writeErr(w, http.StatusNotFound, "no such backup")
		return "", false
	}
	b, _ := s.Store.GetBackupSettings(r.Context())
	p := filepath.Join(s.backupDir(b), name)
	if _, err := os.Stat(p); err != nil {
		writeErr(w, http.StatusNotFound, "no such backup")
		return "", false
	}
	return p, true
}

func (s *Server) downloadBackup(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.backupPath(w, r)
	if !ok {
		return
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="`+filepath.Base(p)+`"`)
	w.Header().Set("Cache-Control", "no-store")
	http.ServeFile(w, r, p)
}

func (s *Server) deleteBackup(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.backupPath(w, r)
	if !ok {
		return
	}
	if err := os.Remove(p); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type restoreRequest struct {
	// Settings also restores kinara.env.
	Settings bool `json:"settings"`
}

func (s *Server) restoreBackup(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.backupPath(w, r)
	if !ok {
		return
	}
	var req restoreRequest
	if r.ContentLength != 0 {
		if err := readJSON(r, &req); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	s.stageRestore(w, r, u, p, backup.Options{Settings: req.Settings})
}

// uploadRestore takes a backup file as the request body (?settings=1 also
// restores kinara.env).
func (s *Server) uploadRestore(w http.ResponseWriter, r *http.Request, u store.User) {
	if s.Backups == nil {
		writeErr(w, http.StatusNotImplemented, "backups aren't available on this server")
		return
	}
	b, _ := s.Store.GetBackupSettings(r.Context())
	dir := s.backupDir(b)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	f, err := os.CreateTemp(dir, ".upload-*.zip")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer os.Remove(f.Name())
	_, err = io.Copy(f, http.MaxBytesReader(w, r.Body, maxUpload))
	f.Close()
	if err != nil {
		writeErr(w, http.StatusBadRequest, "upload: "+err.Error())
		return
	}
	s.stageRestore(w, r, u, f.Name(), backup.Options{Settings: r.URL.Query().Get("settings") == "1"})
}

// stageRestore checks a backup, makes a backup of what's here now, stages
// the restore and restarts the server.
func (s *Server) stageRestore(w http.ResponseWriter, r *http.Request, u store.User, p string, opts backup.Options) {
	if s.Backups == nil || s.Restart == nil {
		writeErr(w, http.StatusNotImplemented, "restoring isn't available on this server")
		return
	}
	if !s.restarting.CompareAndSwap(false, true) {
		writeErr(w, http.StatusConflict, "a restore is already under way")
		return
	}
	man, err := backup.Check(r.Context(), p, s.Version)
	if err != nil {
		s.restarting.Store(false)
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	// What's here now, so the restore can be undone from the list.
	b, _ := s.Store.GetBackupSettings(r.Context())
	if _, err := s.Backups.Create(r.Context(), s.backupDir(b), backup.ReasonBeforeRestore); err != nil {
		s.restarting.Store(false)
		writeErr(w, http.StatusInternalServerError, "couldn't back up the current library first, so nothing was restored: "+err.Error())
		return
	}
	if err := backup.Stage(s.DataDir, p, opts); err != nil {
		s.restarting.Store(false)
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.Log.Info("restore staged; restarting", "by", u.Username, "made", time.Unix(man.CreatedAt, 0).Format(time.RFC3339), "host", man.Host)
	writeJSON(w, http.StatusAccepted, map[string]any{"restarting": true, "backup": man})
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	// After the answer is out.
	go func() {
		time.Sleep(500 * time.Millisecond)
		s.Restart("restoring a backup")
	}()
}

// RunBackups makes the nightly backup at about 3:30 in the morning, and
// one shortly after starting when the last is more than a day old (the
// computer was off at night, say).
func (s *Server) RunBackups(ctx context.Context) {
	if s.Backups == nil {
		return
	}
	run := func() {
		b, err := s.Store.GetBackupSettings(ctx)
		if err != nil || !b.Auto {
			return
		}
		dir := s.backupDir(b)
		if _, err := s.Backups.Create(ctx, dir, backup.ReasonScheduled); err != nil {
			s.Log.Warn("nightly backup failed", "dir", dir, "err", err)
			return
		}
		backup.Prune(dir, b.Keep, s.Log)
	}
	first := 10 * time.Minute
	if b, err := s.Store.GetBackupSettings(ctx); err == nil {
		if list, err := backup.List(s.backupDir(b)); err == nil && len(list) > 0 &&
			time.Since(time.Unix(list[0].CreatedAt, 0)) < 26*time.Hour {
			first = time.Until(nextNightly(time.Now()))
		}
	}
	wait := first
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		run()
		wait = time.Until(nextNightly(time.Now()))
	}
}

// nextNightly is the next 3:30 in the morning, local time.
func nextNightly(now time.Time) time.Time {
	t := time.Date(now.Year(), now.Month(), now.Day(), 3, 30, 0, 0, now.Location())
	if !t.After(now) {
		t = t.AddDate(0, 0, 1)
	}
	return t
}

// BackupBeforeUpdate is for the updater: a backup right before installing.
func (s *Server) BackupBeforeUpdate(ctx context.Context) error {
	if s.Backups == nil {
		return nil
	}
	b, _ := s.Store.GetBackupSettings(ctx)
	_, err := s.Backups.Create(ctx, s.backupDir(b), backup.ReasonBeforeUpdate)
	if err != nil {
		return fmt.Errorf("backup before updating: %w", err)
	}
	return nil
}
