//go:build windows

// kinara-tray is the Kinara icon in the Windows notification area. It shows
// whether the server is up and gives quick access to it:
//
//	Kinara is running            (status, not clickable)
//	Open Kinara                  opens the web app in the browser
//	Start / Stop / Restart server
//	Open data folder, Open logs
//	Quit
//
// How the server runs is chosen in the installer and stored in
// HKLM\Software\Kinara (Mode):
//
//	service  the "Kinara" Windows service runs the server, before anyone signs
//	         in and after they sign out. The tray only watches and controls
//	         it; quitting the tray leaves the server running.
//	signin   the tray starts the server itself, hidden, when you sign in, and
//	         stops it when you quit the tray or sign out.
//
// Plain Win32 through syscall, no outside packages, no console window
// (built with -ldflags -H=windowsgui).
package main

import (
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"
)

func init() { runtime.LockOSThread() } // the window lives on the main thread

var (
	user32   = syscall.NewLazyDLL("user32.dll")
	shell32  = syscall.NewLazyDLL("shell32.dll")
	kernel32 = syscall.NewLazyDLL("kernel32.dll")
	advapi32 = syscall.NewLazyDLL("advapi32.dll")

	pRegisterClassEx       = user32.NewProc("RegisterClassExW")
	pCreateWindowEx        = user32.NewProc("CreateWindowExW")
	pDefWindowProc         = user32.NewProc("DefWindowProcW")
	pGetMessage            = user32.NewProc("GetMessageW")
	pTranslateMessage      = user32.NewProc("TranslateMessage")
	pDispatchMessage       = user32.NewProc("DispatchMessageW")
	pPostMessage           = user32.NewProc("PostMessageW")
	pPostQuitMessage       = user32.NewProc("PostQuitMessage")
	pRegisterWindowMessage = user32.NewProc("RegisterWindowMessageW")
	pLoadImage             = user32.NewProc("LoadImageW")
	pGetSystemMetrics      = user32.NewProc("GetSystemMetrics")
	pCreatePopupMenu       = user32.NewProc("CreatePopupMenu")
	pAppendMenu            = user32.NewProc("AppendMenuW")
	pDestroyMenu           = user32.NewProc("DestroyMenu")
	pTrackPopupMenu        = user32.NewProc("TrackPopupMenu")
	pSetForegroundWindow   = user32.NewProc("SetForegroundWindow")
	pGetCursorPos          = user32.NewProc("GetCursorPos")
	pMessageBox            = user32.NewProc("MessageBoxW")
	pShellNotifyIcon       = shell32.NewProc("Shell_NotifyIconW")
	pShellExecute          = shell32.NewProc("ShellExecuteW")
	pGetModuleHandle       = kernel32.NewProc("GetModuleHandleW")
	pCreateMutex           = kernel32.NewProc("CreateMutexW")
	pRegGetValue           = advapi32.NewProc("RegGetValueW")
	pOpenSCManager         = advapi32.NewProc("OpenSCManagerW")
	pOpenService           = advapi32.NewProc("OpenServiceW")
	pStartService          = advapi32.NewProc("StartServiceW")
	pControlService        = advapi32.NewProc("ControlService")
	pQueryServiceStatus    = advapi32.NewProc("QueryServiceStatus")
	pCloseServiceHandle    = advapi32.NewProc("CloseServiceHandle")
)

const (
	wmDestroy     = 0x0002
	wmCommand     = 0x0111
	wmNull        = 0x0000
	wmLButtonUp   = 0x0202
	wmLButtonDbl  = 0x0203
	wmRButtonUp   = 0x0205
	wmContextMenu = 0x007B
	wmApp         = 0x8000
	wmTray        = wmApp + 1 // icon clicks
	wmStatus      = wmApp + 2 // the watcher saw a change

	nimAdd    = 0
	nimModify = 1
	nimDelete = 2
	nifMsg    = 0x1
	nifIcon   = 0x2
	nifTip    = 0x4
	nifInfo   = 0x10
	niifInfo  = 0x1
	niifWarn  = 0x2

	mfString    = 0x0
	mfGrayed    = 0x1
	mfSeparator = 0x800
	tpmReturn   = 0x0100
	tpmRight    = 0x0002

	imageIcon        = 1
	lrLoadFromFile   = 0x10
	smCxSmIcon       = 49
	smCySmIcon       = 50
	errAlreadyExists = 183

	hkeyLocalMachine = 0x80000002
	rrfRtRegSz       = 0x2

	scManagerConnect    = 0x0001
	serviceQueryStatus  = 0x0004
	serviceStart        = 0x0010
	serviceStop         = 0x0020
	serviceControlStop  = 1
	svcStopped          = 1
	svcRunning          = 4
	errAccessDenied     = 5
	errServiceNotActive = 1062
	serviceName         = "Kinara"
)

// Menu command IDs.
const (
	cmdOpen = iota + 1
	cmdStart
	cmdStop
	cmdRestart
	cmdData
	cmdLogs
	cmdQuit
)

type point struct{ X, Y int32 }

type msg struct {
	HWnd    uintptr
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	Pt      point
	_       uint32
}

type wndClassEx struct {
	Size       uint32
	Style      uint32
	WndProc    uintptr
	ClsExtra   int32
	WndExtra   int32
	Instance   uintptr
	Icon       uintptr
	Cursor     uintptr
	Background uintptr
	MenuName   *uint16
	ClassName  *uint16
	IconSm     uintptr
}

type notifyIconData struct {
	Size            uint32
	HWnd            uintptr
	ID              uint32
	Flags           uint32
	CallbackMessage uint32
	Icon            uintptr
	Tip             [128]uint16
	State           uint32
	StateMask       uint32
	Info            [256]uint16
	Version         uint32
	InfoTitle       [64]uint16
	InfoFlags       uint32
	GUIDItem        [16]byte
	BalloonIcon     uintptr
}

type serviceStatus struct {
	ServiceType, CurrentState, ControlsAccepted, Win32ExitCode, ServiceSpecificExitCode, CheckPoint, WaitHint uint32
}

// Status the watcher derives every few seconds.
const (
	stUnknown int32 = iota
	stRunning
	stStarting
	stStopping
	stStopped
)

var app struct {
	hwnd     uintptr
	icon     uintptr
	taskbar  uint32 // "TaskbarCreated": Explorer restarted, add the icon again
	mode     string // "service" or "signin"
	dataDir  string
	cacheDir string
	exeDir   string
	port     string
	status   atomic.Int32
	wanted   atomic.Bool // signin mode: we want the server up
	child    struct {
		sync.Mutex
		cmd   *exec.Cmd
		stdin interface{ Close() error }
	}
}

func main() {
	exe, _ := os.Executable()
	app.exeDir = filepath.Dir(exe)
	app.mode = regString("Mode", "service")
	app.dataDir = regString("DataDir", filepath.Join(os.Getenv("ProgramData"), "Kinara"))
	app.cacheDir = regString("CacheDir", filepath.Join(app.dataDir, "cache"))
	app.port = listenPort(filepath.Join(app.dataDir, "kinara.env"))

	// --open (Start menu, desktop, end of setup): open Kinara in the browser
	// once the server answers. It only stays as the tray icon when nothing
	// else runs the server (sign-in mode with no tray running yet).
	open := len(os.Args) > 1 && os.Args[1] == "--open"
	name, _ := syscall.UTF16PtrFromString(`Local\KinaraTray`)
	_, _, err := pCreateMutex.Call(0, 0, uintptr(unsafe.Pointer(name)))
	already := err == syscall.Errno(errAlreadyExists)
	if open && (already || app.mode == "service") {
		openWhenUp()
		return
	}
	if already {
		return // already in the tray
	}
	if open {
		go openWhenUp()
	}

	if err := makeWindow(); err != nil {
		alert("Kinara could not start its tray icon: " + err.Error())
		return
	}
	addIcon()
	if app.mode == "signin" {
		app.wanted.Store(true)
		startChild()
	}
	go watch()

	var m msg
	for {
		r, _, _ := pGetMessage.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if int32(r) <= 0 {
			break
		}
		pTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		pDispatchMessage.Call(uintptr(unsafe.Pointer(&m)))
	}
	removeIcon()
	stopChild()
}

func makeWindow() error {
	inst, _, _ := pGetModuleHandle.Call(0)
	cls, _ := syscall.UTF16PtrFromString("KinaraTrayWindow")
	app.icon = loadIcon()
	wc := wndClassEx{WndProc: syscall.NewCallback(wndProc), Instance: inst, ClassName: cls, Icon: app.icon, IconSm: app.icon}
	wc.Size = uint32(unsafe.Sizeof(wc))
	if r, _, err := pRegisterClassEx.Call(uintptr(unsafe.Pointer(&wc))); r == 0 {
		return err
	}
	title, _ := syscall.UTF16PtrFromString("Kinara")
	// A normal (never shown) top-level window, not a message-only one: only
	// top-level windows hear "TaskbarCreated".
	h, _, err := pCreateWindowEx.Call(0, uintptr(unsafe.Pointer(cls)), uintptr(unsafe.Pointer(title)), 0, 0, 0, 0, 0, 0, 0, inst, 0)
	if h == 0 {
		return err
	}
	app.hwnd = h
	tb, _ := syscall.UTF16PtrFromString("TaskbarCreated")
	r, _, _ := pRegisterWindowMessage.Call(uintptr(unsafe.Pointer(tb)))
	app.taskbar = uint32(r)
	return nil
}

func loadIcon() uintptr {
	path, _ := syscall.UTF16PtrFromString(filepath.Join(app.exeDir, "kinara.ico"))
	cx, _, _ := pGetSystemMetrics.Call(smCxSmIcon)
	cy, _, _ := pGetSystemMetrics.Call(smCySmIcon)
	h, _, _ := pLoadImage.Call(0, uintptr(unsafe.Pointer(path)), imageIcon, cx, cy, lrLoadFromFile)
	return h
}

func wndProc(hwnd uintptr, m uint32, w, l uintptr) uintptr {
	switch {
	case m == wmTray:
		switch uint32(l) & 0xFFFF {
		case wmLButtonDbl:
			openApp()
		case wmRButtonUp, wmLButtonUp, wmContextMenu:
			showMenu()
		}
		return 0
	case m == wmStatus:
		setTip()
		return 0
	case m == app.taskbar && m != 0:
		addIcon()
		return 0
	case m == wmDestroy:
		pPostQuitMessage.Call(0)
		return 0
	}
	r, _, _ := pDefWindowProc.Call(hwnd, uintptr(m), w, l)
	return r
}

func iconData() notifyIconData {
	d := notifyIconData{HWnd: app.hwnd, ID: 1}
	d.Size = uint32(unsafe.Sizeof(d))
	return d
}

func addIcon() {
	d := iconData()
	d.Flags = nifMsg | nifIcon | nifTip
	d.CallbackMessage = wmTray
	d.Icon = app.icon
	copyUTF16(d.Tip[:], tipText())
	pShellNotifyIcon.Call(nimAdd, uintptr(unsafe.Pointer(&d)))
}

func removeIcon() {
	d := iconData()
	pShellNotifyIcon.Call(nimDelete, uintptr(unsafe.Pointer(&d)))
}

func setTip() {
	d := iconData()
	d.Flags = nifTip
	copyUTF16(d.Tip[:], tipText())
	pShellNotifyIcon.Call(nimModify, uintptr(unsafe.Pointer(&d)))
}

// notify shows a short notification from the tray icon.
func notify(title, text string, warn bool) {
	d := iconData()
	d.Flags = nifInfo
	d.InfoFlags = niifInfo
	if warn {
		d.InfoFlags = niifWarn
	}
	copyUTF16(d.InfoTitle[:], title)
	copyUTF16(d.Info[:], text)
	pShellNotifyIcon.Call(nimModify, uintptr(unsafe.Pointer(&d)))
}

func statusText() string {
	switch app.status.Load() {
	case stRunning:
		return "Kinara is running"
	case stStarting:
		return "Kinara is starting"
	case stStopping:
		return "Kinara is stopping"
	case stStopped:
		return "Kinara is stopped"
	}
	return "Kinara: checking"
}

func tipText() string {
	if app.status.Load() == stRunning {
		return "Kinara is running on port " + app.port
	}
	return statusText()
}

func showMenu() {
	menu, _, _ := pCreatePopupMenu.Call()
	defer pDestroyMenu.Call(menu)
	st := app.status.Load()
	add := func(id int, text string, enabled bool) {
		t, _ := syscall.UTF16PtrFromString(text)
		flags := uintptr(mfString)
		if !enabled {
			flags |= mfGrayed
		}
		pAppendMenu.Call(menu, flags, uintptr(id), uintptr(unsafe.Pointer(t)))
	}
	sep := func() { pAppendMenu.Call(menu, mfSeparator, 0, 0) }

	add(0, statusText(), false)
	sep()
	add(cmdOpen, "Open Kinara", st == stRunning)
	sep()
	add(cmdStart, "Start server", st == stStopped || st == stUnknown)
	add(cmdStop, "Stop server", st == stRunning || st == stStarting)
	add(cmdRestart, "Restart server", st == stRunning)
	sep()
	add(cmdData, "Open data folder", true)
	add(cmdLogs, "Open logs", true)
	sep()
	if app.mode == "signin" {
		add(cmdQuit, "Quit Kinara", true)
	} else {
		add(cmdQuit, "Hide tray icon (server keeps running)", true)
	}

	var pt point
	pGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))
	// Without this the menu doesn't close when you click elsewhere.
	pSetForegroundWindow.Call(app.hwnd)
	cmd, _, _ := pTrackPopupMenu.Call(menu, tpmReturn|tpmRight, uintptr(pt.X), uintptr(pt.Y), 0, app.hwnd, 0)
	pPostMessage.Call(app.hwnd, wmNull, 0, 0)

	switch int(cmd) {
	case cmdOpen:
		openApp()
	case cmdStart:
		go startServer()
	case cmdStop:
		go stopServer()
	case cmdRestart:
		go func() {
			if stopServer() {
				startServer()
			}
		}()
	case cmdData:
		shellOpen(app.dataDir)
	case cmdLogs:
		shellOpen(filepath.Join(app.dataDir, "logs"))
	case cmdQuit:
		removeIcon()
		pPostQuitMessage.Call(0)
	}
}

func openApp() { shellOpen("http://localhost:" + app.port + "/") }

// openWhenUp waits (up to a minute) for the server to answer, then opens it.
// If it never does, it says where to look rather than opening an error page.
func openWhenUp() {
	client := &http.Client{Timeout: 2 * time.Second}
	for i := 0; i < 60; i++ {
		if resp, err := client.Get("http://127.0.0.1:" + app.port + "/api/v1/health"); err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				openApp()
				return
			}
		}
		time.Sleep(time.Second)
	}
	alert("Kinara isn't answering on port " + app.port + " yet.\n\nIf it was just installed or the PC just started, give it a minute. Otherwise the log may say why:\n" + filepath.Join(app.dataDir, "logs", "server.log"))
}

// ---- watching ----

func watch() {
	client := &http.Client{Timeout: 2 * time.Second}
	last := stUnknown
	for {
		st := probe(client)
		if st != last {
			app.status.Store(st)
			pPostMessage.Call(app.hwnd, wmStatus, 0, 0)
			// Only speak up when the server went down by itself.
			if last == stRunning && st == stStopped && app.mode == "service" {
				notify("Kinara stopped", "The server stopped. Open logs from this menu to see why; Windows will try to start it again.", true)
			}
			last = st
		}
		if st == stRunning {
			time.Sleep(5 * time.Second)
		} else {
			time.Sleep(1500 * time.Millisecond)
		}
	}
}

func probe(client *http.Client) int32 {
	healthy := false
	if resp, err := client.Get("http://127.0.0.1:" + app.port + "/api/v1/health"); err == nil {
		resp.Body.Close()
		healthy = resp.StatusCode == http.StatusOK
	}
	if app.mode == "signin" {
		app.child.Lock()
		running := app.child.cmd != nil
		app.child.Unlock()
		switch {
		case healthy:
			return stRunning
		case running:
			return stStarting
		}
		return stStopped
	}
	switch state, ok := queryService(); {
	case !ok:
		if healthy {
			return stRunning
		}
		return stUnknown
	case state == svcRunning && healthy:
		return stRunning
	case state == svcStopped:
		return stStopped
	case state == 3: // STOP_PENDING
		return stStopping
	default: // START_PENDING, or running but not answering yet
		return stStarting
	}
}

// ---- controlling ----

func startServer() {
	if app.mode == "signin" {
		app.wanted.Store(true)
		startChild()
		return
	}
	if err := controlService(true); err != nil {
		alert("Couldn't start the Kinara service: " + err.Error())
	}
}

// stopServer stops the server and waits (up to 40 s) for it to finish.
func stopServer() bool {
	if app.mode == "signin" {
		app.wanted.Store(false)
		stopChild()
		return true
	}
	if err := controlService(false); err != nil {
		alert("Couldn't stop the Kinara service: " + err.Error())
		return false
	}
	for i := 0; i < 80; i++ {
		if s, ok := queryService(); ok && s == svcStopped {
			return true
		}
		time.Sleep(500 * time.Millisecond)
	}
	return false
}

func openService(access uint32) (scm, svc uintptr, err error) {
	scm, _, err = pOpenSCManager.Call(0, 0, scManagerConnect)
	if scm == 0 {
		return 0, 0, err
	}
	name, _ := syscall.UTF16PtrFromString(serviceName)
	svc, _, err = pOpenService.Call(scm, uintptr(unsafe.Pointer(name)), uintptr(access))
	if svc == 0 {
		pCloseServiceHandle.Call(scm)
		return 0, 0, err
	}
	return scm, svc, nil
}

func queryService() (uint32, bool) {
	scm, svc, err := openService(serviceQueryStatus)
	if err != nil {
		return 0, false
	}
	defer pCloseServiceHandle.Call(scm)
	defer pCloseServiceHandle.Call(svc)
	var st serviceStatus
	if r, _, _ := pQueryServiceStatus.Call(svc, uintptr(unsafe.Pointer(&st))); r == 0 {
		return 0, false
	}
	return st.CurrentState, true
}

// controlService starts or stops the service. The installer lets signed-in
// users start and stop this one service, so normally no prompt appears; if
// that permission was removed, it falls back to asking Windows for admin
// (a UAC prompt) and runs sc.exe.
func controlService(start bool) error {
	access := uint32(serviceStop)
	if start {
		access = serviceStart
	}
	scm, svc, err := openService(access)
	if err == nil {
		defer pCloseServiceHandle.Call(scm)
		defer pCloseServiceHandle.Call(svc)
		var r uintptr
		if start {
			r, _, err = pStartService.Call(svc, 0, 0)
		} else {
			var st serviceStatus
			r, _, err = pControlService.Call(svc, serviceControlStop, uintptr(unsafe.Pointer(&st)))
			if r == 0 && err == syscall.Errno(errServiceNotActive) {
				return nil
			}
		}
		if r != 0 {
			return nil
		}
	}
	if err != syscall.Errno(errAccessDenied) {
		return err
	}
	verb := "stop"
	if start {
		verb = "start"
	}
	return shellRun("runas", "sc.exe", verb+" "+serviceName)
}

// Sign-in mode: the server is our hidden child. Its stdin is a pipe we
// hold; closing it (or this process ending) tells it to shut down cleanly.
func startChild() {
	app.child.Lock()
	defer app.child.Unlock()
	if app.child.cmd != nil {
		return
	}
	cmd := exec.Command(filepath.Join(app.exeDir, "kinara-server.exe"), "serve")
	cmd.Env = append(os.Environ(),
		"KINORA_ENV_FILE="+filepath.Join(app.dataDir, "kinara.env"),
		"KINORA_STOP_ON_EOF=1")
	cmd.Dir = app.dataDir
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000} // CREATE_NO_WINDOW
	if err := os.MkdirAll(filepath.Join(app.dataDir, "logs"), 0o755); err == nil {
		logPath := filepath.Join(app.dataDir, "logs", "server.log")
		if fi, err := os.Stat(logPath); err == nil && fi.Size() > 10<<20 {
			_ = os.Rename(logPath, logPath+".old")
		}
		if f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); err == nil {
			cmd.Stdout, cmd.Stderr = f, f
		}
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		alert("Couldn't start Kinara: " + err.Error())
		return
	}
	if err := cmd.Start(); err != nil {
		alert("Couldn't start Kinara: " + err.Error())
		return
	}
	app.child.cmd, app.child.stdin = cmd, stdin
	go func() {
		err := cmd.Wait()
		if f, ok := cmd.Stdout.(*os.File); ok {
			f.Close()
		}
		app.child.Lock()
		app.child.cmd, app.child.stdin = nil, nil
		app.child.Unlock()
		if app.wanted.Load() {
			// It ended without being asked: say so, then try again shortly.
			notify("Kinara stopped", fmt.Sprintf("The server stopped (%v). Starting it again; Open logs shows why.", err), true)
			time.Sleep(5 * time.Second)
			if app.wanted.Load() {
				startChild()
			}
		}
	}()
}

func stopChild() {
	app.child.Lock()
	cmd, stdin := app.child.cmd, app.child.stdin
	app.child.Unlock()
	if cmd == nil {
		return
	}
	stdin.Close()
	done := make(chan struct{})
	go func() {
		for {
			app.child.Lock()
			gone := app.child.cmd == nil
			app.child.Unlock()
			if gone {
				close(done)
				return
			}
			time.Sleep(200 * time.Millisecond)
		}
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		_ = cmd.Process.Kill()
	}
}

// ---- small helpers ----

func shellOpen(target string) { _ = shellRun("open", target, "") }

func shellRun(verb, file, args string) error {
	v, _ := syscall.UTF16PtrFromString(verb)
	f, _ := syscall.UTF16PtrFromString(file)
	var a *uint16
	if args != "" {
		a, _ = syscall.UTF16PtrFromString(args)
	}
	r, _, _ := pShellExecute.Call(app.hwnd, uintptr(unsafe.Pointer(v)), uintptr(unsafe.Pointer(f)), uintptr(unsafe.Pointer(a)), 0, 0 /* SW_HIDE */)
	if r <= 32 {
		if r == 5 { // SE_ERR_ACCESSDENIED: the UAC prompt was declined
			return fmt.Errorf("permission was not given")
		}
		return fmt.Errorf("Windows error %d", r)
	}
	return nil
}

func alert(text string) {
	t, _ := syscall.UTF16PtrFromString(text)
	c, _ := syscall.UTF16PtrFromString("Kinara")
	pMessageBox.Call(app.hwnd, uintptr(unsafe.Pointer(t)), uintptr(unsafe.Pointer(c)), 0x30 /* MB_ICONWARNING */)
}

func regString(name, fallback string) string {
	key, _ := syscall.UTF16PtrFromString(`Software\Kinara`)
	val, _ := syscall.UTF16PtrFromString(name)
	buf := make([]uint16, 1024)
	size := uint32(len(buf) * 2)
	r, _, _ := pRegGetValue.Call(hkeyLocalMachine, uintptr(unsafe.Pointer(key)), uintptr(unsafe.Pointer(val)), rrfRtRegSz, 0, uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size)))
	if r != 0 {
		return fallback
	}
	if s := syscall.UTF16ToString(buf); s != "" {
		return s
	}
	return fallback
}

// listenPort reads KINORA_LISTEN from the settings file. In service mode
// that file is readable only by admins (it sits beside the database), so
// otherwise it's the port the installer recorded.
func listenPort(envFile string) string {
	if b, err := os.ReadFile(envFile); err == nil {
		return portFromEnv(string(b))
	}
	return regString("Port", "8420")
}

func portFromEnv(text string) string {
	port := "8420"
	for _, line := range strings.Split(text, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok || strings.TrimSpace(k) != "KINORA_LISTEN" {
			continue
		}
		v = strings.Trim(strings.TrimSpace(v), `"'`)
		if i := strings.LastIndex(v, ":"); i >= 0 && i+1 < len(v) {
			port = v[i+1:]
		}
	}
	return port
}

func copyUTF16(dst []uint16, s string) {
	u, _ := syscall.UTF16FromString(s)
	if len(u) > len(dst) {
		u = u[:len(dst)]
		u[len(u)-1] = 0
	}
	copy(dst, u)
}
