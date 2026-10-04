package filedownload

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestFileDownloadSpoolPathIsolation(t *testing.T) {
	r := &downloadRepo{data: []byte("x")}
	o := &downloadObjects{data: r.data}
	for _, which := range []string{"symlink", "open_permissions", "unknown_content", "different_owner"} {
		t.Run(which, func(t *testing.T) {
			base := t.TempDir()
			root := filepath.Join(base, "spool")
			switch which {
			case "symlink":
				target := filepath.Join(base, "target")
				os.Mkdir(target, 0700)
				os.Symlink(target, root)
			case "open_permissions":
				os.Mkdir(root, 0755)
			case "unknown_content":
				os.Mkdir(root, 0700)
				os.WriteFile(filepath.Join(root, "customer.txt"), []byte("preserve"), 0600)
			case "different_owner":
				s, e := NewService(r, o, root, downloadTestID)
				if e != nil {
					t.Fatal(e)
				}
				s.Close()
			}
			owner := downloadTestID
			if which == "different_owner" {
				owner = "00000000-0000-4000-8000-000000000002"
			}
			if s, e := NewService(r, o, root, owner); e == nil {
				s.Close()
				t.Fatal("unproven root accepted")
			}
			if which == "unknown_content" {
				b, e := os.ReadFile(filepath.Join(root, "customer.txt"))
				if e != nil || string(b) != "preserve" {
					t.Fatal("unknown content deleted", e)
				}
			}
		})
	}
}
func TestFileDownloadSpoolCrashHelper(t *testing.T) {
	dir := os.Getenv("IM_P424_DOWNLOAD_CRASH_ROOT")
	if dir == "" {
		return
	}
	r := &downloadRepo{data: []byte("crash payload")}
	s, e := NewService(r, &downloadObjects{data: r.data}, dir, downloadTestID)
	if e != nil {
		os.Exit(2)
	}
	if _, e = s.Prepare(context.Background(), downloadTestActor, downloadTestID); e != nil {
		os.Exit(3)
	}
	os.Exit(0)
}
func TestFileDownloadSpoolCrashRecovery(t *testing.T) {
	root := filepath.Join(t.TempDir(), "spool")
	ctx := context.Background()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestFileDownloadSpoolCrashHelper$")
	cmd.Env = append(os.Environ(), "IM_P424_DOWNLOAD_CRASH_ROOT="+root)
	if b, e := cmd.CombinedOutput(); e != nil {
		t.Fatal(e, string(b))
	}
	before, e := os.ReadDir(root)
	if e != nil || len(before) != 2 {
		t.Fatal("no owned orphan", before, e)
	}
	r := &downloadRepo{data: []byte("x")}
	s, e := NewService(r, &downloadObjects{data: r.data}, root, downloadTestID)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	after, e := os.ReadDir(root)
	if e != nil || len(after) != 1 {
		t.Fatal("owned crash not reclaimed", after, e)
	}
	if other, e := NewService(r, &downloadObjects{data: r.data}, root, downloadTestID); e == nil {
		other.Close()
		t.Fatal("two active owners")
	}
}

func TestFileDownloadSpoolUnknownRuntimeContent(t *testing.T) {
	root := filepath.Join(t.TempDir(), "spool")
	r := &downloadRepo{data: []byte("x")}
	s, e := NewService(r, &downloadObjects{data: r.data}, root, downloadTestID)
	if e != nil {
		t.Fatal(e)
	}
	p, e := s.Prepare(context.Background(), downloadTestActor, downloadTestID)
	if e != nil {
		t.Fatal(e)
	}
	marker := filepath.Join(root, p.ticket.SessionID, "customer.txt")
	if e = os.WriteFile(marker, []byte("preserve"), 0600); e != nil {
		t.Fatal(e)
	}
	if e = p.Close(); e == nil {
		t.Fatal("unknown content claimed clean")
	}
	if next, e := s.Prepare(context.Background(), downloadTestActor, downloadTestID); next != nil || e == nil {
		t.Fatal("disk budget reused after failed cleanup")
	}
	if e = s.Close(); e == nil {
		t.Fatal("service close hid failed cleanup")
	}
	if b, e := os.ReadFile(marker); e != nil || string(b) != "preserve" {
		t.Fatal("unknown content removed", e)
	}
}
