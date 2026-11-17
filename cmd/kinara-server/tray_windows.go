//go:build windows

package main

// Starting the tray icon again after an automatic update, in service mode.
//
// The update runs the installer from the service, in session 0, where
// nobody signs in. The installer stops every tray icon so it can replace
// kinara-tray.exe, but a program it starts again lands in session 0 as the
// service's account, where no one can see it. So the installer leaves a
// note (DATA\relaunch-tray) instead, and the new service, once it starts,
// opens the tray in each signed-in session as that session's user. This
// needs the service to run as Local System (the installer's default); as
// Network Service it can't, and the tray comes back at the next sign-in.

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"unsafe"
)

var (
	wtsapi32                   = syscall.NewLazyDLL("wtsapi32.dll")
	userenv                    = syscall.NewLazyDLL("userenv.dll")
	procWTSEnumerateSessions   = wtsapi32.NewProc("WTSEnumerateSessionsW")
	procWTSQueryUserToken      = wtsapi32.NewProc("WTSQueryUserToken")
	procWTSFreeMemory          = wtsapi32.NewProc("WTSFreeMemory")
	procCreateEnvironmentBlock = userenv.NewProc("CreateEnvironmentBlock")
	procDestroyEnvironmentBlk  = userenv.NewProc("DestroyEnvironmentBlock")
)

const (
	wtsActive                = 0
	createUnicodeEnvironment = 0x400
)

type wtsSessionInfo struct {
	SessionID  uint32
	WinStation *uint16
	State      uint32
}

// relaunchTray starts the tray in every signed-in session if the installer
// asked for it.
func relaunchTray() {
	data := os.Getenv("KINARA_DATA")
	if data == "" {
		data = filepath.Join(os.Getenv("ProgramData"), "Kinara")
	}
	note := filepath.Join(data, "relaunch-tray")
	if _, err := os.Stat(note); err != nil {
		return
	}
	_ = os.Remove(note)
	exe, err := os.Executable()
	if err != nil {
		return
	}
	tray := filepath.Join(filepath.Dir(exe), "kinara-tray.exe")
	if _, err := os.Stat(tray); err != nil {
		return
	}
	var info *wtsSessionInfo
	var count uint32
	r, _, err := procWTSEnumerateSessions.Call(0, 0, 1, uintptr(unsafe.Pointer(&info)), uintptr(unsafe.Pointer(&count)))
	if r == 0 {
		fmt.Fprintln(os.Stderr, "tray: listing sessions:", err)
		return
	}
	defer procWTSFreeMemory.Call(uintptr(unsafe.Pointer(info)))
	sessions := unsafe.Slice(info, count)
	for _, s := range sessions {
		if s.State != wtsActive || s.SessionID == 0 {
			continue
		}
		if err := startTrayIn(s.SessionID, tray); err != nil {
			fmt.Fprintf(os.Stderr, "tray: session %d: %v (it starts at the next sign-in)\n", s.SessionID, err)
		}
	}
}

func startTrayIn(session uint32, tray string) error {
	var tok syscall.Token
	if r, _, err := procWTSQueryUserToken.Call(uintptr(session), uintptr(unsafe.Pointer(&tok))); r == 0 {
		return fmt.Errorf("signed-in user's token: %w", err)
	}
	defer tok.Close()
	var env *uint16
	if r, _, _ := procCreateEnvironmentBlock.Call(uintptr(unsafe.Pointer(&env)), uintptr(tok), 0); r == 0 {
		env = nil // the service's own environment, then
	} else {
		defer procDestroyEnvironmentBlk.Call(uintptr(unsafe.Pointer(env)))
	}
	desktop, _ := syscall.UTF16PtrFromString(`winsta0\default`)
	si := syscall.StartupInfo{Desktop: desktop}
	si.Cb = uint32(unsafe.Sizeof(si))
	var pi syscall.ProcessInformation
	app, _ := syscall.UTF16PtrFromString(tray)
	cmd, _ := syscall.UTF16PtrFromString(syscall.EscapeArg(tray))
	dir, _ := syscall.UTF16PtrFromString(filepath.Dir(tray))
	flags := uint32(0)
	if env != nil {
		flags |= createUnicodeEnvironment
	}
	if err := syscall.CreateProcessAsUser(tok, app, cmd, nil, nil, false, flags, env, dir, &si, &pi); err != nil {
		return err
	}
	syscall.CloseHandle(pi.Thread)
	syscall.CloseHandle(pi.Process)
	fmt.Fprintf(os.Stderr, "tray: started again in session %d\n", session)
	return nil
}
