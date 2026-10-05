package filedownload

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func healthService(t *testing.T) (*Service, string, *downloadRepo, *downloadObjects) {
	t.Helper()
	r := &downloadRepo{data: []byte("hello")}
	o := &downloadObjects{data: r.data}
	dir := filepath.Join(t.TempDir(), "download")
	s, e := NewService(r, o, dir, downloadTestID)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { s.Close() })
	return s, dir, r, o
}

func TestDownloadRuntimeHealth(t *testing.T) {
	t.Run("healthy", func(t *testing.T) {
		s, _, _, _ := healthService(t)
		if e := s.CheckHealth(context.Background()); e != nil {
			t.Fatal(e)
		}
	})
	t.Run("closed", func(t *testing.T) {
		s, _, _, _ := healthService(t)
		s.Close()
		if s.CheckHealth(context.Background()) == nil {
			t.Fatal("closed service healthy")
		}
	})
	t.Run("failed", func(t *testing.T) {
		s, _, _, _ := healthService(t)
		s.lifecycle.Lock()
		s.failed = true
		s.lifecycle.Unlock()
		if s.CheckHealth(context.Background()) == nil {
			t.Fatal("failed cleanup service healthy")
		}
	})
	t.Run("cancelled", func(t *testing.T) {
		s, _, _, _ := healthService(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if s.CheckHealth(ctx) == nil {
			t.Fatal("caller cancellation ignored")
		}
	})
	t.Run("permissions", func(t *testing.T) {
		s, dir, _, _ := healthService(t)
		if e := os.Chmod(dir, 0755); e != nil {
			t.Fatal(e)
		}
		if s.CheckHealth(context.Background()) == nil {
			t.Fatal("public spool healthy")
		}
	})
	t.Run("owner_manifest", func(t *testing.T) {
		s, dir, _, _ := healthService(t)
		b, e := json.Marshal(spoolManifest{Purpose: "enterprise-im-download-owner-v1", Owner: "00000000-0000-4000-8000-000000000002"})
		if e != nil {
			t.Fatal(e)
		}
		if e = os.WriteFile(filepath.Join(dir, spoolOwnerName), b, 0600); e != nil {
			t.Fatal(e)
		}
		if s.CheckHealth(context.Background()) == nil {
			t.Fatal("reassigned manifest healthy")
		}
	})
}

// The host user cannot chown to another UID; exercise the reused OS metadata validator directly.
// RP13 supplies the separate real Linux ownership rejection evidence.
type differentUIDInfo struct{ os.FileInfo }

func (i differentUIDInfo) Sys() any {
	st := *(i.FileInfo.Sys().(*syscall.Stat_t))
	st.Uid ^= 1
	return &st
}
func TestDownloadRuntimeWrongUID(t *testing.T) {
	_, dir, _, _ := healthService(t)
	info, e := os.Lstat(dir)
	if e != nil {
		t.Fatal(e)
	}
	if ownedInfo(differentUIDInfo{info}, true, 0) {
		t.Fatal("foreign UID accepted by runtime ownership validator")
	}
}

func TestDownloadRuntimeRootReplacement(t *testing.T) {
	for _, which := range []string{"different_inode", "symlink"} {
		t.Run(which, func(t *testing.T) {
			s, dir, _, _ := healthService(t)
			moved := dir + "-original"
			if e := os.Rename(dir, moved); e != nil {
				t.Fatal(e)
			}
			if which == "symlink" {
				if e := os.Symlink(moved, dir); e != nil {
					t.Fatal(e)
				}
			} else {
				if e := os.Mkdir(dir, 0700); e != nil {
					t.Fatal(e)
				}
			}
			if s.CheckHealth(context.Background()) == nil {
				t.Fatal("replaced live directory reported healthy")
			}
			if _, e := os.Stat(filepath.Join(moved, spoolOwnerName)); e != nil {
				t.Fatal("original evidence altered", e)
			}
		})
	}
}

func TestDownloadRuntimeHealthNoSideEffects(t *testing.T) {
	s, dir, r, o := healthService(t)
	unknown := filepath.Join(dir, "customer.txt")
	if e := os.WriteFile(unknown, []byte("preserve"), 0600); e != nil {
		t.Fatal(e)
	}
	before, e := os.ReadDir(dir)
	if e != nil {
		t.Fatal(e)
	}
	for i := 0; i < 3; i++ {
		if e := s.CheckHealth(context.Background()); e != nil {
			t.Fatal(e)
		}
	}
	after, e := os.ReadDir(dir)
	if e != nil || len(before) != len(after) {
		t.Fatal("health changed spool", e)
	}
	if r.seq != 0 || len(r.results) != 0 || o.seen.VersionID != "" {
		t.Fatal("health started business download")
	}
	b, e := os.ReadFile(unknown)
	if e != nil || string(b) != "preserve" {
		t.Fatal("health removed unknown contents", e)
	}
}
