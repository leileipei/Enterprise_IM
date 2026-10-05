package filedownload

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"sync"
	"syscall"
	"time"

	"github.com/leileipei/Enterprise_IM/internal/access"
	"github.com/leileipei/Enterprise_IM/internal/objectstore"
)

const DownloadTimeout = 60 * time.Second
const DownloadCheckTimeout = time.Second

var errDownloadIntegrity = errors.New("download integrity mismatch")

type Service struct {
	repo           Repository
	objects        objectstore.Store
	owner          string
	rootPath       string
	root           *os.Root
	lock           *os.File
	slots          chan struct{}
	lifecycle      sync.Mutex
	closed, failed bool
	active         sync.WaitGroup
	closeOnce      sync.Once
	closeErr       error
	cleanupErr     error
}
type Prepared struct {
	service                      *Service
	ticket                       Ticket
	ctx                          context.Context
	cancel                       context.CancelFunc
	file                         *os.File
	dirname                      string
	dirinfo                      os.FileInfo
	stop                         func() bool
	mu                           sync.Mutex
	closed, authorized, finished bool
	closeOnce                    sync.Once
	closeErr                     error
}

func NewService(repo Repository, objects objectstore.Store, spoolRoot, ownerID string) (*Service, error) {
	if repo == nil || objects == nil || !canonicalID.MatchString(ownerID) {
		return nil, ErrUnavailable
	}
	dir, e := spoolAbsolute(spoolRoot)
	if e != nil {
		return nil, ErrUnavailable
	}
	root, lock, e := claimDownloadSpool(dir, ownerID)
	if e != nil {
		return nil, ErrUnavailable
	}
	return &Service{repo: repo, objects: objects, owner: ownerID, rootPath: dir, root: root, lock: lock, slots: make(chan struct{}, 4)}, nil
}
func (s *Service) admit() error {
	s.lifecycle.Lock()
	defer s.lifecycle.Unlock()
	if s.closed || s.failed {
		return ErrUnavailable
	}
	select {
	case s.slots <- struct{}{}:
		s.active.Add(1)
		return nil
	default:
		return ErrLimit
	}
}
func (s *Service) Close() error {
	s.closeOnce.Do(func() {
		s.lifecycle.Lock()
		s.closed = true
		s.lifecycle.Unlock()
		s.active.Wait()
		s.closeErr = errors.Join(s.cleanupErr, s.root.Close(), s.lock.Close())
	})
	return s.closeErr
}
func (s *Service) Prepare(parent context.Context, id access.TrustedIdentity, fileID string) (prepared *Prepared, err error) {
	if e := s.admit(); e != nil {
		return nil, e
	}
	ctx, cancel := context.WithTimeout(parent, DownloadTimeout)
	p := &Prepared{service: s, ctx: ctx, cancel: cancel}
	defer func() {
		if err != nil {
			if p.ticket.SessionID != "" {
				reason := "dependency_unavailable"
				if errors.Is(err, errDownloadIntegrity) {
					reason = "integrity_mismatch"
				} else if errors.Is(ctx.Err(), context.DeadlineExceeded) {
					reason = "timeout"
				} else if errors.Is(ctx.Err(), context.Canceled) {
					reason = "client_disconnected"
				}
				settle, stop := context.WithTimeout(context.WithoutCancel(parent), 5*time.Second)
				e := s.repo.FinishFileDownload(settle, p.ticket, Result{Outcome: "interrupted", Reason: reason})
				stop()
				if e != nil {
					err = errors.Join(ErrUnavailable, err, e)
				}
			}
			if e := p.Close(); e != nil {
				err = errors.Join(ErrUnavailable, err, e)
			}
		}
	}()
	deadline, _ := ctx.Deadline()
	ticket, e := s.repo.BeginFileDownload(ctx, id, fileID, s.owner, deadline)
	if e != nil {
		return nil, e
	}
	p.ticket = ticket
	if ValidateTicket(ticket) != nil || ticket.OwnerID != s.owner || ticket.Identity != id || ticket.File.ID != fileID || ticket.File.ObjectVersionID == "null" || !time.Now().Before(ticket.Deadline) {
		return nil, ErrUnavailable
	}
	// Own snapshot slices and pointer values; an adapter cannot mutate the seal
	// while an object is being read into the private spool.
	size := *ticket.File.ActualSizeBytes
	p.ticket.File.ActualSizeBytes = &size
	p.ticket.File.SHA256 = append([]byte(nil), ticket.File.SHA256...)
	p.ticket.File.ScanSHA256 = append([]byte(nil), ticket.File.ScanSHA256...)
	if ticket.Deadline.Before(deadline) {
		cancel()
		ctx, cancel = context.WithDeadline(parent, ticket.Deadline)
		p.ctx, p.cancel = ctx, cancel
	}
	if e = s.root.Mkdir(ticket.SessionID, 0700); e != nil {
		return nil, ErrUnavailable
	}
	p.dirname = ticket.SessionID
	p.dirinfo, e = s.root.Lstat(p.dirname)
	if e != nil || !ownedInfo(p.dirinfo, true, 0) {
		return nil, ErrUnavailable
	}
	child, e := s.root.OpenRoot(p.dirname)
	if e != nil {
		return nil, ErrUnavailable
	}
	defer child.Close()
	if e = writeSpoolManifest(child, spoolSessionName, spoolManifest{Purpose: "enterprise-im-download-session-v1", Owner: s.owner, Session: ticket.SessionID, File: fileID, Size: size}); e != nil {
		return nil, ErrUnavailable
	}
	p.file, e = child.OpenFile(spoolContentName, os.O_RDWR|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0600)
	if e != nil {
		return nil, ErrUnavailable
	}
	if info, e := p.file.Stat(); e != nil || !ownedInfo(info, false, 0) {
		return nil, ErrUnavailable
	}
	body, e := s.objects.ReadVersion(ctx, objectstore.VersionRef{Location: objectstore.Location{TenantID: id.TenantID, FileID: fileID}, VersionID: ticket.File.ObjectVersionID})
	if e != nil || body == nil {
		return nil, errors.Join(ErrUnavailable, e)
	}
	stopBody := context.AfterFunc(ctx, func() { body.Close() })
	defer stopBody()
	h := sha256.New()
	n, copyErr := io.CopyBuffer(io.MultiWriter(p.file, h), io.LimitReader(downloadContextReader{ctx: ctx, r: body}, size+1), make([]byte, 32768))
	closeErr := body.Close()
	if copyErr != nil || closeErr != nil || ctx.Err() != nil {
		return nil, errors.Join(ErrUnavailable, copyErr, closeErr, ctx.Err())
	}
	if n != size || !bytes.Equal(h.Sum(nil), p.ticket.File.SHA256) {
		return nil, errors.Join(ErrUnavailable, errDownloadIntegrity)
	}
	if e = p.file.Sync(); e != nil {
		return nil, ErrUnavailable
	}
	if _, e = p.file.Seek(0, io.SeekStart); e != nil {
		return nil, ErrUnavailable
	}
	if ctx.Err() != nil {
		return nil, errors.Join(ErrUnavailable, ctx.Err())
	}
	stop := context.AfterFunc(ctx, func() { p.Close() })
	p.mu.Lock()
	p.stop = stop
	closed := p.closed
	p.mu.Unlock()
	if closed {
		stop()
		return nil, ErrUnavailable
	}
	return p, nil
}

type downloadContextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r downloadContextReader) Read(b []byte) (int, error) {
	if e := r.ctx.Err(); e != nil {
		return 0, e
	}
	n, e := r.r.Read(b)
	if x := r.ctx.Err(); x != nil {
		return n, x
	}
	return n, e
}
func (s *Service) owned(p *Prepared) bool {
	return p != nil && p.service == s && p.ticket.SessionID != "" && ValidateTicket(p.ticket) == nil
}
func (p *Prepared) operationContext(parent context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithDeadline(parent, p.ticket.Deadline)
	stop := context.AfterFunc(p.ctx, cancel)
	return ctx, func() { stop(); cancel() }
}
func (s *Service) Authorize(parent context.Context, id access.TrustedIdentity, p *Prepared) error {
	if !s.owned(p) || id != p.ticket.Identity {
		return ErrNotFound
	}
	p.mu.Lock()
	closed := p.closed
	p.mu.Unlock()
	if closed {
		return ErrUnavailable
	}
	ctx, cancel := p.operationContext(parent)
	defer cancel()
	if e := s.repo.AuthorizeFileDownload(ctx, id, p.ticket); e != nil {
		return e
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed || p.ctx.Err() != nil {
		return ErrUnavailable
	}
	p.authorized = true
	return nil
}
func (s *Service) Check(parent context.Context, id access.TrustedIdentity, p *Prepared) error {
	if !s.owned(p) || id != p.ticket.Identity {
		return ErrNotFound
	}
	p.mu.Lock()
	valid := !p.closed && p.authorized && !p.finished
	p.mu.Unlock()
	if !valid {
		return ErrUnavailable
	}
	ctx, cancel := p.operationContext(parent)
	defer cancel()
	checked, stop := context.WithTimeout(ctx, DownloadCheckTimeout)
	defer stop()
	return s.repo.CheckFileDownload(checked, id, p.ticket)
}
func (s *Service) Finish(ctx context.Context, p *Prepared, result Result) error {
	if !s.owned(p) {
		return ErrNotFound
	}
	if e := ValidateResult(p.SizeBytes(), result); e != nil {
		return e
	}
	if e := s.repo.FinishFileDownload(ctx, p.ticket, result); e != nil {
		return e
	}
	p.mu.Lock()
	p.finished = true
	p.mu.Unlock()
	return nil
}
func (p *Prepared) SizeBytes() int64 {
	if p == nil || p.ticket.File.ActualSizeBytes == nil {
		return 0
	}
	return *p.ticket.File.ActualSizeBytes
}
func (p *Prepared) Filename() string {
	if p == nil {
		return ""
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.authorized {
		return ""
	}
	return p.ticket.File.OriginalFilename
}
func (p *Prepared) Read(b []byte) (int, error) {
	if p == nil {
		return 0, ErrUnavailable
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed || !p.authorized || p.finished || p.file == nil {
		return 0, ErrUnavailable
	}
	if e := p.ctx.Err(); e != nil {
		return 0, errors.Join(ErrUnavailable, e)
	}
	return p.file.Read(b)
}
func (p *Prepared) Close() error {
	if p == nil || p.service == nil {
		return ErrUnavailable
	}
	p.closeOnce.Do(func() {
		p.mu.Lock()
		p.closed = true
		var closeErr error
		if p.file != nil {
			closeErr = p.file.Close()
		}
		stop := p.stop
		p.mu.Unlock()
		if stop != nil {
			stop()
		}
		p.cancel()
		if p.dirname != "" {
			closeErr = errors.Join(closeErr, removeDownloadSpool(p.service.root, p.dirname, p.dirinfo, p.SizeBytes()))
		}
		p.closeErr = closeErr
		if closeErr != nil {
			p.service.lifecycle.Lock()
			p.service.failed = true
			p.service.cleanupErr = errors.Join(p.service.cleanupErr, closeErr)
			p.service.lifecycle.Unlock()
		}
		<-p.service.slots
		p.service.active.Done()
	})
	return p.closeErr
}
