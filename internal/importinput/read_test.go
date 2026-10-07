//go:build linux || darwin

package importinput

import (
	"context"
	p "github.com/leileipei/Enterprise_IM/internal/importpreflight"
	"golang.org/x/sys/unix"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func physicalTemp(t *testing.T) string {
	t.Helper()
	raw, e := os.MkdirTemp("/tmp", "im-preflight-")
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { os.RemoveAll(raw) })
	d, e := filepath.EvalSymlinks(raw)
	if e != nil {
		t.Fatal(e)
	}
	return d
}
func TestSharedReadKindsAndReplacement(t *testing.T) {
	d := physicalTemp(t)
	regular := filepath.Join(d, "ordinary")
	if e := os.WriteFile(regular, []byte("{}"), 0600); e != nil {
		t.Fatal(e)
	}
	f, e := OpenRegular(regular)
	if e != nil {
		t.Fatal(e)
	}
	initial, _ := f.Stat()
	f.Close()
	link := filepath.Join(d, "link")
	os.Symlink(regular, link)
	dirlink := filepath.Join(d, "dirlink")
	os.Symlink(d, dirlink)
	fifo := filepath.Join(d, "fifo")
	if e := unix.Mkfifo(fifo, 0600); e != nil {
		t.Fatal(e)
	}
	sock := filepath.Join(d, "socket")
	l, e := net.Listen("unix", sock)
	if e != nil {
		t.Fatal(e)
	}
	defer l.Close()
	for _, path := range []string{d, link, filepath.Join(dirlink, "ordinary"), fifo, sock, "/dev/null"} {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_, _, e := Read(ctx, path)
		cancel()
		if e == nil || e.Error() != "INPUT_TYPE_UNSUPPORTED" {
			t.Fatalf("unsafe type accepted: %v", e)
		}
	}
	other := filepath.Join(d, "other")
	os.WriteFile(other, []byte("{}"), 0600)
	g, _ := os.Open(other)
	defer g.Close()
	if VerifyOpened(initial, g) == nil {
		t.Fatal("replacement inode accepted")
	}
	// Concurrent substitution may return the original safe file or reject; never follow a link/FIFO or wait for a writer.
	for i := 0; i < 40; i++ {
		path := filepath.Join(d, "race")
		os.WriteFile(path, []byte("{}"), 0600)
		ready := make(chan struct{})
		done := make(chan struct{})
		go func() {
			<-ready
			os.Remove(path)
			if i%2 == 0 {
				os.Symlink(regular, path)
			} else {
				unix.Mkfifo(path, 0600)
			}
			close(done)
		}()
		close(ready)
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		b, _, err := Read(ctx, path)
		cancel()
		<-done
		if err == nil && string(b) != "{}" {
			t.Fatal("unsafe replacement read")
		}
		os.Remove(path)
	}
	os.WriteFile(regular, []byte("changed metadata"), 0600)
	g2, _ := os.Open(regular)
	defer g2.Close()
	if VerifyOpened(initial, g2) == nil {
		t.Fatal("changed metadata accepted")
	}
}
func TestSharedReadByteBoundAndCancellation(t *testing.T) {
	d := physicalTemp(t)
	path := filepath.Join(d, "input-private-marker")
	for _, n := range []int{p.MaxInput, p.MaxInput + 1, p.MaxInput + 65536} {
		os.WriteFile(path, make([]byte, n), 0600)
		b, issue, e := Read(context.Background(), path)
		if n == p.MaxInput {
			if e != nil || issue != nil || len(b) != n {
				t.Fatal("boundary read")
			}
		} else if e != nil || b != nil || issue == nil || issue.Code != "INPUT_TOO_LARGE" {
			t.Fatal("partial bytes hashable")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	b, _, e := Read(ctx, path)
	if b != nil || e == nil || e.Error() != "CANCELED" {
		t.Fatal("input cancel")
	}
	_, _, e = Read(context.Background(), filepath.Join(d, "missing-private-marker"))
	if e == nil || e.Error() != "INPUT_READ_FAILED" {
		t.Fatal("unsafe read diagnostic")
	}
}
