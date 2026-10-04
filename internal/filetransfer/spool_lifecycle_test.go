package filetransfer

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/leileipei/Enterprise_IM/internal/access"
	"github.com/leileipei/Enterprise_IM/internal/files"
	"github.com/leileipei/Enterprise_IM/internal/objectstore"
)

type killedSpoolObjects struct {
	objectStub
	active bool
}

func (s *killedSpoolObjects) PutVersion(ctx context.Context, _ objectstore.Location, _ string, _ files.Measurement, _ io.ReadSeeker) (objectstore.VersionRef, error) {
	if s.active {
		os.Stdout.WriteString("SPOOL_ACTIVE\n")
		<-ctx.Done()
		return objectstore.VersionRef{}, ctx.Err()
	}
	syscall.Kill(os.Getpid(), syscall.SIGKILL)
	panic("SIGKILL returned")
}

type killedSpoolScanner struct{ scannerStub }

func (*killedSpoolScanner) Scan(context.Context, *os.File, files.Metadata) (files.ScanDecision, error) {
	syscall.Kill(os.Getpid(), syscall.SIGKILL)
	panic("SIGKILL returned")
}
func TestFileSpoolCrashChild(t *testing.T) {
	dir := os.Getenv("IM_TEST_CRASH_SPOOL_DIR")
	if dir == "" {
		return
	}
	role := os.Getenv("IM_TEST_CRASH_SPOOL_ROLE")
	objects := &killedSpoolObjects{objectStub: objectStub{body: []byte("x")}, active: role == "active"}
	svc, e := NewService(&uploadRepoStub{ticket: transferTicket()}, objects, dir, testID)
	if e != nil {
		t.Fatal(e)
	}
	if role == "scan" {
		worker := ScanWorker{Repo: &scanRepoStub{ticket: scanTransferTicket()}, Objects: objects, Scanner: &killedSpoolScanner{}, SpoolDir: dir, OwnerID: testID}
		_, e = worker.RunOnce(context.Background())
	} else {
		_, e = svc.Upload(context.Background(), access.TrustedIdentity{}, testID, strings.NewReader("x"))
	}
	t.Fatal("child survived", e)
}
func spoolChild(ctx context.Context, dir, role string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestFileSpoolCrashChild$")
	cmd.Env = append(os.Environ(), "IM_TEST_CRASH_SPOOL_DIR="+dir, "IM_TEST_CRASH_SPOOL_ROLE="+role)
	return cmd
}
func assertSpoolCount(t *testing.T, dir string, want int) {
	t.Helper()
	entries, e := os.ReadDir(dir)
	if e != nil {
		t.Fatal(e)
	}
	n := 0
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "im-upload-") {
			n++
		}
	}
	if n != want {
		t.Fatalf("spool files = %d, want %d", n, want)
	}
}
func TestFileSpoolCrashRecovery(t *testing.T) {
	for _, role := range []string{"upload", "scan"} {
		t.Run(role, func(t *testing.T) {
			dir := t.TempDir() + "/private"
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			e := spoolChild(ctx, dir, role).Run()
			ex, ok := e.(*exec.ExitError)
			if !ok || ex.ProcessState.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL {
				t.Fatal("not expected crash", e)
			}
			assertSpoolCount(t, dir, 1)
			note := filepath.Join(dir, "operator-note")
			if e = os.WriteFile(note, []byte("keep"), 0600); e != nil {
				t.Fatal(e)
			}
			svc, e := NewService(&uploadRepoStub{ticket: transferTicket()}, &objectStub{}, dir, testID)
			if e != nil {
				t.Fatal(e)
			}
			defer svc.Close()
			assertSpoolCount(t, dir, 0)
			if _, e = svc.Upload(context.Background(), access.TrustedIdentity{}, testID, strings.NewReader("x")); e != nil {
				t.Fatal(e)
			}
			assertSpoolCount(t, dir, 0)
			if b, e := os.ReadFile(note); e != nil || string(b) != "keep" {
				t.Fatal("unrelated file removed", e)
			}
		})
	}
}
func TestFileSpoolActiveProcessProtected(t *testing.T) {
	dir := t.TempDir() + "/private"
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := spoolChild(ctx, dir, "active")
	pipe, e := cmd.StdoutPipe()
	if e != nil {
		t.Fatal(e)
	}
	if e = cmd.Start(); e != nil {
		t.Fatal(e)
	}
	defer func() { cmd.Process.Kill(); cmd.Wait() }()
	line, e := bufio.NewReader(pipe).ReadString('\n')
	if e != nil || line != "SPOOL_ACTIVE\n" {
		t.Fatal(line, e)
	}
	assertSpoolCount(t, dir, 1)
	svc, e := NewService(&uploadRepoStub{ticket: transferTicket()}, &objectStub{}, dir, testID)
	if e == nil {
		_ = svc
		t.Fatal("second process accepted shared active spool")
	}
	assertSpoolCount(t, dir, 1)
}

type waitingSpoolObjects struct {
	objectStub
	entered, release chan struct{}
}

func (s *waitingSpoolObjects) PutVersion(ctx context.Context, l objectstore.Location, a string, m files.Measurement, r io.ReadSeeker) (objectstore.VersionRef, error) {
	close(s.entered)
	select {
	case <-s.release:
	case <-ctx.Done():
		return objectstore.VersionRef{}, ctx.Err()
	}
	return s.objectStub.PutVersion(ctx, l, a, m, r)
}
func TestFileSpoolCloseWaits(t *testing.T) {
	dir := t.TempDir() + "/private"
	objects := &waitingSpoolObjects{entered: make(chan struct{}), release: make(chan struct{})}
	svc, e := NewService(&uploadRepoStub{ticket: transferTicket()}, objects, dir, testID)
	if e != nil {
		t.Fatal(e)
	}
	defer svc.Close()
	uploaded := make(chan error, 1)
	go func() {
		_, e := svc.Upload(context.Background(), access.TrustedIdentity{}, testID, strings.NewReader("x"))
		uploaded <- e
	}()
	<-objects.entered
	closed := make(chan error, 1)
	go func() { closed <- svc.Close() }()
	deadline := time.Now().Add(time.Second)
	for {
		svc.lifecycle.Lock()
		stopped := svc.closed
		svc.lifecycle.Unlock()
		if stopped {
			break
		}
		if time.Now().After(deadline) {
			close(objects.release)
			t.Fatal("close did not stop admission")
		}
		time.Sleep(time.Millisecond)
	}
	if _, e := svc.Upload(context.Background(), access.TrustedIdentity{}, testID, strings.NewReader("x")); !errors.Is(e, files.ErrDependencyUnavailable) {
		close(objects.release)
		t.Fatal("closed service accepted upload", e)
	}
	other, e := NewService(&uploadRepoStub{ticket: transferTicket()}, &objectStub{}, dir, testID)
	if e == nil {
		other.Close()
		close(objects.release)
		t.Fatal("directory released while upload active")
	}
	assertSpoolCount(t, dir, 1)
	select {
	case e := <-closed:
		close(objects.release)
		t.Fatal("close returned before cleanup", e)
	default:
	}
	close(objects.release)
	if e = <-uploaded; e != nil {
		t.Fatal(e)
	}
	if e = <-closed; e != nil {
		t.Fatal(e)
	}
	assertSpoolCount(t, dir, 0)
	restarted, e := NewService(&uploadRepoStub{ticket: transferTicket()}, &objectStub{}, dir, testID)
	if e != nil {
		t.Fatal(e)
	}
	defer restarted.Close()
}
func TestFileSpoolStartupSafety(t *testing.T) {
	for _, kind := range []string{"symlink", "public permissions"} {
		t.Run(kind, func(t *testing.T) {
			base := t.TempDir()
			dir := base + "/private"
			if e := os.Mkdir(dir, 0700); e != nil {
				t.Fatal(e)
			}
			target := base + "/outside"
			if e := os.WriteFile(target, []byte("preserve"), 0600); e != nil {
				t.Fatal(e)
			}
			name := dir + "/im-upload-123"
			if kind == "symlink" {
				if e := os.Symlink(target, name); e != nil {
					t.Fatal(e)
				}
			} else {
				if e := os.WriteFile(name, []byte("preserve"), 0644); e != nil {
					t.Fatal(e)
				}
			}
			svc, e := NewService(&uploadRepoStub{ticket: transferTicket()}, &objectStub{}, dir, testID)
			if e == nil {
				svc.Close()
				t.Fatal("unproven file reclaimed")
			}
			if b, e := os.ReadFile(target); e != nil || string(b) != "preserve" {
				t.Fatal("outside file touched", e)
			}
			if _, e = os.Lstat(name); e != nil {
				t.Fatal("unproven entry removed", e)
			}
			if e = os.Remove(name); e != nil {
				t.Fatal(e)
			}
			restarted, e := NewService(&uploadRepoStub{ticket: transferTicket()}, &objectStub{}, dir, testID)
			if e != nil {
				t.Fatal("failed precheck leaked directory lock", e)
			}
			restarted.Close()
		})
	}
}
