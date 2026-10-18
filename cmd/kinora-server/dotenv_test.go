package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseDotEnvLine(t *testing.T) {
	cases := []struct {
		in, key, value string
		ok, bad        bool
	}{
		{in: "", ok: false},
		{in: "# comment", ok: false},
		{in: "KINORA_TMDB_KEY=eyJabc.def", key: "KINORA_TMDB_KEY", value: "eyJabc.def", ok: true},
		{in: "export KINORA_LISTEN=:9000", key: "KINORA_LISTEN", value: ":9000", ok: true},
		{in: `KINORA_DATA="/srv/kinora data"`, key: "KINORA_DATA", value: "/srv/kinora data", ok: true},
		{in: "A='single # kept'", key: "A", value: "single # kept", ok: true},
		{in: "B=value # note", key: "B", value: "value", ok: true},
		{in: "C=a#b", key: "C", value: "a#b", ok: true},
		{in: " D = spaced ", key: "D", value: "spaced", ok: true},
		{in: "E=", key: "E", value: "", ok: true},
		{in: "no equals sign", bad: true},
		{in: "BAD KEY=1", bad: true},
	}
	for _, c := range cases {
		k, v, ok, err := parseDotEnvLine(c.in)
		if c.bad {
			if err == nil {
				t.Errorf("%q: want error", c.in)
			}
			continue
		}
		if err != nil || ok != c.ok || k != c.key || v != c.value {
			t.Errorf("%q: got %q=%q ok=%v err=%v", c.in, k, v, ok, err)
		}
	}
}

func TestLoadDotEnv(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	os.WriteFile(path, []byte("KINORA_TEST_FROM_FILE=file\nKINORA_TEST_ALREADY_SET=file\n"), 0o600)
	t.Setenv("KINORA_TEST_ALREADY_SET", "real")
	t.Setenv("KINORA_TEST_FROM_FILE", "")
	os.Unsetenv("KINORA_TEST_FROM_FILE")

	if err := loadDotEnv(path); err != nil {
		t.Fatal(err)
	}
	if got := os.Getenv("KINORA_TEST_FROM_FILE"); got != "file" {
		t.Errorf("from file = %q", got)
	}
	if got := os.Getenv("KINORA_TEST_ALREADY_SET"); got != "real" {
		t.Errorf("real environment should win, got %q", got)
	}
	if err := loadDotEnv(filepath.Join(t.TempDir(), "missing.env")); err != nil {
		t.Errorf("missing file should be fine: %v", err)
	}
}

func TestLoadDotEnvSkipsBOM(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kinara.env")
	os.WriteFile(path, []byte("\uFEFFKINORA_TEST_BOM=C:\\Données\\Kinara\n"), 0o600)
	t.Setenv("KINORA_TEST_BOM", "")
	os.Unsetenv("KINORA_TEST_BOM")
	if err := loadDotEnv(path); err != nil {
		t.Fatal(err)
	}
	if got := os.Getenv("KINORA_TEST_BOM"); got != `C:\Données\Kinara` {
		t.Errorf("got %q", got)
	}
}
