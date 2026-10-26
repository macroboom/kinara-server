//go:build windows

package update

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"syscall"
	"unsafe"
)

func canLaunch(kind string) bool { return kind == KindWindows }

// verify has nothing to add on Windows: the installer isn't code signed
// (yet), so the size and checksum from latest.json, fetched over HTTPS,
// are the check.
func verify(ctx context.Context, kind, path string) error { return nil }

const (
	detachedProcess       = 0x00000008
	createNewProcessGroup = 0x00000200
	errElevationRequired  = syscall.Errno(740)
	errCancelled          = syscall.Errno(1223)
)

// launch starts the Kinara installer silently. It stops the server (and
// the tray), installs over the same folders and settings, and starts the
// new server; /AUTOUPDATE=1 makes it also start the tray again in sign-in
// mode. A server running as the Windows service can start it directly; one
// running as the signed-in user (sign-in mode) asks Windows for permission,
// which shows the usual admin prompt on that PC.
func launch(kind, path, version, dir string, ended func(error)) error {
	args := []string{"/VERYSILENT", "/SUPPRESSMSGBOXES", "/NORESTART", "/SP-", "/AUTOUPDATE=1",
		"/LOG=" + filepath.Join(dir, "install.log")}
	cmd := exec.Command(path, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: detachedProcess | createNewProcessGroup}
	err := cmd.Start()
	if err == nil {
		go func() { ended(exitErr(cmd.Wait())) }()
		return nil
	}
	if !errors.Is(err, errElevationRequired) {
		return fmt.Errorf("couldn't start the installer: %w", err)
	}
	params := ""
	for _, a := range args {
		params += syscall.EscapeArg(a) + " "
	}
	proc, err := runElevated(path, params)
	if errors.Is(err, errCancelled) {
		return errors.New("the update needs permission to install, and the Windows prompt was declined")
	}
	if err != nil {
		return fmt.Errorf("couldn't start the installer: %w", err)
	}
	go func() {
		defer syscall.CloseHandle(proc)
		_, _ = syscall.WaitForSingleObject(proc, syscall.INFINITE)
		var code uint32
		_ = syscall.GetExitCodeProcess(proc, &code)
		if code != 0 {
			ended(fmt.Errorf("the installer stopped with code %d (see install.log in the updates folder)", code))
			return
		}
		ended(nil)
	}()
	return nil
}

func exitErr(err error) error {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return fmt.Errorf("the installer stopped with code %d (see install.log in the updates folder)", ee.ExitCode())
	}
	return err
}

var (
	shell32            = syscall.NewLazyDLL("shell32.dll")
	procShellExecuteEx = shell32.NewProc("ShellExecuteExW")
)

// shellExecuteInfo is SHELLEXECUTEINFOW.
type shellExecuteInfo struct {
	size        uint32
	mask        uint32
	hwnd        uintptr
	verb        *uint16
	file        *uint16
	parameters  *uint16
	directory   *uint16
	show        int32
	instApp     uintptr
	idList      uintptr
	class       *uint16
	keyClass    uintptr
	hotKey      uint32
	iconMonitor uintptr
	process     syscall.Handle
}

const (
	seeMaskNoCloseProcess = 0x00000040
	seeMaskNoAsync        = 0x00000100
)

// runElevated starts a program with "Run as administrator" and returns
// its process handle.
func runElevated(path, params string) (syscall.Handle, error) {
	verb, _ := syscall.UTF16PtrFromString("runas")
	file, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	p, err := syscall.UTF16PtrFromString(params)
	if err != nil {
		return 0, err
	}
	info := shellExecuteInfo{mask: seeMaskNoCloseProcess | seeMaskNoAsync, verb: verb, file: file, parameters: p}
	info.size = uint32(unsafe.Sizeof(info))
	r, _, callErr := procShellExecuteEx.Call(uintptr(unsafe.Pointer(&info)))
	if r == 0 {
		if errno, ok := callErr.(syscall.Errno); ok {
			return 0, errno
		}
		return 0, callErr
	}
	if info.process == 0 {
		return 0, errors.New("Windows gave no process for the installer")
	}
	return info.process, nil
}
