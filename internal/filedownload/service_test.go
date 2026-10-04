package filedownload

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/leileipei/Enterprise_IM/internal/access"
	"github.com/leileipei/Enterprise_IM/internal/files"
	"github.com/leileipei/Enterprise_IM/internal/objectstore"
)

const downloadTestID = "00000000-0000-4000-8000-000000000001"

var downloadTestActor = access.TrustedIdentity{TenantID: downloadTestID, UserID: downloadTestID, ActingMembershipID: downloadTestID}

type downloadRepo struct {
	mu                 sync.Mutex
	seq                int
	data               []byte
	results            []Result
	authErr, finishErr error
	filename           string
	version            string
}

func (r *downloadRepo) BeginFileDownload(_ context.Context, id access.TrustedIdentity, fid, owner string, deadline time.Time) (Ticket, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seq++
	now := time.Now().UTC().Add(-time.Minute)
	up := now.Add(time.Second)
	scan := now.Add(2 * time.Second)
	size := int64(len(r.data))
	hash := sha256.Sum256(r.data)
	name := r.filename
	if name == "" {
		name = "中文报告.txt"
	}
	m := files.Metadata{CreateParams: files.CreateParams{TenantID: id.TenantID, ConversationID: downloadTestID, UploaderUserID: downloadTestID, UploaderMembershipID: downloadTestID, UploadRequestID: downloadTestID, OriginalFilename: name, DeclaredMediaType: "text/plain", DeclaredSizeBytes: size}, ID: fid, RequestDigest: make([]byte, 32), State: files.StateReady, StateVersion: 3, CreatedAt: now, UpdatedAt: scan, UploadExpiresAt: now.Add(15 * time.Minute), ObjectKey: "tenants/" + id.TenantID + "/files/" + fid, ObjectVersionID: "fixed-v1", DetectedMediaType: "text/plain", ActualSizeBytes: &size, SHA256: hash[:], UploadedAt: &up, ScanJobID: downloadTestID, ScanEngine: "clamav", ScanDefinitionVersion: "fresh", ScannedAt: &scan, ScanSHA256: append([]byte(nil), hash[:]...)}
	if r.version != "" {
		m.ObjectVersionID = r.version
	}
	session := fmt.Sprintf("00000000-0000-4000-8000-%012x", r.seq+100)
	return Ticket{SessionID: session, OwnerID: owner, LeaseToken: downloadTestID, Identity: id, File: m, MessageID: downloadTestID, MessageSeq: 1, Deadline: deadline, LeaseExpiresAt: deadline}, nil
}
func (r *downloadRepo) AuthorizeFileDownload(context.Context, access.TrustedIdentity, Ticket) error {
	return r.authErr
}
func (r *downloadRepo) CheckFileDownload(context.Context, access.TrustedIdentity, Ticket) error {
	return nil
}
func (r *downloadRepo) FinishFileDownload(_ context.Context, _ Ticket, result Result) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.results = append(r.results, result)
	return r.finishErr
}

type downloadObjects struct {
	data    []byte
	err     error
	seen    objectstore.VersionRef
	entered chan struct{}
	gate    chan struct{}
	reader  io.ReadCloser
}

func (o *downloadObjects) ValidateCapabilities(context.Context) error { return nil }
func (o *downloadObjects) PutVersion(context.Context, objectstore.Location, string, files.Measurement, io.ReadSeeker) (objectstore.VersionRef, error) {
	return objectstore.VersionRef{}, errors.New("unused")
}
func (o *downloadObjects) FindAttemptVersions(context.Context, objectstore.Location, string, int) ([]objectstore.VersionRef, error) {
	return nil, errors.New("unused")
}
func (o *downloadObjects) ReadVersion(ctx context.Context, ref objectstore.VersionRef) (io.ReadCloser, error) {
	if o.entered != nil {
		o.entered <- struct{}{}
		select {
		case <-o.gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	} else {
		o.seen = ref
	}
	if o.err != nil {
		return nil, o.err
	}
	if o.reader != nil {
		return o.reader, nil
	}
	return io.NopCloser(bytes.NewReader(o.data)), nil
}
func TestFileDownloadPrepareFailureSettlement(t *testing.T) {
	for _, kind := range []string{"read", "short", "extra", "hash", "timeout", "body_timeout", "null_version", "audit_gap"} {
		t.Run(kind, func(t *testing.T) {
			repo := &downloadRepo{data: []byte("verified bytes")}
			obj := &downloadObjects{data: append([]byte(nil), repo.data...)}
			switch kind {
			case "read":
				obj.err = errors.New("read failed")
			case "short":
				obj.data = obj.data[:1]
			case "extra":
				obj.data = append(obj.data, 'x')
			case "hash":
				obj.data[0] ^= 1
			case "timeout":
				obj.entered = make(chan struct{}, 1)
				obj.gate = make(chan struct{})
			case "body_timeout":
				obj.reader = &waitingDownloadBody{done: make(chan struct{})}
			case "null_version":
				repo.version = "null"
			case "audit_gap":
				obj.err = errors.New("read failed")
				repo.finishErr = errors.New("database unavailable")
			}
			s, e := NewService(repo, obj, filepath.Join(t.TempDir(), "spool"), downloadTestID)
			if e != nil {
				t.Fatal(e)
			}
			defer s.Close()
			ctx := context.Background()
			if kind == "timeout" || kind == "body_timeout" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 25*time.Millisecond)
				defer cancel()
			}
			p, e := s.Prepare(ctx, downloadTestActor, downloadTestID)
			if p != nil || !errors.Is(e, ErrUnavailable) {
				t.Fatal("unverified object exposed", p, e)
			}
			if len(repo.results) != 1 || repo.results[0].Outcome != "interrupted" || repo.results[0].BytesWritten != 0 {
				t.Fatal("failure not settled", repo.results)
			}
		})
	}
}
func TestFileDownloadSpoolIntegrity(t *testing.T) {
	repo := &downloadRepo{data: []byte("verified bytes")}
	obj := &downloadObjects{data: repo.data}
	dir := filepath.Join(t.TempDir(), "spool")
	s, e := NewService(repo, obj, dir, downloadTestID)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	p, e := s.Prepare(context.Background(), downloadTestActor, downloadTestID)
	if e != nil {
		t.Fatal(e)
	}
	defer p.Close()
	if _, e = p.Read(make([]byte, 1)); e == nil {
		t.Fatal("read without authorization")
	}
	if e = s.Authorize(context.Background(), downloadTestActor, p); e != nil {
		t.Fatal(e)
	}
	got, e := io.ReadAll(p)
	if e != nil || !bytes.Equal(got, repo.data) || p.SizeBytes() != int64(len(got)) || p.Filename() != repo.filename && p.Filename() != "中文报告.txt" {
		t.Fatal(string(got), e)
	}
	if obj.seen.VersionID != "fixed-v1" || obj.seen.Location.FileID != downloadTestID {
		t.Fatal("not fixed version", obj.seen)
	}
	if e = s.Finish(context.Background(), p, Result{Outcome: "completed", Reason: "completed", BytesWritten: int64(len(got))}); e != nil {
		t.Fatal(e)
	}
}
func TestFileDownloadSpoolBounds(t *testing.T) {
	repo := &downloadRepo{data: []byte("x")}
	obj := &downloadObjects{data: repo.data, entered: make(chan struct{}, 4), gate: make(chan struct{})}
	s, e := NewService(repo, obj, filepath.Join(t.TempDir(), "spool"), downloadTestID)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	out := make(chan *Prepared, 4)
	errs := make(chan error, 4)
	for i := 0; i < 4; i++ {
		go func() {
			p, e := s.Prepare(context.Background(), downloadTestActor, downloadTestID)
			out <- p
			errs <- e
		}()
	}
	for i := 0; i < 4; i++ {
		select {
		case <-obj.entered:
		case <-time.After(3 * time.Second):
			t.Fatal("node preparations not started")
		}
	}
	if p, e := s.Prepare(context.Background(), downloadTestActor, downloadTestID); p != nil || !errors.Is(e, ErrLimit) {
		t.Fatal("fifth admitted", p, e)
	}
	close(obj.gate)
	for i := 0; i < 4; i++ {
		p := <-out
		if e := <-errs; e != nil {
			t.Fatal(e)
		}
		if e := p.Close(); e != nil {
			t.Fatal(e)
		}
	}
}
func TestFileDownloadSpoolInvalidPrepared(t *testing.T) {
	r := &downloadRepo{data: []byte("x")}
	s, e := NewService(r, &downloadObjects{data: r.data}, filepath.Join(t.TempDir(), "spool"), downloadTestID)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	if e = s.Authorize(context.Background(), downloadTestActor, &Prepared{}); e == nil {
		t.Fatal("externally constructed prepared authorized")
	}
	if e = s.Finish(context.Background(), &Prepared{}, Result{Outcome: "completed", Reason: "completed", BytesWritten: 1}); e == nil {
		t.Fatal("externally constructed prepared settled")
	}
}
func TestFileDownloadSpoolPathsAndPermissions(t *testing.T) {
	r := &downloadRepo{data: []byte("x")}
	dir := filepath.Join(t.TempDir(), "spool")
	s, e := NewService(r, &downloadObjects{data: r.data}, dir, downloadTestID)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	p, e := s.Prepare(context.Background(), downloadTestActor, downloadTestID)
	if e != nil {
		t.Fatal(e)
	}
	if st, e := os.Stat(dir); e != nil || st.Mode().Perm() != 0700 {
		t.Fatal(st, e)
	}
	found := 0
	if e = filepath.WalkDir(dir, func(path string, d os.DirEntry, e error) error {
		if e != nil {
			return e
		}
		st, e := d.Info()
		if e != nil {
			return e
		}
		if d.IsDir() {
			if st.Mode().Perm() != 0700 {
				t.Fatal(path, st.Mode())
			}
		} else {
			if st.Mode().Perm() != 0600 {
				t.Fatal(path, st.Mode())
			}
			found++
		}
		return nil
	}); e != nil || found < 2 {
		t.Fatal(found, e)
	}
	if e = p.Close(); e != nil {
		t.Fatal(e)
	}
	r.filename = "../../outside.txt"
	if p, e = s.Prepare(context.Background(), downloadTestActor, downloadTestID); p != nil || e == nil {
		t.Fatal("filename path used", p, e)
	}
}

type waitingDownloadBody struct {
	done chan struct{}
	once sync.Once
}

func (b *waitingDownloadBody) Read([]byte) (int, error) { <-b.done; return 0, io.ErrClosedPipe }
func (b *waitingDownloadBody) Close() error             { b.once.Do(func() { close(b.done) }); return nil }

type endlessDownloadBody struct{ read int64 }

func (b *endlessDownloadBody) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'x'
	}
	b.read += int64(len(p))
	return len(p), nil
}
func (b *endlessDownloadBody) Close() error { return nil }
func TestFileDownloadSpoolBoundsMaxRead(t *testing.T) {
	r := &downloadRepo{data: bytes.Repeat([]byte("x"), int(files.MaxFileSizeBytes))}
	body := &endlessDownloadBody{}
	dir := filepath.Join(t.TempDir(), "spool")
	s, e := NewService(r, &downloadObjects{reader: body}, dir, downloadTestID)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	if p, e := s.Prepare(context.Background(), downloadTestActor, downloadTestID); p != nil || e == nil {
		t.Fatal(p, e)
	}
	if body.read != files.MaxFileSizeBytes+1 {
		t.Fatal("unbounded read", body.read)
	}
	entries, e := os.ReadDir(dir)
	if e != nil || len(entries) != 1 {
		t.Fatal("disk budget not released", entries, e)
	}
}
func TestFileDownloadSpoolCloseCancellation(t *testing.T) {
	r := &downloadRepo{data: []byte("x")}
	dir := filepath.Join(t.TempDir(), "spool")
	s, e := NewService(r, &downloadObjects{data: r.data}, dir, downloadTestID)
	if e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	p, e := s.Prepare(ctx, downloadTestActor, downloadTestID)
	if e != nil {
		t.Fatal(e)
	}
	done := make(chan error, 1)
	go func() { done <- s.Close() }()
	select {
	case e := <-done:
		if e != nil {
			t.Fatal(e)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("expired preparation retained node")
	}
	if e = p.Close(); e != nil {
		t.Fatal(e)
	}
	entries, e := os.ReadDir(dir)
	if e != nil || len(entries) != 1 {
		t.Fatal(entries, e)
	}
}
