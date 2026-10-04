package policystore

import (
	"context"
	"github.com/jackc/pgx/v5"
	"github.com/leileipei/Enterprise_IM/internal/access"
	"github.com/leileipei/Enterprise_IM/internal/files"
	"github.com/leileipei/Enterprise_IM/internal/policy"
	"time"
)

// Complete all evidence reads before the final clock and pure decision.
func authorizeFileDownloadTx(ctx context.Context, tx pgx.Tx, id access.TrustedIdentity, fileID string) (files.Metadata, string, int64, time.Time, error) {
	f, err := loadFileVisibilityFactsTx(ctx, tx, id, fileID)
	if err != nil {
		return files.Metadata{}, "", 0, time.Time{}, err
	}
	fresh, err := fileClock(ctx, tx)
	if err != nil {
		return files.Metadata{}, "", 0, time.Time{}, err
	}
	m, mid, seq, err := evaluateFileVisibility(f, fresh)
	if err != nil {
		return files.Metadata{}, "", 0, time.Time{}, err
	}
	return m, mid, seq, fresh, nil
}

func downloadPairHardDenied(a, b policy.Membership, action policy.Action, rules []policy.Rule, at time.Time) bool {
	for _, r := range rules {
		if r.TenantID != a.TenantID || r.Action != action || r.Effect != policy.EffectHardDeny || at.Before(r.EffectiveFrom) || (!r.EffectiveTo.IsZero() && !at.Before(r.EffectiveTo)) {
			continue
		}
		if (groupHistoryRuleSideMatches(a, r.SourceOrganizationID, r.SourceMembershipID) && groupHistoryRuleSideMatches(b, r.TargetOrganizationID, r.TargetMembershipID)) || (groupHistoryRuleSideMatches(b, r.SourceOrganizationID, r.SourceMembershipID) && groupHistoryRuleSideMatches(a, r.TargetOrganizationID, r.TargetMembershipID)) {
			return true
		}
	}
	return false
}
