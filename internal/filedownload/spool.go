package filedownload

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"github.com/leileipei/Enterprise_IM/internal/files"
)

const spoolOwnerName = ".owner.json"
const spoolSessionName = ".session.json"
const spoolContentName = "content"

type spoolManifest struct {
	Purpose string `json:"purpose"`
	Owner   string `json:"owner"`
	Session string `json:"session,omitempty"`
	File    string `json:"file,omitempty"`
	Size    int64  `json:"size,omitempty"`
}

func ownedInfo(st os.FileInfo, dir bool, max int64) bool {
	if st == nil || st.Mode()&os.ModeSymlink != 0 {
		return false
	}
	stat, ok := st.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) {
		return false
	}
	if dir {
		return st.IsDir() && st.Mode().Perm() == 0700
	}
	return st.Mode().IsRegular() && st.Mode().Perm() == 0600 && st.Size() >= 0 && st.Size() <= max && stat.Nlink == 1
}
func writeSpoolManifest(root *os.Root, name string, m spoolManifest) error {
	f, e := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0600)
	if e != nil {
		return e
	}
	e = json.NewEncoder(f).Encode(m)
	if e == nil {
		e = f.Sync()
	}
	return errors.Join(e, f.Close())
}
func readSpoolManifest(root *os.Root, name string) (spoolManifest, error) {
	var m spoolManifest
	before, e := root.Lstat(name)
	if e != nil || !ownedInfo(before, false, 4096) {
		return m, ErrUnavailable
	}
	f, e := root.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if e != nil {
		return m, ErrUnavailable
	}
	defer f.Close()
	after, e := f.Stat()
	if e != nil || !os.SameFile(before, after) {
		return m, ErrUnavailable
	}
	b, e := io.ReadAll(io.LimitReader(f, 4097))
	if e != nil || len(b) > 4096 {
		return m, ErrUnavailable
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if e = d.Decode(&m); e != nil {
		return m, ErrUnavailable
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return m, ErrUnavailable
	}
	return m, nil
}
func rootEntries(root *os.Root) ([]os.DirEntry, error) {
	f, e := root.Open(".")
	if e != nil {
		return nil, e
	}
	defer f.Close()
	entries, e := f.ReadDir(6)
	if e != nil && e != io.EOF {
		return nil, e
	}
	if len(entries) > 5 {
		return nil, ErrUnavailable
	}
	return entries, nil
}
func claimDownloadSpool(dir, owner string) (root *os.Root, lock *os.File, err error) {
	if dir == "" {
		return nil, nil, ErrUnavailable
	}
	if e := os.MkdirAll(dir, 0700); e != nil {
		return nil, nil, ErrUnavailable
	}
	before, e := os.Lstat(dir)
	if e != nil || !ownedInfo(before, true, 0) {
		return nil, nil, ErrUnavailable
	}
	lock, e = os.Open(dir)
	if e != nil {
		return nil, nil, ErrUnavailable
	}
	ok := false
	defer func() {
		if !ok {
			if root != nil {
				root.Close()
			}
			lock.Close()
		}
	}()
	after, e := lock.Stat()
	if e != nil || !os.SameFile(before, after) {
		return nil, nil, ErrUnavailable
	}
	if e = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); e != nil {
		return nil, nil, ErrUnavailable
	}
	root, e = os.OpenRoot(dir)
	if e != nil {
		return nil, nil, ErrUnavailable
	}
	opened, e := root.Stat(".")
	if e != nil || !os.SameFile(before, opened) {
		return nil, nil, ErrUnavailable
	}
	entries, e := rootEntries(root)
	if e != nil {
		return nil, nil, ErrUnavailable
	}
	if len(entries) == 0 {
		if e = writeSpoolManifest(root, spoolOwnerName, spoolManifest{Purpose: "enterprise-im-download-owner-v1", Owner: owner}); e != nil {
			return nil, nil, ErrUnavailable
		}
		if e = lock.Sync(); e != nil {
			return nil, nil, ErrUnavailable
		}
	} else {
		m, e := readSpoolManifest(root, spoolOwnerName)
		if e != nil || m != (spoolManifest{Purpose: "enterprise-im-download-owner-v1", Owner: owner}) {
			return nil, nil, ErrUnavailable
		}
		type orphan struct {
			name string
			info os.FileInfo
			size int64
		}
		var proven []orphan
		// Validate the whole bounded root before removing any content. Unknown paths
		// and incomplete provenance fail startup and remain untouched.
		for _, entry := range entries {
			if entry.Name() == spoolOwnerName {
				continue
			}
			if !canonicalID.MatchString(entry.Name()) {
				return nil, nil, ErrUnavailable
			}
			info, e := root.Lstat(entry.Name())
			if e != nil || !ownedInfo(info, true, 0) {
				return nil, nil, ErrUnavailable
			}
			child, e := root.OpenRoot(entry.Name())
			if e != nil {
				return nil, nil, ErrUnavailable
			}
			m, e := readSpoolManifest(child, spoolSessionName)
			if e == nil && (m.Purpose != "enterprise-im-download-session-v1" || m.Owner != owner || m.Session != entry.Name() || !canonicalID.MatchString(m.File) || m.Size < 1 || m.Size > files.MaxFileSizeBytes) {
				e = ErrUnavailable
			}
			if e == nil {
				e = validateSessionContents(child, m.Size)
			}
			child.Close()
			if e != nil {
				return nil, nil, ErrUnavailable
			}
			proven = append(proven, orphan{entry.Name(), info, m.Size})
		}
		for _, p := range proven {
			if e = removeDownloadSpool(root, p.name, p.info, p.size); e != nil {
				return nil, nil, ErrUnavailable
			}
		}
	}
	ok = true
	return root, lock, nil
}
func validateSessionContents(root *os.Root, size int64) error {
	f, e := root.Open(".")
	if e != nil {
		return e
	}
	defer f.Close()
	entries, e := f.ReadDir(3)
	if e != nil && e != io.EOF {
		return e
	}
	if len(entries) > 2 {
		return ErrUnavailable
	}
	for _, entry := range entries {
		max := size + 1
		switch entry.Name() {
		case spoolSessionName:
			max = 4096
		case spoolContentName:
		default:
			return ErrUnavailable
		}
		info, e := root.Lstat(entry.Name())
		if e != nil || !ownedInfo(info, false, max) {
			return ErrUnavailable
		}
	}
	return nil
}
func removeDownloadSpool(root *os.Root, name string, original os.FileInfo, size int64) error {
	info, e := root.Lstat(name)
	if e != nil || !ownedInfo(info, true, 0) || !os.SameFile(original, info) {
		return ErrUnavailable
	}
	child, e := root.OpenRoot(name)
	if e != nil {
		return ErrUnavailable
	}
	if e = validateSessionContents(child, size); e != nil {
		child.Close()
		return e
	}
	for _, n := range []string{spoolContentName, spoolSessionName} {
		if e = child.Remove(n); e != nil && !os.IsNotExist(e) {
			child.Close()
			return e
		}
	}
	if e = child.Close(); e != nil {
		return e
	}
	info, e = root.Lstat(name)
	if e != nil || !os.SameFile(original, info) {
		return ErrUnavailable
	}
	return root.Remove(name)
}
func spoolAbsolute(dir string) (string, error) {
	if dir == "" {
		return "", ErrUnavailable
	}
	return filepath.Abs(dir)
}
