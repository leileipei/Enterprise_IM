package filetransfer

import (
	"bytes"
	"context"
	"errors"
	"github.com/leileipei/Enterprise_IM/internal/files"
	"github.com/leileipei/Enterprise_IM/internal/filescanner"
	"github.com/leileipei/Enterprise_IM/internal/objectstore"
	"os"
	"regexp"
	"slices"
	"sync"
	"time"
)

const ScanTimeout = 90 * time.Second
const ScanRenewInterval = 15 * time.Second

type ScanRepository interface {
	ClaimFileScan(context.Context, string) (files.ScanTicket, bool, error)
	RenewFileScan(context.Context, files.ScanTicket) (files.ScanTicket, error)
	CompleteFileScan(context.Context, files.ScanTicket, files.ScanDecision) error
	RecoverExpiredFileScans(context.Context, string, int) (int, error)
}
type ContentScanner interface {
	ValidateRuntime(context.Context) error
	Scan(context.Context, *os.File, files.Metadata) (files.ScanDecision, error)
}
type ScanWorker struct {
	Repo              ScanRepository
	Objects           objectstore.Store
	Scanner           ContentScanner
	SpoolDir, OwnerID string
	slot              sync.Mutex
}

func failedFileScan(reason string) files.ScanDecision {
	return files.ScanDecision{State: files.StateScanFailed, ReasonCode: reason}
}
func scannerFailure(d files.ScanDecision) files.ScanDecision {
	if d.State == files.StateScanFailed && slices.Contains([]string{"scan_error", "scanner_unavailable", "scanner_protocol_error", "scanner_limits", "definitions_stale", "structure_invalid", "object_mismatch", "object_read_failed"}, d.ReasonCode) {
		return failedFileScan(d.ReasonCode)
	}
	return failedFileScan("scanner_unavailable")
}
func (w *ScanWorker) RunOnce(parent context.Context) (found bool, err error) {
	if w.Repo == nil || w.Objects == nil || w.Scanner == nil || !regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`).MatchString(w.OwnerID) {
		return false, files.ErrDependencyUnavailable
	}
	if !w.slot.TryLock() {
		return false, ErrNodeBusy
	}
	defer w.slot.Unlock()
	if err = privateSpoolDir(w.SpoolDir); err != nil {
		return false, err
	}
	ctx, cancel := context.WithTimeout(parent, ScanTimeout)
	defer cancel()
	ticket, found, err := w.Repo.ClaimFileScan(ctx, w.OwnerID)
	if err != nil || !found {
		return found, err
	}
	renewCtx, stopRenew := context.WithCancel(ctx)
	defer stopRenew()
	done := make(chan struct{})
	lost := make(chan struct{}, 1)
	go func() {
		defer close(done)
		timer := time.NewTicker(ScanRenewInterval)
		defer timer.Stop()
		for {
			select {
			case <-renewCtx.Done():
				return
			case <-timer.C:
				if _, e := w.Repo.RenewFileScan(renewCtx, ticket); e != nil {
					lost <- struct{}{}
					cancel()
					return
				}
			}
		}
	}()
	stopHeartbeat := func() { stopRenew(); <-done }
	defer stopHeartbeat()
	decision := failedFileScan("object_read_failed")
	m := ticket.File
	if files.ValidateMetadata(m) != nil || m.State != files.StateScanning || m.ScanJobID != ticket.JobID || m.StateVersion != ticket.ClaimVersion || m.ActualSizeBytes == nil || len(m.SHA256) != 32 || m.ObjectVersionID == "" || m.ObjectVersionID == "null" {
		decision = failedFileScan("object_mismatch")
	} else {
		ref := objectstore.VersionRef{Location: objectstore.Location{TenantID: m.TenantID, FileID: m.ID}, VersionID: m.ObjectVersionID}
		body, e := w.Objects.ReadVersion(ctx, ref)
		if e == nil {
			f, measured, e := Spool(ctx, w.SpoolDir, *m.ActualSizeBytes, body)
			closeErr := body.Close()
			if f != nil {
				defer func() {
					name := f.Name()
					if e := f.Close(); e != nil {
						err = files.ErrDependencyUnavailable
					}
					if e := os.Remove(name); e != nil {
						err = files.ErrDependencyUnavailable
					}
				}()
			}
			if e != nil {
				if errors.Is(e, files.ErrInvalidFileSize) || errors.Is(e, files.ErrFileTooLarge) {
					decision = failedFileScan("object_mismatch")
				}
			} else if closeErr != nil {
				decision = failedFileScan("object_read_failed")
			} else if !bytes.Equal(measured.SHA256[:], m.SHA256) {
				decision = failedFileScan("object_mismatch")
			} else if e = w.Scanner.ValidateRuntime(ctx); e != nil {
				decision = failedFileScan("scanner_unavailable")
				if errors.Is(e, filescanner.ErrDefinitionsStale) {
					decision = failedFileScan("definitions_stale")
				}
			} else {
				decision, e = w.Scanner.Scan(ctx, f, m)
				if e != nil || ctx.Err() != nil {
					decision = scannerFailure(decision)
				} else if decision.State != files.StateReady && decision.State != files.StateRejected && decision.State != files.StateScanFailed {
					decision = failedFileScan("scanner_protocol_error")
				}
			}
		}
	}
	stopHeartbeat()
	select {
	case <-lost:
		return true, files.ErrLeaseLost
	default:
	}
	// Only failed evidence may be recorded after external I/O was canceled. The
	// repository still checks the original live lease at the actual write.
	completeCtx := ctx
	stop := func() {}
	if ctx.Err() != nil {
		decision = failedFileScan("scanner_unavailable")
		completeCtx, stop = context.WithTimeout(context.WithoutCancel(parent), 5*time.Second)
	}
	defer stop()
	if err = w.Repo.CompleteFileScan(completeCtx, ticket, decision); err != nil {
		return true, err
	}
	return true, nil
}
