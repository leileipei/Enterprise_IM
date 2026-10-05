package filetransfer

import (
	"context"
	"crypto/sha256"
	"errors"
	"github.com/leileipei/Enterprise_IM/internal/access"
	"github.com/leileipei/Enterprise_IM/internal/files"
	"github.com/leileipei/Enterprise_IM/internal/objectstore"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"
)

const ReceiveTimeout = 60 * time.Second
const UploadTimeout = 150 * time.Second
const UploadRenewInterval = 15 * time.Second

var ErrNodeBusy = errors.New("upload node capacity exhausted")

type UploadRepository interface {
	AcquireFileUpload(context.Context, access.TrustedIdentity, string, string) (files.UploadTicket, error)
	RenewFileUpload(context.Context, access.TrustedIdentity, files.UploadTicket) (files.UploadTicket, error)
	ReceiveFileUpload(context.Context, access.TrustedIdentity, files.UploadTicket, files.Measurement) (files.UploadTicket, error)
	BeginFileObjectWrite(context.Context, access.TrustedIdentity, files.UploadTicket) (files.UploadTicket, error)
	RememberFileObject(context.Context, access.TrustedIdentity, files.UploadTicket, string) (files.UploadTicket, error)
	SealFileUpload(context.Context, access.TrustedIdentity, files.UploadTicket) (files.Metadata, error)
	FailFileUpload(context.Context, access.TrustedIdentity, files.UploadTicket, string) error
	ClaimFileUploadRecovery(context.Context, string) (files.RecoveryTicket, bool, error)
	CompleteFileUploadRecovery(context.Context, files.RecoveryTicket, files.RecoveryEvidence) error
}
type Service struct {
	Repo              UploadRepository
	Objects           objectstore.Store
	SpoolDir, OwnerID string
	slots             chan struct{}
	spoolLock         *os.File
	lifecycle         sync.Mutex
	closed            bool
	active            sync.WaitGroup
	closeOnce         sync.Once
	closeErr          error
}

func NewService(repo UploadRepository, objects objectstore.Store, dir, owner string) (*Service, error) {
	if repo == nil || objects == nil || !regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`).MatchString(owner) {
		return nil, files.ErrDependencyUnavailable
	}
	abs, e := filepath.Abs(dir)
	if e != nil || dir == "" {
		return nil, files.ErrDependencyUnavailable
	}
	lock, e := claimSpoolDir(abs)
	if e != nil {
		return nil, e
	}
	return &Service{Repo: repo, Objects: objects, SpoolDir: abs, OwnerID: owner, slots: make(chan struct{}, 4), spoolLock: lock}, nil
}
func readMeasurement(ctx context.Context, store objectstore.Store, ref objectstore.VersionRef, m files.Measurement) (err error) {
	if ref.VersionID == "" || ref.VersionID == "null" {
		return files.ErrDependencyUnavailable
	}
	body, e := store.ReadVersion(ctx, ref)
	if e != nil {
		return files.ErrDependencyUnavailable
	}
	defer func() {
		if e := body.Close(); e != nil {
			err = files.ErrDependencyUnavailable
		}
	}()
	h := sha256.New()
	n, e := io.CopyBuffer(h, io.LimitReader(contextReader{ctx, body}, m.SizeBytes+1), make([]byte, 32768))
	if e != nil || n != m.SizeBytes {
		return files.ErrDependencyUnavailable
	}
	var d [32]byte
	copy(d[:], h.Sum(nil))
	if d != m.SHA256 {
		return files.ErrUploadConflict
	}
	return nil
}
func (s *Service) Upload(parent context.Context, id access.TrustedIdentity, fileID string, body io.Reader) (result files.Metadata, err error) {
	if !s.beginUpload() {
		return result, files.ErrDependencyUnavailable
	}
	defer s.active.Done()
	select {
	case s.slots <- struct{}{}:
		defer func() { <-s.slots }()
	default:
		return result, ErrNodeBusy
	}
	ctx, cancel := context.WithTimeout(parent, UploadTimeout)
	defer cancel()
	ticket, err := s.Repo.AcquireFileUpload(ctx, id, fileID, s.OwnerID)
	if err != nil {
		return result, err
	}
	defer func() {
		if err != nil {
			reason := "receive_failed"
			if ticket.Phase == files.UploadStoring || ticket.Phase == files.UploadRecoveryPending {
				reason = "object_write_uncertain"
			} else if ticket.Phase == files.UploadRecovered {
				reason = "object_read_failed"
			}
			if errors.Is(err, files.ErrLeaseLost) {
				reason = "lease_lost"
			}
			failureCtx, stop := context.WithTimeout(context.WithoutCancel(parent), 5*time.Second)
			defer stop()
			if failure := s.Repo.FailFileUpload(failureCtx, id, ticket, reason); failure != nil && !errors.Is(failure, files.ErrLeaseLost) {
				err = files.ErrDependencyUnavailable
			}
		}
	}()
	renewCtx, stopRenew := context.WithCancel(ctx)
	done := make(chan struct{})
	lost := make(chan error, 1)
	initial := ticket
	go func() {
		defer close(done)
		timer := time.NewTicker(UploadRenewInterval)
		defer timer.Stop()
		for {
			select {
			case <-renewCtx.Done():
				return
			case <-timer.C:
				if _, e := s.Repo.RenewFileUpload(renewCtx, id, initial); e != nil {
					lost <- files.ErrLeaseLost
					cancel()
					return
				}
			}
		}
	}()
	stopHeartbeat := func() { stopRenew(); <-done }
	defer stopHeartbeat()
	receiveCtx, stopReceive := context.WithTimeout(ctx, ReceiveTimeout)
	f, m, e := Spool(receiveCtx, s.SpoolDir, ticket.File.DeclaredSizeBytes, body)
	stopReceive()
	if e != nil {
		select {
		case e = <-lost:
		default:
		}
		return result, e
	}
	defer func() {
		closeErr := f.Close()
		removeErr := os.Remove(f.Name())
		if err == nil && (closeErr != nil || removeErr != nil) {
			err = files.ErrDependencyUnavailable
		}
	}()
	received, e := s.Repo.ReceiveFileUpload(ctx, id, ticket, m)
	if e != nil {
		return result, e
	}
	ticket = received
	location := objectstore.Location{TenantID: ticket.File.TenantID, FileID: ticket.File.ID}
	if ticket.Phase != files.UploadRecovered {
		storing, e := s.Repo.BeginFileObjectWrite(ctx, id, ticket)
		if e != nil {
			return result, e
		}
		ticket = storing
		ref, e := s.Objects.PutVersion(ctx, location, ticket.AttemptID, m, f)
		if e != nil {
			return result, files.ErrDependencyUnavailable
		}
		if ref.Location != location {
			return result, files.ErrDependencyUnavailable
		}
		remembered, e := s.Repo.RememberFileObject(ctx, id, ticket, ref.VersionID)
		if e != nil {
			return result, e
		}
		ticket = remembered
	}
	if e = readMeasurement(ctx, s.Objects, objectstore.VersionRef{Location: location, VersionID: ticket.ObjectVersionID}, m); e != nil {
		return result, e
	}
	stopRenew()
	<-done
	select {
	case e = <-lost:
		return result, e
	default:
	}
	if e = ctx.Err(); e != nil {
		return result, files.ErrDependencyUnavailable
	}
	return s.Repo.SealFileUpload(ctx, id, ticket)
}
