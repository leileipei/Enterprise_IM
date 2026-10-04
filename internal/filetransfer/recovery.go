package filetransfer

import (
	"context"
	"github.com/leileipei/Enterprise_IM/internal/files"
	"github.com/leileipei/Enterprise_IM/internal/objectstore"
)

func (s *Service) RecoverOnce(parent context.Context) (bool, error) {
	ctx, cancel := context.WithTimeout(parent, 90e9)
	defer cancel()
	ticket, found, e := s.Repo.ClaimFileUploadRecovery(ctx, s.OwnerID)
	if e != nil || !found {
		return found, e
	}
	evidence := files.RecoveryEvidence{ReasonCode: "recovery_pending"}
	u := ticket.Upload
	if u.Measurement == nil {
		evidence.ReasonCode = "recovery_mismatch"
	} else {
		location := objectstore.Location{TenantID: u.File.TenantID, FileID: u.File.ID}
		refs, e := s.Objects.FindAttemptVersions(ctx, location, u.AttemptID, 100)
		if e != nil {
			evidence.ReasonCode = "object_read_failed"
		} else {
			var matches []objectstore.VersionRef
			uncertain := false
			for _, ref := range refs {
				if ref.Location != location {
					uncertain = true
					continue
				}
				if e = readMeasurement(ctx, s.Objects, ref, *u.Measurement); e == nil {
					matches = append(matches, ref)
				} else {
					uncertain = true
				}
			}
			if uncertain {
				evidence.ReasonCode = "recovery_mismatch"
			} else if len(matches) > 1 {
				evidence.ReasonCode = "recovery_ambiguous"
			} else if len(matches) == 1 {
				evidence = files.RecoveryEvidence{Resolved: true, VersionID: matches[0].VersionID, Measurement: *u.Measurement, ReasonCode: "recovery_resolved"}
			}
		}
	}
	if e = s.Repo.CompleteFileUploadRecovery(ctx, ticket, evidence); e != nil {
		return true, e
	}
	return true, nil
}
