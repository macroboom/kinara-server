//go:build darwin

package update

import (
	"context"
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
// server; the package is deleted when it ends, whatever happened.
func launch(kind, path, version, dir string, ended func(error)) error {
	script := fmt.Sprintf(`set p to %s
do shell script "/usr/sbin/installer -pkg " & quoted form of p & " -target / ; rc=$? ; /bin/rm -f " & quoted form of p & " ; exit $rc" with prompt %s with administrator privileges`,
		appleString(path), appleString("Kinara is updating to version "+version+"."))
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

// appleString quotes s as an AppleScript string.
func appleString(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}
