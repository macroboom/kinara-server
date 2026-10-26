package transcode

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"time"
	"unicode/utf8"

	"kinara/internal/ffmpeg"
)

// TextSubtitle reports whether a subtitle codec is text that converts to
// WebVTT. Picture subtitles (Blu-ray PGS, DVD) can't be.
func TextSubtitle(codec string) bool {
	switch codec {
	case "subrip", "srt", "ass", "ssa", "mov_text", "webvtt", "text":
		return true
	}
	return false
}

// Subtitles turns a file's text subtitle tracks into WebVTT, which web
// browsers and phone players show. Each track is converted once and kept.
type Subtitles struct {
	Tool *ffmpeg.Tool
	Dir  string

	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

// ErrNotText is returned for a picture subtitle track.
var ErrNotText = errors.New("that subtitle track is pictures, not text, so it can't be shown here")

// VTT returns the WebVTT for subtitle stream index of the file at path (a
// version's ID names the cache; mtime invalidates it when the file changes).
func (s *Subtitles) VTT(ctx context.Context, versionID, path string, mtime int64, index int, codec string) ([]byte, error) {
	if !TextSubtitle(codec) {
		return nil, ErrNotText
	}
	key := fmt.Sprintf("%s-%d-%d", versionID, index, mtime)
	out := filepath.Join(s.Dir, key+".vtt")
	lock := s.lock(key)
	lock.Lock()
	defer lock.Unlock()
	if b, err := os.ReadFile(out); err == nil {
		return b, nil
	}
	if !s.Tool.Caps().Found {
		return nil, errors.New("ffmpeg was not found")
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, s.Tool.Path(), "-nostdin", "-hide_banner", "-loglevel", "error",
		"-i", path, "-map", "0:"+strconv.Itoa(index), "-f", "webvtt", "pipe:1")
	ffmpeg.HideWindow(cmd)
	var stdout bytes.Buffer
	tail := &ffmpeg.Tail{N: 1024}
	cmd.Stdout, cmd.Stderr = &stdout, tail
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("ffmpeg: %v %s", err, tail.String())
	}
	if err := os.MkdirAll(s.Dir, 0o755); err == nil {
		_ = os.WriteFile(out, stdout.Bytes(), 0o644)
	}
	return stdout.Bytes(), nil
}

// VTTFile returns the WebVTT for a subtitle file of its own (an .srt next
// to a video, or one downloaded). key names the cache; mtime invalidates
// it. A file that isn't UTF-8 is read as Windows-1252, which most older
// subtitle files in Western languages are.
func (s *Subtitles) VTTFile(ctx context.Context, key, path string, mtime int64) ([]byte, error) {
	out := filepath.Join(s.Dir, fmt.Sprintf("%s-%d.vtt", key, mtime))
	lock := s.lock(key)
	lock.Lock()
	defer lock.Unlock()
	if b, err := os.ReadFile(out); err == nil {
		return b, nil
	}
	if !s.Tool.Caps().Found {
		return nil, errors.New("ffmpeg was not found")
	}
	args := []string{"-nostdin", "-hide_banner", "-loglevel", "error"}
	if head, err := readHead(path, 1<<20); err == nil && !utf8.Valid(trimPartialRune(head)) {
		args = append(args, "-sub_charenc", "CP1252")
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	args = append(args, "-i", path, "-map", "0:0", "-f", "webvtt", "pipe:1")
	cmd := exec.CommandContext(ctx, s.Tool.Path(), args...)
	ffmpeg.HideWindow(cmd)
	var stdout bytes.Buffer
	tail := &ffmpeg.Tail{N: 1024}
	cmd.Stdout, cmd.Stderr = &stdout, tail
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("ffmpeg: %v %s", err, tail.String())
	}
	if err := os.MkdirAll(s.Dir, 0o755); err == nil {
		_ = os.WriteFile(out, stdout.Bytes(), 0o644)
	}
	return stdout.Bytes(), nil
}

func readHead(path string, n int) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	buf := make([]byte, n)
	m, err := io.ReadFull(f, buf)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return buf[:m], nil
}

// trimPartialRune drops a UTF-8 sequence cut off at the end of a read.
func trimPartialRune(b []byte) []byte {
	for i := 0; i < 3 && len(b) > 0; i++ {
		if r, _ := utf8.DecodeLastRune(b); r != utf8.RuneError {
			return b
		}
		b = b[:len(b)-1]
	}
	return b
}

func (s *Subtitles) lock(key string) *sync.Mutex {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.locks == nil {
		s.locks = map[string]*sync.Mutex{}
	}
	l, ok := s.locks[key]
	if !ok {
		l = &sync.Mutex{}
		s.locks[key] = l
	}
	return l
}
