//go:build windows

// Package fsacl makes folders private to the account the server runs as.
package fsacl

import (
	"fmt"
	"syscall"
	"unsafe"
)

var (
	advapi32                 = syscall.NewLazyDLL("advapi32.dll")
	procConvertSDDL          = advapi32.NewProc("ConvertStringSecurityDescriptorToSecurityDescriptorW")
	procGetSecurityDescDacl  = advapi32.NewProc("GetSecurityDescriptorDacl")
	procSetNamedSecurityInfo = advapi32.NewProc("SetNamedSecurityInfoW")
	kernel32                 = syscall.NewLazyDLL("kernel32.dll")
	procLocalFree            = kernel32.NewProc("LocalFree")
)

const (
	seFileObject              = 1
	daclSecurityInformation   = 0x00000004
	protectedDaclSecurityInfo = 0x80000000
	sddlRevision1             = 1
)

// Private gives a folder an access list of its own, not inherited from the
// one it's in (the installer opens the data and cache folders to every
// user in sign-in mode): full control for SYSTEM, administrators and the
// account the server runs as, nothing for anyone else. Files made in it
// afterwards inherit that.
func Private(dir string) error {
	sid, err := currentSID()
	if err != nil {
		return err
	}
	sddl := "D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;FA;;;" + sid + ")"
	sddlPtr, err := syscall.UTF16PtrFromString(sddl)
	if err != nil {
		return err
	}
	var sd uintptr
	r, _, callErr := procConvertSDDL.Call(uintptr(unsafe.Pointer(sddlPtr)), sddlRevision1, uintptr(unsafe.Pointer(&sd)), 0)
	if r == 0 {
		return fmt.Errorf("access list: %w", callErr)
	}
	defer procLocalFree.Call(sd)
	var present, defaulted int32
	var dacl uintptr
	r, _, callErr = procGetSecurityDescDacl.Call(sd, uintptr(unsafe.Pointer(&present)), uintptr(unsafe.Pointer(&dacl)), uintptr(unsafe.Pointer(&defaulted)))
	if r == 0 {
		return fmt.Errorf("access list: %w", callErr)
	}
	path, err := syscall.UTF16PtrFromString(dir)
	if err != nil {
		return err
	}
	// SetNamedSecurityInfo returns the error code itself.
	rc, _, _ := procSetNamedSecurityInfo.Call(uintptr(unsafe.Pointer(path)), seFileObject,
		daclSecurityInformation|protectedDaclSecurityInfo, 0, 0, dacl, 0)
	if rc != 0 {
		return fmt.Errorf("access list: %w", syscall.Errno(rc))
	}
	return nil
}

// currentSID is the account this process runs as, like S-1-5-18.
func currentSID() (string, error) {
	tok, err := syscall.OpenCurrentProcessToken()
	if err != nil {
		return "", err
	}
	defer tok.Close()
	u, err := tok.GetTokenUser()
	if err != nil {
		return "", err
	}
	return u.User.Sid.String()
}
