package filetransfer

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/leileipei/Enterprise_IM/internal/files"
)

// Hold the directory lock for the service lifetime. The kernel releases it on
// process death; startup reclamation cannot race another service using this dir.
func claimSpoolDir(dir string) (lock *os.File, err error) {
	if err = privateSpoolDir(dir); err != nil {
		return nil, err
	}
	before, e := os.Lstat(dir)
	if e != nil {
		return nil, files.ErrDependencyUnavailable
	}
	f, e := os.Open(dir)
	if e != nil {
		return nil, files.ErrDependencyUnavailable
	}
	ok := false
	defer func() {
		if !ok {
			f.Close()
		}
	}()
	after, e := f.Stat()
	if e != nil || !os.SameFile(before, after) {
		return nil, files.ErrDependencyUnavailable
	}
	if e = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); e != nil {
		return nil, files.ErrDependencyUnavailable
	}
	count := 0
	for {
		entries, readErr := f.ReadDir(64)
		for _, entry := range entries {
			name := entry.Name()
			if !ownedSpoolName(name) {
				continue
			}
			info, e := entry.Info()
			if e != nil {
				return nil, files.ErrDependencyUnavailable
			}
			stat, valid := info.Sys().(*syscall.Stat_t)
			if !valid || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || stat.Uid != uint32(os.Geteuid()) || info.Size() > files.MaxFileSizeBytes+1 {
				return nil, files.ErrDependencyUnavailable
			}
			// Bound each startup sweep. Retry may continue a large legacy backlog.
			if count >= 1024 {
				return nil, files.ErrDependencyUnavailable
			}
			count++
			if e = os.Remove(filepath.Join(dir, name)); e != nil {
				return nil, files.ErrDependencyUnavailable
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return nil, files.ErrDependencyUnavailable
		}
	}
	ok = true
	return f, nil
}
func ownedSpoolName(name string) bool {
	suffix, ok := strings.CutPrefix(name, "im-upload-")
	if !ok || suffix == "" {
		return false
	}
	for _, b := range []byte(suffix) {
		if b < '0' || b > '9' {
			return false
		}
	}
	return true
}

// Close stops admission, waits for accepted uploads to clean their temporary
// files, then releases the directory. Workers close only after RunOnce exits.
func (s *Service) Close() error {
	s.closeOnce.Do(func() {
		s.lifecycle.Lock()
		s.closed = true
		s.lifecycle.Unlock()
		s.active.Wait()
		if s.spoolLock != nil {
			s.closeErr = s.spoolLock.Close()
		}
	})
	return s.closeErr
}
func (s *Service) beginUpload() bool {
	s.lifecycle.Lock()
	defer s.lifecycle.Unlock()
	if s.closed {
		return false
	}
	s.active.Add(1)
	return true
}
