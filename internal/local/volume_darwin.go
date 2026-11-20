package local

import (
	"encoding/hex"
	"fmt"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// nativeVolumeUUID reads the persistent volume identity, not st_dev or f_fsid,
// both of which describe a mount and can change across host reboots.
func nativeVolumeUUID(path string) (string, error) {
	var fs unix.Statfs_t
	if err := unix.Statfs(path, &fs); err != nil {
		return "", err
	}
	root, err := unix.BytePtrFromString(unix.ByteSliceToString(fs.Mntonname[:]))
	if err != nil {
		return "", err
	}
	attrs := unix.Attrlist{Bitmapcount: unix.ATTR_BIT_MAP_COUNT, Volattr: unix.ATTR_VOL_INFO | unix.ATTR_VOL_UUID}
	var result struct {
		Length uint32
		UUID   [16]byte
	}
	// x/sys exposes the attribute constants but has no Getattrlist wrapper.
	_, _, errno := syscall.Syscall6(unix.SYS_GETATTRLIST,
		uintptr(unsafe.Pointer(root)), uintptr(unsafe.Pointer(&attrs)),
		uintptr(unsafe.Pointer(&result)), unsafe.Sizeof(result), 0, 0)
	if errno != 0 {
		return "", errno
	}
	if result.Length != uint32(unsafe.Sizeof(result)) || result.UUID == [16]byte{} {
		return "", fmt.Errorf("filesystem did not return a volume UUID")
	}
	return hex.EncodeToString(result.UUID[:]), nil
}
