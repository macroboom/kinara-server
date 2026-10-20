//go:build windows

package main

// Running as a Windows service. The installer registers
//
//	"C:\Program Files\Kinara\kinara-server.exe" service
//
// as the "Kinara" service; Windows starts it at boot, before anyone signs
// in, and it keeps running when people sign out. This talks to the service
// control manager directly through advapi32 (no outside packages), the
// same way golang.org/x/sys/windows/svc does underneath.
//
// A service has no console, so everything the server logs goes to
// DATA\logs\server.log (KINARA_LOG to put it elsewhere). The file is
// started afresh, keeping the previous one as server.log.old, once it
// passes 10 MB.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"unsafe"
)

const serviceName = "Kinara"

var (
	advapi32                       = syscall.NewLazyDLL("advapi32.dll")
	kernel32                       = syscall.NewLazyDLL("kernel32.dll")
	procStartServiceCtrlDispatcher = advapi32.NewProc("StartServiceCtrlDispatcherW")
	procRegisterServiceCtrlHandler = advapi32.NewProc("RegisterServiceCtrlHandlerExW")
	procSetServiceStatus           = advapi32.NewProc("SetServiceStatus")
	procSetStdHandle               = kernel32.NewProc("SetStdHandle")
)

const (
	serviceWin32OwnProcess = 0x10

	stateStopped      = 1
	stateStartPending = 2
	stateStopPending  = 3
	stateRunning      = 4

	controlStop        = 1
	controlInterrogate = 4
	controlShutdown    = 5

	acceptStop     = 0x1
	acceptShutdown = 0x4

	errCallNotImplemented          = 120
	errServiceSpecific             = 1066
	errFailedServiceControllerConn = 1063

	stdErrorHandle  = ^uintptr(12) + 1 // STD_ERROR_HANDLE (-12)
	stdOutputHandle = ^uintptr(11) + 1 // STD_OUTPUT_HANDLE (-11)
)

// serviceStatus is SERVICE_STATUS.
type serviceStatus struct {
	ServiceType             uint32
	CurrentState            uint32
	ControlsAccepted        uint32
	Win32ExitCode           uint32
	ServiceSpecificExitCode uint32
	CheckPoint              uint32
	WaitHint                uint32
}

// serviceTableEntry is SERVICE_TABLE_ENTRYW.
type serviceTableEntry struct {
	name *uint16
	proc uintptr
}

var svc struct {
	mu     sync.Mutex
	handle uintptr
	args   []string
	cancel context.CancelFunc
	check  uint32
}

// runService hands the process to the service control manager. It returns
// when the service has stopped.
func runService(args []string) error {
	svc.args = args
	if err := logToFile(); err != nil {
		return err
	}
	name, _ := syscall.UTF16PtrFromString(serviceName)
	table := []serviceTableEntry{
		{name: name, proc: syscall.NewCallback(serviceMain)},
		{}, // the table ends with an empty entry
	}
	r, _, err := procStartServiceCtrlDispatcher.Call(uintptr(unsafe.Pointer(&table[0])))
	if r == 0 {
		if errno, ok := err.(syscall.Errno); ok && errno == errFailedServiceControllerConn {
			return errors.New("this is the Windows service entry point; to run Kinara in a window, use: kinara-server serve")
		}
		return fmt.Errorf("starting the service: %w", err)
	}
	return nil
}

// serviceMain is LPSERVICE_MAIN_FUNCTIONW, called on a thread the service
// manager makes. It runs the server and reports its state.
func serviceMain(argc uint32, argv **uint16) uintptr {
	name, _ := syscall.UTF16PtrFromString(serviceName)
	h, _, _ := procRegisterServiceCtrlHandler.Call(uintptr(unsafe.Pointer(name)), syscall.NewCallback(serviceHandler), 0)
	if h == 0 {
		return 0
	}
	ctx, cancel := context.WithCancel(context.Background())
	svc.mu.Lock()
	svc.handle = h
	svc.cancel = cancel
	svc.mu.Unlock()

	setServiceState(stateStartPending, 0, 0, 10000)
	done := make(chan error, 1)
	go func() { done <- serveUntil(ctx, svc.args) }()
	setServiceState(stateRunning, acceptStop|acceptShutdown, 0, 0)

	err := <-done
	cancel()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		// A non-zero code lets the restart-on-failure setting kick in.
		setServiceState(stateStopped, 0, 1, 0)
		return 0
	}
	setServiceState(stateStopped, 0, 0, 0)
	return 0
}

// serviceHandler is LPHANDLER_FUNCTION_EX: stop and shutdown requests.
func serviceHandler(control, eventType, eventData, context uintptr) uintptr {
	switch control {
	case controlStop, controlShutdown:
		// Up to 30 s to finish: open streams end, the database closes.
		setServiceState(stateStopPending, 0, 0, 30000)
		svc.mu.Lock()
		cancel := svc.cancel
		svc.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		return 0
	case controlInterrogate:
		return 0
	}
	return errCallNotImplemented
}

func setServiceState(state, accepts, exitCode, waitHint uint32) {
	svc.mu.Lock()
	defer svc.mu.Unlock()
	if svc.handle == 0 {
		return
	}
	st := serviceStatus{ServiceType: serviceWin32OwnProcess, CurrentState: state, ControlsAccepted: accepts, WaitHint: waitHint}
	if exitCode != 0 {
		st.Win32ExitCode = errServiceSpecific
		st.ServiceSpecificExitCode = exitCode
	}
	if state == stateStartPending || state == stateStopPending {
		svc.check++
		st.CheckPoint = svc.check
	}
	procSetServiceStatus.Call(svc.handle, uintptr(unsafe.Pointer(&st)))
}

// logToFile points stdout and stderr (and so the server's log, and any crash
// report) at the log file, since a service has no console.
func logToFile() error {
	path := os.Getenv("KINARA_LOG")
	if path == "" {
		data := os.Getenv("KINARA_DATA")
		if data == "" {
			exe, _ := os.Executable()
			data = filepath.Join(filepath.Dir(exe), "kinara-data")
		}
		path = filepath.Join(data, "logs", "server.log")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if fi, err := os.Stat(path); err == nil && fi.Size() > 10<<20 {
		_ = os.Rename(path, path+".old")
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	os.Stderr = f
	os.Stdout = f
	procSetStdHandle.Call(stdErrorHandle, f.Fd())
	procSetStdHandle.Call(stdOutputHandle, f.Fd())
	return nil
}
