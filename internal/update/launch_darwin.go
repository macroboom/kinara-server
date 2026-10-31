//go:build darwin

package update

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

func canLaunch(kind string) bool { return kind == KindMacOS }

// teamID is Macroboom LLC's Apple developer team, which signs Kinara.pkg.
const teamID = "3ZSCTM7V92"

// verify checks the package is signed by Macroboom LLC and that macOS
// accepts it (signed with a Developer ID and notarized by Apple).
func verify(ctx context.Context, kind, path string) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/usr/sbin/pkgutil", "--check-signature", path).CombinedOutput()
	if err != nil || !strings.Contains(string(out), "Developer ID Installer: Macroboom LLC ("+teamID+")") {
		return errors.New("the downloaded package isn't signed by Macroboom LLC, so it wasn't installed")
	}
	out, err = exec.CommandContext(ctx, "/usr/sbin/spctl", "--assess", "--type", "install", "-vv", path).CombinedOutput()
	if err != nil {
		return fmt.Errorf("macOS doesn't accept the downloaded package: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

// launch installs the package with macOS's own password prompt (Kinara
// runs as you, and installing needs an administrator). The package's
// postinstall restarts the server and the menu bar item. The installer runs
// in a session of its own, so it keeps going when launchd stops this
// server.
//
// The downloaded file sits in a folder this account can write, so the
// script that runs as root first copies it into a folder of its own that
// only root can change, checks that copy (checksum, Macroboom's signature,
// notarization) and installs that copy, not the original. Both are deleted
// when it ends, whatever happened.
func launch(kind, path, version, sha, dir string, ended func(error)) error {
	if _, err := hex.DecodeString(sha); err != nil || len(sha) != 64 {
		return errors.New("the release list gives no usable checksum")
	}
	sh := `d=$(/usr/bin/mktemp -d /private/tmp/kinara-update.XXXXXX) || exit 1
p="$d/Kinara.pkg"
fail() { /bin/rm -rf "$d"; echo "$1" >&2; exit 1; }
/bin/cp -X ` + shQuote(path) + ` "$p" || fail "couldn't copy the package"
/bin/rm -f ` + shQuote(path) + `
[ "$(/usr/bin/shasum -a 256 "$p" | /usr/bin/cut -d' ' -f1)" = "` + strings.ToLower(sha) + `" ] || fail "the package changed after it was downloaded"
/usr/sbin/pkgutil --check-signature "$p" | /usr/bin/grep -qF "Developer ID Installer: Macroboom LLC (` + teamID + `)" || fail "the package isn't signed by Macroboom LLC"
/usr/sbin/spctl --assess --type install "$p" || fail "macOS doesn't accept the package"
/usr/sbin/installer -pkg "$p" -target /
rc=$?
/bin/rm -rf "$d"
exit $rc`
	script := fmt.Sprintf(`do shell script %s with prompt %s with administrator privileges`,
		appleString(sh), appleString("Kinara is updating to version "+version+"."))
	cmd := exec.Command("/usr/bin/osascript", "-e", script)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	var out strings.Builder
	cmd.Stderr = &out
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("couldn't start the installer: %w", err)
	}
	go func() {
		err := cmd.Wait()
		if err == nil {
			ended(nil)
			return
		}
		msg := strings.TrimSpace(out.String())
		if strings.Contains(msg, "-128") || strings.Contains(strings.ToLower(msg), "cancel") {
			ended(errors.New("the update was cancelled at the password prompt"))
			return
		}
		if msg == "" {
			msg = err.Error()
		}
		ended(errors.New("the installer failed: " + msg))
	}()
	return nil
}

// shQuote quotes s for the shell.
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// appleString quotes s as an AppleScript string.
func appleString(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(s) + `"`
}
