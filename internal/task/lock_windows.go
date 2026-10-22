//go:build windows

package task

import (
	"os"

	"golang.org/x/sys/windows"
)

type instanceLock struct {
	file       *os.File
	overlapped windows.Overlapped
}

func acquireInstanceLock(path string) (*instanceLock, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = f.Chmod(0600); err != nil {
		_ = f.Close()
		return nil, err
	}
	l := &instanceLock{file: f}
	err = windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &l.overlapped)
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return l, nil
}

func (l *instanceLock) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	err := windows.UnlockFileEx(windows.Handle(l.file.Fd()), 0, 1, 0, &l.overlapped)
	closeErr := l.file.Close()
	if err != nil {
		return err
	}
	return closeErr
}

// Windows does not support opening a directory handle through os.Open for fsync.
func syncDir(string) error { return nil }
