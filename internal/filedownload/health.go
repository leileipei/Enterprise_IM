package filedownload

import (
	"context"
	"os"
)

func (s *Service) CheckHealth(ctx context.Context) error {
	if s == nil || ctx.Err() != nil {
		return ErrUnavailable
	}
	s.lifecycle.Lock()
	defer s.lifecycle.Unlock()
	if s.closed || s.failed || s.root == nil || s.lock == nil {
		return ErrUnavailable
	}
	return s.checkSpoolHealth(ctx)
}

// Inspect only the owned root and its manifest; never traverse customer content.
func (s *Service) checkSpoolHealth(ctx context.Context) error {
	pathInfo, e := os.Lstat(s.rootPath)
	if e != nil || !ownedInfo(pathInfo, true, 0) {
		return ErrUnavailable
	}
	lockInfo, e := s.lock.Stat()
	if e != nil || !ownedInfo(lockInfo, true, 0) || !os.SameFile(pathInfo, lockInfo) {
		return ErrUnavailable
	}
	rootInfo, e := s.root.Stat(".")
	if e != nil || !ownedInfo(rootInfo, true, 0) || !os.SameFile(pathInfo, rootInfo) {
		return ErrUnavailable
	}
	m, e := readSpoolManifest(s.root, spoolOwnerName)
	if e != nil || m != (spoolManifest{Purpose: "enterprise-im-download-owner-v1", Owner: s.owner}) || ctx.Err() != nil {
		return ErrUnavailable
	}
	return nil
}
