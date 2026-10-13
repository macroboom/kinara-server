//go:build windows

package scanner

import (
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"
)

const (
	ioctlStorageQueryProperty = 0x002D1400
	storageDeviceSeekPenalty  = 7 // StorageDeviceSeekPenaltyProperty
	propertyStandardQuery     = 0
)

// storagePropertyQuery is STORAGE_PROPERTY_QUERY.
type storagePropertyQuery struct {
	PropertyID           uint32
	QueryType            uint32
	AdditionalParameters [4]byte
}

// seekPenaltyDescriptor is DEVICE_SEEK_PENALTY_DESCRIPTOR.
type seekPenaltyDescriptor struct {
	Version           uint32
	Size              uint32
	IncursSeekPenalty uint8
	_                 [3]byte
}

// detectDisk asks Windows whether the volume's disk "incurs a seek penalty",
// which is how Windows itself tells hard drives from SSDs (the Optimize
// Drives tool uses the same query).
func detectDisk(path string) DiskKind {
	abs, err := filepath.Abs(path)
	if err != nil {
		return DiskUnknown
	}
	vol := filepath.VolumeName(abs)
	if strings.HasPrefix(vol, `\\`) {
		return DiskNetwork // UNC path: \\server\share
	}
	if len(vol) != 2 || vol[1] != ':' {
		return DiskUnknown
	}
	root, err := syscall.UTF16PtrFromString(vol + `\`)
	if err != nil {
		return DiskUnknown
	}
	if t := getDriveType(root); t == driveRemote {
		return DiskNetwork // mapped network drive
	}

	name, err := syscall.UTF16PtrFromString(`\\.\` + vol)
	if err != nil {
		return DiskUnknown
	}
	// Access 0 is enough for this query and needs no admin rights.
	h, err := syscall.CreateFile(name, 0, syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE,
		nil, syscall.OPEN_EXISTING, 0, 0)
	if err != nil {
		return DiskUnknown
	}
	defer syscall.CloseHandle(h)

	q := storagePropertyQuery{PropertyID: storageDeviceSeekPenalty, QueryType: propertyStandardQuery}
	var d seekPenaltyDescriptor
	var n uint32
	err = syscall.DeviceIoControl(h, ioctlStorageQueryProperty,
		(*byte)(unsafe.Pointer(&q)), uint32(unsafe.Sizeof(q)),
		(*byte)(unsafe.Pointer(&d)), uint32(unsafe.Sizeof(d)), &n, nil)
	if err != nil || n < 9 {
		return DiskUnknown
	}
	if d.IncursSeekPenalty != 0 {
		return DiskHDD
	}
	return DiskSSD
}

const driveRemote = 4 // DRIVE_REMOTE

var procGetDriveTypeW = syscall.NewLazyDLL("kernel32.dll").NewProc("GetDriveTypeW")

func getDriveType(root *uint16) uintptr {
	r, _, _ := procGetDriveTypeW.Call(uintptr(unsafe.Pointer(root)))
	return r
}
