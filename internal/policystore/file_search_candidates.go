package policystore

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5"
	"github.com/leileipei/Enterprise_IM/internal/access"
	"sort"
	"time"
)

type fileSearchCandidate struct {
	conversation, kind, message, sender, file string
	seq                                       int64
	at                                        time.Time
}
type fileSearchBatch struct {
	conversation, kind string
	candidates         []fileSearchCandidate
	more               bool
}

func readFileSearchCandidatesTx(ctx context.Context, tx pgx.Tx, id access.TrustedIdentity, c historyCandidate, after int64, limit int) (fileSearchBatch, error) {
	b := fileSearchBatch{conversation: c.ID, kind: c.Kind, candidates: make([]fileSearchCandidate, 0, min(limit, 50))}
	rows, e := tx.Query(ctx, `SELECT m.id::text,m.seq,m.sender_user_id::text,m.accepted_at,COALESCE(a.file_id::text,'') FROM messages m LEFT JOIN message_attachments a ON a.tenant_id=m.tenant_id AND a.conversation_id=m.conversation_id AND a.message_id=m.id WHERE m.tenant_id=$1 AND m.conversation_id=$2 AND m.message_type='file' AND m.seq>$3 ORDER BY m.seq LIMIT $4`, id.TenantID, c.ID, after, limit+1)
	if e != nil {
		return b, e
	}
	defer rows.Close()
	for rows.Next() {
		var v fileSearchCandidate
		v.conversation = c.ID
		v.kind = c.Kind
		if e = rows.Scan(&v.message, &v.seq, &v.sender, &v.at, &v.file); e != nil {
			return b, e
		}
		if len(b.candidates) == limit {
			b.more = true
			break
		}
		b.candidates = append(b.candidates, v)
	}
	return b, rows.Err()
}
func sortedFileSearchIDs(values map[string]bool) []string {
	out := make([]string, 0, len(values))
	for v := range values {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

// Prelock the complete batch, in parent/conversation/policy/file/source order.
// Repeated proof loaders can only reacquire locks already held by this batch.
func prelockFileSearchTx(ctx context.Context, tx pgx.Tx, id access.TrustedIdentity, batches []fileSearchBatch) (map[string]string, error) {
	fileIDs, convIDs, members := map[string]bool{}, map[string]bool{}, map[string]bool{id.ActingMembershipID: true}
	for _, b := range batches {
		convIDs[b.conversation] = true
		for _, c := range b.candidates {
			if c.file != "" {
				fileIDs[c.file] = true
			}
		}
	}
	owners := map[string]string{}
	rows, e := tx.Query(ctx, `SELECT id::text,conversation_id::text,uploader_membership_id::text FROM file_objects WHERE tenant_id=$1 AND id=ANY($2::uuid[]) ORDER BY id`, id.TenantID, sortedFileSearchIDs(fileIDs))
	if e != nil {
		return nil, e
	}
	for rows.Next() {
		var fid, cid, mid string
		if e = rows.Scan(&fid, &cid, &mid); e != nil {
			rows.Close()
			return nil, e
		}
		owners[fid] = cid
		members[mid] = true
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return nil, e
	}
	lock := func(q string, args ...any) error {
		rows, e := tx.Query(ctx, q, args...)
		if e != nil {
			return e
		}
		for rows.Next() {
		}
		e = rows.Err()
		rows.Close()
		return e
	}
	if e = lock(`SELECT m.id FROM user_organizations m JOIN users u ON u.tenant_id=m.tenant_id AND u.id=m.user_id JOIN organizations o ON o.tenant_id=m.tenant_id AND o.id=m.organization_id JOIN legal_entities l ON l.tenant_id=o.tenant_id AND l.id=o.legal_entity_id JOIN tenants t ON t.id=m.tenant_id WHERE m.tenant_id=$1 AND m.id=ANY($2::uuid[]) ORDER BY m.id FOR SHARE OF t,u,m,o,l NOWAIT`, id.TenantID, sortedFileSearchIDs(members)); e != nil {
		return nil, e
	}
	if e = lock(`SELECT id FROM conversations WHERE tenant_id=$1 AND id=ANY($2::uuid[]) ORDER BY id FOR UPDATE NOWAIT`, id.TenantID, sortedFileSearchIDs(convIDs)); e != nil {
		return nil, e
	}
	var version int64
	e = tx.QueryRow(ctx, `SELECT current_version FROM policy_current WHERE tenant_id=$1 FOR SHARE`, id.TenantID).Scan(&version)
	if e != nil && !errors.Is(e, pgx.ErrNoRows) {
		return nil, e
	}
	if e = lock(`SELECT tenant_id FROM tenant_file_retention_policy WHERE tenant_id=$1 FOR SHARE`, id.TenantID); e != nil {
		return nil, e
	}
	if e = lock(`SELECT id FROM file_objects WHERE tenant_id=$1 AND id=ANY($2::uuid[]) ORDER BY id FOR SHARE`, id.TenantID, sortedFileSearchIDs(fileIDs)); e != nil {
		return nil, e
	}
	if e = lock(`SELECT m.id,a.file_id FROM messages m JOIN message_attachments a ON a.tenant_id=m.tenant_id AND a.message_id=m.id AND a.conversation_id=m.conversation_id WHERE a.tenant_id=$1 AND a.file_id=ANY($2::uuid[]) ORDER BY a.file_id,m.id FOR SHARE OF m,a`, id.TenantID, sortedFileSearchIDs(fileIDs)); e != nil {
		return nil, e
	}
	return owners, nil
}
