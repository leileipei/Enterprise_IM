package filetransfer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"github.com/leileipei/Enterprise_IM/internal/access"
	"github.com/leileipei/Enterprise_IM/internal/files"
	"github.com/leileipei/Enterprise_IM/internal/objectstore"
	"io"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"
)

const testID = "00000000-0000-4000-8000-000000000001"

func TestFileTransferSpoolBounds(t *testing.T) {
	dir := t.TempDir() + "/private" + "/private"
	for _, tc := range []struct {
		b    string
		size int64
		want error
	}{{"x", 1, nil}, {"", 1, files.ErrInvalidFileSize}, {"x", 2, files.ErrInvalidFileSize}, {"xx", 1, files.ErrFileTooLarge}} {
		f, m, e := Spool(context.Background(), dir, tc.size, strings.NewReader(tc.b))
		if !errors.Is(e, tc.want) {
			t.Fatal(tc, e)
		}
		if e == nil {
			st, e := f.Stat()
			if e != nil || st.Mode().Perm() != 0600 || m.SHA256 != sha256.Sum256([]byte(tc.b)) {
				t.Fatal(st, m, e)
			}
			b, _ := io.ReadAll(f)
			if string(b) != tc.b {
				t.Fatal(b)
			}
			name := f.Name()
			f.Close()
			os.Remove(name)
		}
	}
	st, e := os.Stat(dir)
	if e != nil || st.Mode().Perm() != 0700 {
		t.Fatal(st, e)
	}
	entries, e := os.ReadDir(dir)
	if e != nil || len(entries) != 0 {
		t.Fatal(entries, e)
	}
	b := bytes.Repeat([]byte("x"), int(files.MaxFileSizeBytes))
	f, _, e := Spool(context.Background(), dir, int64(len(b)), bytes.NewReader(b))
	if e != nil {
		t.Fatal(e)
	}
	name := f.Name()
	f.Close()
	os.Remove(name)
}
func TestFileTransferSpoolDisconnect(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	r, w := io.Pipe()
	defer w.Close()
	if _, _, e := Spool(ctx, t.TempDir()+"/private", 1, r); !errors.Is(e, context.DeadlineExceeded) {
		t.Fatal("blocked read did not cancel", e)
	}
}

type uploadRepoStub struct {
	ticket               files.UploadTicket
	acquireError         error
	receiveError         error
	failedTicket         files.UploadTicket
	failure, seal, begin int
	claim                *files.RecoveryTicket
	evidence             files.RecoveryEvidence
}

func (r *uploadRepoStub) AcquireFileUpload(context.Context, access.TrustedIdentity, string, string) (files.UploadTicket, error) {
	return r.ticket, r.acquireError
}
func (r *uploadRepoStub) RenewFileUpload(_ context.Context, _ access.TrustedIdentity, t files.UploadTicket) (files.UploadTicket, error) {
	return t, nil
}
func (r *uploadRepoStub) ReceiveFileUpload(_ context.Context, _ access.TrustedIdentity, t files.UploadTicket, m files.Measurement) (files.UploadTicket, error) {
	if r.receiveError != nil {
		return files.UploadTicket{}, r.receiveError
	}
	if t.Measurement != nil && *t.Measurement != m {
		return t, files.ErrUploadConflict
	}
	t.Measurement = &m
	if t.Phase == files.UploadReceiving {
		t.Phase = files.UploadReceived
	}
	return t, nil
}
func (r *uploadRepoStub) BeginFileObjectWrite(_ context.Context, _ access.TrustedIdentity, t files.UploadTicket) (files.UploadTicket, error) {
	r.begin++
	t.Phase = files.UploadStoring
	return t, nil
}
func (r *uploadRepoStub) RememberFileObject(_ context.Context, _ access.TrustedIdentity, t files.UploadTicket, v string) (files.UploadTicket, error) {
	t.Phase = files.UploadRecovered
	t.ObjectVersionID = v
	return t, nil
}
func (r *uploadRepoStub) SealFileUpload(_ context.Context, _ access.TrustedIdentity, t files.UploadTicket) (files.Metadata, error) {
	r.seal++
	m := t.File
	m.State = files.StateUploaded
	return m, nil
}
func (r *uploadRepoStub) FailFileUpload(_ context.Context, _ access.TrustedIdentity, ticket files.UploadTicket, _ string) error {
	r.failedTicket = ticket
	r.failure++
	return nil
}
func (r *uploadRepoStub) ClaimFileUploadRecovery(context.Context, string) (files.RecoveryTicket, bool, error) {
	if r.claim == nil {
		return files.RecoveryTicket{}, false, nil
	}
	return *r.claim, true, nil
}
func (r *uploadRepoStub) CompleteFileUploadRecovery(_ context.Context, _ files.RecoveryTicket, e files.RecoveryEvidence) error {
	r.evidence = e
	return nil
}

type objectStub struct {
	puts     int
	body     []byte
	refs     []objectstore.VersionRef
	putError error
}

func (s *objectStub) ValidateCapabilities(context.Context) error { return nil }
func (s *objectStub) PutVersion(_ context.Context, l objectstore.Location, _ string, _ files.Measurement, r io.ReadSeeker) (objectstore.VersionRef, error) {
	s.puts++
	b, _ := io.ReadAll(r)
	s.body = b
	return objectstore.VersionRef{Location: l, VersionID: "fixed"}, s.putError
}
func (s *objectStub) ReadVersion(context.Context, objectstore.VersionRef) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(s.body)), nil
}
func (s *objectStub) FindAttemptVersions(context.Context, objectstore.Location, string, int) ([]objectstore.VersionRef, error) {
	return s.refs, nil
}
func transferTicket() files.UploadTicket {
	return files.UploadTicket{File: files.Metadata{ID: testID, CreateParams: files.CreateParams{TenantID: testID, DeclaredSizeBytes: 1}, UploadExpiresAt: time.Now().Add(time.Hour)}, AttemptID: testID, LeaseToken: testID, OwnerID: testID, Phase: files.UploadReceiving, LeaseExpiresAt: time.Now().Add(3 * time.Minute)}
}
func TestFileTransferUploadBounds(t *testing.T) {
	for _, tc := range []struct {
		body string
		want error
	}{{"x", nil}, {"", files.ErrInvalidFileSize}, {"xx", files.ErrFileTooLarge}} {
		repo := &uploadRepoStub{ticket: transferTicket()}
		obj := &objectStub{}
		dir := t.TempDir() + "/private"
		s, e := NewService(repo, obj, dir, testID)
		if e != nil {
			t.Fatal(e)
		}
		_, e = s.Upload(context.Background(), access.TrustedIdentity{}, testID, strings.NewReader(tc.body))
		if !errors.Is(e, tc.want) {
			t.Fatal(e)
		}
		want := 0
		if tc.want == nil {
			want = 1
		}
		if obj.puts != want || repo.seal != want {
			t.Fatal(obj.puts, repo.seal)
		}
		entries, _ := os.ReadDir(dir)
		if len(entries) != 0 {
			t.Fatal("spool leak")
		}
	}
}
func TestFileTransferRecoveryNoReput(t *testing.T) {
	u := transferTicket()
	m := files.Measurement{SizeBytes: 1, SHA256: sha256.Sum256([]byte("x")), DetectedMediaType: "text/plain"}
	u.Measurement = &m
	u.Phase = files.UploadRecovered
	u.ObjectVersionID = "fixed"
	repo := &uploadRepoStub{ticket: u}
	obj := &objectStub{body: []byte("x")}
	s, e := NewService(repo, obj, t.TempDir()+"/private", testID)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.Upload(context.Background(), access.TrustedIdentity{}, testID, strings.NewReader("x")); e != nil || obj.puts != 0 || repo.begin != 0 || repo.seal != 1 {
		t.Fatal(e, obj.puts, repo)
	}
	if _, e = s.Upload(context.Background(), access.TrustedIdentity{}, testID, strings.NewReader("y")); !errors.Is(e, files.ErrUploadConflict) || obj.puts != 0 {
		t.Fatal(e, obj.puts)
	}
}

type countReader struct{ n int }

func (r *countReader) Read([]byte) (int, error) { r.n++; return 0, io.EOF }
func TestFileTransferSealedAndFullSlotsReadNothing(t *testing.T) {
	repo := &uploadRepoStub{ticket: transferTicket(), acquireError: files.ErrAlreadyUploaded}
	obj := &objectStub{}
	s, e := NewService(repo, obj, t.TempDir()+"/private", testID)
	if e != nil {
		t.Fatal(e)
	}
	r := &countReader{}
	if _, e = s.Upload(context.Background(), access.TrustedIdentity{}, testID, r); !errors.Is(e, files.ErrAlreadyUploaded) || r.n != 0 || obj.puts != 0 {
		t.Fatal(e, r.n, obj.puts)
	}
	for i := 0; i < 4; i++ {
		s.slots <- struct{}{}
	}
	if _, e = s.Upload(context.Background(), access.TrustedIdentity{}, testID, r); !errors.Is(e, ErrNodeBusy) || r.n != 0 {
		t.Fatal(e, r.n)
	}
}

func TestFileTransferErrorRetainsLeaseToken(t *testing.T) {
	repo := &uploadRepoStub{ticket: transferTicket(), receiveError: files.ErrInvalidIdentity}
	obj := &objectStub{}
	s, e := NewService(repo, obj, t.TempDir()+"/private", testID)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.Upload(context.Background(), access.TrustedIdentity{}, testID, strings.NewReader("x")); !errors.Is(e, files.ErrInvalidIdentity) || repo.failedTicket.LeaseToken != testID {
		t.Fatal(e, repo.failedTicket.LeaseToken)
	}
}

type transferRepeat struct{}

func (transferRepeat) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'x'
	}
	return len(p), nil
}
func TestFileTransferSpoolMemoryAndDirectory(t *testing.T) {
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	f, _, e := Spool(context.Background(), t.TempDir()+"/private", files.MaxFileSizeBytes, io.LimitReader(transferRepeat{}, files.MaxFileSizeBytes))
	if e != nil {
		t.Fatal(e)
	}
	runtime.ReadMemStats(&after)
	name := f.Name()
	f.Close()
	os.Remove(name)
	if after.TotalAlloc-before.TotalAlloc > 2*1024*1024 {
		t.Fatal("whole content allocated", after.TotalAlloc-before.TotalAlloc)
	}
	dir := t.TempDir() + "/symlink"
	if e = os.Symlink(t.TempDir(), dir); e != nil {
		t.Fatal(e)
	}
	if _, _, e = Spool(context.Background(), dir, 1, strings.NewReader("x")); !errors.Is(e, files.ErrDependencyUnavailable) {
		t.Fatal("symlink accepted", e)
	}
}
