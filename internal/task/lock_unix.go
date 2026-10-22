//go:build !windows

package task

import (
	"os"
	"syscall"
)

type instanceLock struct{ file *os.File }

func acquireInstanceLock(path string) (*instanceLock, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = f.Chmod(0600); err == nil {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	}
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return &instanceLock{file: f}, nil
}

func (l *instanceLock) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	err := syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
	closeErr := l.file.Close()
	if err != nil {
		return err
	}
	return closeErr
}

func syncDir(path string) error {
	d, err := os.Open(path)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
