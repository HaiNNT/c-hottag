package fsutil

import (
	"errors"
	"os"
	"syscall"
)

// Lock takes an exclusive advisory flock on path, creating it (0600) if
// needed, and blocks until it is free. The kernel releases the lock if the
// process dies, so a crash or reboot never leaves a stale lock. flock locks
// belong to the open file, so two Lock calls in one process also exclude
// each other.
func Lock(path string) (unlock func() error, err error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
		if err != syscall.EINTR {
			break
		}
	}
	if err != nil {
		f.Close()
		return nil, err
	}
	return func() error {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		return f.Close()
	}, nil
}

// TryLock takes the same lock as Lock but never waits: ok is false when
// another holder has it (e.g. `chottag login` is using that slot). The
// caller must not touch the resource when ok is false.
func TryLock(path string) (unlock func() error, ok bool, err error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, false, err
	}
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err != syscall.EINTR {
			break
		}
	}
	if errors.Is(err, syscall.EWOULDBLOCK) {
		f.Close()
		return nil, false, nil
	}
	if err != nil {
		f.Close()
		return nil, false, err
	}
	return func() error {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		return f.Close()
	}, true, nil
}
