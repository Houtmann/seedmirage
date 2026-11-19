//go:build windows

package instance

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// errSharingViolation is ERROR_SHARING_VIOLATION: the file is open by another
// process with a conflicting share mode.
const errSharingViolation syscall.Errno = 32

// lockFile opens the file with share mode FILE_SHARE_READ only: any second
// open for writing fails, while a read-only open (Read) still works.
func lockFile(path string) (*os.File, error) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, fmt.Errorf("instance: %w", err)
	}
	h, err := syscall.CreateFile(p, syscall.GENERIC_READ|syscall.GENERIC_WRITE, syscall.FILE_SHARE_READ,
		nil, syscall.OPEN_ALWAYS, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		if errors.Is(err, errSharingViolation) {
			return nil, ErrLocked
		}
		return nil, fmt.Errorf("instance: %w", err)
	}
	return os.NewFile(uintptr(h), path), nil
}

func unlockFile(*os.File) {}
