package filetransfer

import (
	"context"
	"crypto/sha256"
	"github.com/leileipei/Enterprise_IM/internal/files"
	"github.com/leileipei/Enterprise_IM/internal/objectstore"
	"testing"
)

func TestFileTransferRecoveryUniqueEvidence(t *testing.T) {
	for _, count := range []int{0, 1, 2} {
		u := transferTicket()
		m := measuredForRecovery()
		u.Measurement = &m
		u.Phase = files.UploadRecoveryPending
		repo := &uploadRepoStub{claim: &files.RecoveryTicket{Upload: u, JobID: testID}}
		obj := &objectStub{body: []byte("x")}
		for i := 0; i < count; i++ {
			obj.refs = append(obj.refs, objectstore.VersionRef{Location: objectstore.Location{TenantID: testID, FileID: testID}, VersionID: "fixed"})
		}
		s, e := NewService(repo, obj, t.TempDir()+"/private", testID)
		if e != nil {
			t.Fatal(e)
		}
		found, e := s.RecoverOnce(context.Background())
		if e != nil || !found || repo.evidence.Resolved != (count == 1) || obj.puts != 0 || repo.seal != 0 {
			t.Fatal(count, found, e, repo.evidence)
		}
	}
}
func measuredForRecovery() files.Measurement {
	return files.Measurement{SizeBytes: 1, SHA256: sha256.Sum256([]byte("x")), DetectedMediaType: "text/plain"}
}
