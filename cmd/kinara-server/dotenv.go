package main

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
)

// loadEnvFile loads the file KINARA_ENV_FILE names, or ./.env when it isn't
// set. The default .env is a convenience for running from a source folder,
// so one this user can't read (someone else's, as when kinara-server is run
// by hand from inside another account's folder) is skipped with a note
// instead of stopping the command. A file named in KINARA_ENV_FILE must be
// readable.
func loadEnvFile() error {
	if path := os.Getenv("KINARA_ENV_FILE"); path != "" {
		return loadDotEnv(path)
	}
	err := loadDotEnv(".env")
	if errors.Is(err, fs.ErrPermission) {
		fmt.Fprintf(os.Stderr, "note: skipped .env in this folder: %v\n", err)
		return nil
	}
	return err
}

// loadDotEnv reads KEY=VALUE lines from path into the environment. A missing
// file is fine. Variables already set in the real environment win, so
// `KINARA_LISTEN=:9000 kinara-server` still overrides the file.
//
// Supported: blank lines, # comments, an optional "export " prefix, and
// values in single or double quotes. Inline comments need a space before #
// on unquoted values (KEY=value # note).
func loadDotEnv(path string) error {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	line := 0
	for sc.Scan() {
		line++
		key, value, ok, err := parseDotEnvLine(sc.Text())
		if err != nil {
			return fmt.Errorf("%s line %d: %w", path, line, err)
		}
		if !ok {
			continue
		}
		if _, set := os.LookupEnv(key); set {
			continue
		}
		if err := os.Setenv(key, value); err != nil {
			return fmt.Errorf("%s line %d: %w", path, line, err)
		}
	}
	return sc.Err()
}

func parseDotEnvLine(s string) (key, value string, ok bool, err error) {
	s = strings.TrimSpace(strings.TrimPrefix(s, "\uFEFF"))
	if s == "" || strings.HasPrefix(s, "#") {
		return "", "", false, nil
	}
	s = strings.TrimPrefix(s, "export ")
	key, value, found := strings.Cut(s, "=")
	if !found {
		return "", "", false, errors.New("expected KEY=VALUE")
	}
	key = strings.TrimSpace(key)
	if key == "" || strings.ContainsAny(key, " \t") {
		return "", "", false, fmt.Errorf("invalid name %q", key)
	}
	value = strings.TrimSpace(value)
	switch {
	case len(value) >= 2 && value[0] == '"' && value[len(value)-1] == '"':
		value = strings.NewReplacer(`\n`, "\n", `\"`, `"`, `\\`, `\`).Replace(value[1 : len(value)-1])
	case len(value) >= 2 && value[0] == '\'' && value[len(value)-1] == '\'':
		value = value[1 : len(value)-1]
	default:
		if i := strings.Index(value, " #"); i >= 0 {
			value = strings.TrimSpace(value[:i])
		}
	}
	return key, value, true, nil
}
