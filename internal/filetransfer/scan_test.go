package filetransfer

import (
	"context"
	"crypto/sha256"
	"errors"
	"github.com/leileipei/Enterprise_IM/internal/files"
	"github.com/leileipei/Enterprise_IM/internal/filescanner"
	"github.com/leileipei/Enterprise_IM/internal/objectstore"
	"io"
	"os"
	"sync"
	"testing"
	"time"
)

type scanRepoStub struct {
	ticket                    files.ScanTicket
	mu                        sync.Mutex
	completed                 int
	decision                  files.ScanDecision
	renewError, errorComplete error
	renews                    int
}

func (r *scanRepoStub) ClaimFileScan(context.Context, string) (files.ScanTicket, bool, error) {
	return r.ticket, true, nil
}
func (r *scanRepoStub) RenewFileScan(_ context.Context, t files.ScanTicket) (files.ScanTicket, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.renews++
	return t, r.renewError
}
func (r *scanRepoStub) CompleteFileScan(_ context.Context, _ files.ScanTicket, d files.ScanDecision) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.completed++
	r.decision = d
	return r.errorComplete
}
func (r *scanRepoStub) RecoverExpiredFileScans(context.Context, string, int) (int, error) {
	return 0, nil
}

type scannerStub struct {
	runtimeError, scanError error
	calls                   int
	block                   bool
}

func (s *scannerStub) ValidateRuntime(context.Context) error { return s.runtimeError }
func (s *scannerStub) Scan(ctx context.Context, f *os.File, m files.Metadata) (files.ScanDecision, error) {
	s.calls++
	if s.block {
		<-ctx.Done()
		return files.ScanDecision{}, ctx.Err()
	}
	if _, e := io.ReadAll(f); e != nil {
		return files.ScanDecision{}, e
	}
	d := files.ScanDecision{State: files.StateReady, Engine: "test", DefinitionVersion: "test", ReasonCode: "scan_clean"}
	copy(d.SHA256[:], m.SHA256)
	return d, s.scanError
}
func scanTransferTicket() files.ScanTicket {
	now := time.Now()
	up := now.Add(-time.Second)
	size := int64(1)
	hash := sha256.Sum256([]byte("x"))
	m := files.Metadata{CreateParams: files.CreateParams{TenantID: testID, ConversationID: testID, UploaderUserID: testID, UploaderMembershipID: testID, UploadRequestID: testID, OriginalFilename: "x.txt", DeclaredMediaType: "text/plain", DeclaredSizeBytes: 1}, ID: testID, RequestDigest: hash[:], State: files.StateScanning, StateVersion: 2, CreatedAt: now.Add(-time.Minute), UpdatedAt: now, UploadExpiresAt: now.Add(time.Minute), UploadedAt: &up, ActualSizeBytes: &size, SHA256: hash[:], DetectedMediaType: "text/plain", ObjectKey: "tenants/" + testID + "/files/" + testID, ObjectVersionID: "fixed", ScanJobID: testID}
	return files.ScanTicket{File: m, JobID: testID, LeaseToken: testID, OwnerID: testID, ClaimVersion: 2, Attempt: 1, LeaseExpiresAt: now.Add(120 * time.Second)}
}
func TestFileScanRunnerEvidence(t *testing.T) {
	for _, tc := range []struct {
		name, body, reason               string
		runtimeErr, scanErr, completeErr error
	}{
		{name: "clean", body: "x", reason: "scan_clean"}, {name: "changed", body: "y", reason: "object_mismatch"}, {name: "oversized", body: "xx", reason: "object_mismatch"}, {name: "short", body: "", reason: "object_mismatch"}, {name: "definitions stale", body: "x", reason: "definitions_stale", runtimeErr: filescanner.ErrDefinitionsStale},
		{name: "runtime down", body: "x", reason: "scanner_unavailable", runtimeErr: errors.New("down")}, {name: "false clean with error", body: "x", reason: "scanner_unavailable", scanErr: errors.New("incomplete")}, {name: "audit unavailable", body: "x", reason: "scan_clean", completeErr: errors.New("audit")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := &scanRepoStub{ticket: scanTransferTicket(), errorComplete: tc.completeErr}
			objects := &objectStub{body: []byte(tc.body)}
			scanner := &scannerStub{runtimeError: tc.runtimeErr, scanError: tc.scanErr}
			worker := ScanWorker{Repo: repo, Objects: objects, Scanner: scanner, SpoolDir: t.TempDir() + "/private", OwnerID: testID}
			found, e := worker.RunOnce(context.Background())
			if !found || !errors.Is(e, tc.completeErr) {
				t.Fatal(found, e)
			}
			if repo.completed != 1 || repo.decision.ReasonCode != tc.reason || objects.puts != 0 {
				t.Fatal(repo.completed, repo.decision, objects.puts)
			}
			if tc.reason != "scan_clean" && (repo.decision.Engine != "" || repo.decision.DefinitionVersion != "" || repo.decision.SHA256 != [32]byte{} || repo.decision.State != files.StateScanFailed) {
				t.Fatal("false complete evidence", repo.decision)
			}
			entries, e := os.ReadDir(worker.SpoolDir)
			if e != nil || len(entries) != 0 {
				t.Fatal(entries, e)
			}
		})
	}
}
func TestFileScanRunnerLeaseLost(t *testing.T) {
	repo := &scanRepoStub{ticket: scanTransferTicket(), renewError: files.ErrLeaseLost}
	scanner := &scannerStub{block: true}
	worker := ScanWorker{Repo: repo, Objects: &objectStub{body: []byte("x")}, Scanner: scanner, SpoolDir: t.TempDir() + "/private", OwnerID: testID}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	start := time.Now()
	found, e := worker.RunOnce(ctx)
	if !found || !errors.Is(e, files.ErrLeaseLost) || repo.completed != 0 || repo.renews != 1 || time.Since(start) < 14*time.Second {
		t.Fatal(found, e, repo.completed, repo.renews)
	}
}
func TestFileScanRunnerMissingVersion(t *testing.T) {
	repo := &scanRepoStub{ticket: scanTransferTicket()}
	repo.ticket.File.ObjectVersionID = ""
	scanner := &scannerStub{}
	worker := ScanWorker{Repo: repo, Objects: &objectStub{}, Scanner: scanner, SpoolDir: t.TempDir() + "/private", OwnerID: testID}
	if _, e := worker.RunOnce(context.Background()); e != nil {
		t.Fatal(e)
	}
	if repo.decision.State != files.StateScanFailed || scanner.calls != 0 {
		t.Fatal(repo.decision, scanner.calls)
	}
}

var _ objectstore.Store = (*objectStub)(nil)

func TestFileScanRunnerDefaultDeadline(t *testing.T) {
	repo := &scanRepoStub{ticket: scanTransferTicket()}
	scanner := &scannerStub{block: true}
	worker := ScanWorker{Repo: repo, Objects: &objectStub{body: []byte("x")}, Scanner: scanner, SpoolDir: t.TempDir() + "/private", OwnerID: testID}
	start := time.Now()
	found, e := worker.RunOnce(context.Background())
	elapsed := time.Since(start)
	if e != nil || !found || elapsed < 89*time.Second || elapsed > 100*time.Second || repo.completed != 1 || repo.decision.State != files.StateScanFailed || repo.decision.Engine != "" || repo.renews < 5 {
		t.Fatal(found, e, elapsed, repo.completed, repo.renews, repo.decision)
	}
}
