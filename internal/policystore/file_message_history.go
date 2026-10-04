package policystore

import (
	"context"
	"github.com/jackc/pgx/v5"
	"strings"
	"unicode/utf8"
)

type MessageAttachment struct {
	FileID            string
	Available         bool
	OriginalFilename  string
	ActualSizeBytes   *int64
	DetectedMediaType string
}
type historyReadMode int

const (
	historyReadAll historyReadMode = iota
	historyReadTextOnly
)

// Metadata is read within the message history transaction. This is a card,
// never a download grant. Matching origins also close a damaged association.
const historyAttachmentJoin = `
LEFT JOIN message_attachments a ON a.tenant_id=m.tenant_id AND a.message_id=m.id
 AND a.conversation_id=m.conversation_id AND a.sender_user_id=m.sender_user_id
 AND a.sender_membership_id=m.sender_membership_id
LEFT JOIN file_objects f ON f.tenant_id=a.tenant_id AND f.id=a.file_id
 AND f.conversation_id=a.conversation_id AND f.uploader_user_id=a.sender_user_id
 AND f.uploader_membership_id=a.sender_membership_id
`
const historyAttachmentSelect = `,m.message_type,a.file_id::text,
 COALESCE(f.state='ready' AND f.original_filename IS NOT NULL AND f.actual_size_bytes>0
 AND f.detected_media_type IS NOT NULL AND f.sha256 IS NOT NULL
 AND f.scan_job_id IS NOT NULL AND f.scan_engine IS NOT NULL AND f.scan_definition_version IS NOT NULL
 AND f.scanned_at IS NOT NULL AND f.scan_sha256=f.sha256,false),
 f.original_filename,f.actual_size_bytes,f.detected_media_type`

type historyAttachmentFacts struct {
	kind      string
	fileID    *string
	available bool
	filename  *string
	size      *int64
	mediaType *string
}

func (f *historyAttachmentFacts) destinations() []any {
	return []any{&f.kind, &f.fileID, &f.available, &f.filename, &f.size, &f.mediaType}
}
func (f historyAttachmentFacts) content() (string, *MessageAttachment, bool) {
	switch f.kind {
	case MessageTypeText:
		return f.kind, nil, f.fileID == nil
	case MessageTypeFile:
		if f.fileID == nil || !directoryUUIDPattern.MatchString(*f.fileID) {
			return "", nil, false
		}
		a := &MessageAttachment{FileID: *f.fileID}
		if f.available && f.filename != nil && *f.filename != "" && utf8.ValidString(*f.filename) && f.size != nil && *f.size > 0 && f.mediaType != nil && strings.TrimSpace(*f.mediaType) != "" {
			size := *f.size
			a.Available = true
			a.OriginalFilename = *f.filename
			a.ActualSizeBytes = &size
			a.DetectedMediaType = *f.mediaType
		}
		return f.kind, a, true
	default:
		return "", nil, false
	}
}
func readDirectHistoryBatchTx(ctx context.Context, tx pgx.Tx, scope historyReadContext, cid string, after int64, limit int) (historyReadBatch, error) {
	return readDirectHistoryBatchTxMode(ctx, tx, scope, cid, after, limit, historyReadAll)
}
func readGroupHistoryBatchTx(ctx context.Context, tx pgx.Tx, scope historyReadContext, cid string, after int64, limit int) (historyReadBatch, error) {
	return readGroupHistoryBatchTxMode(ctx, tx, scope, cid, after, limit, historyReadAll)
}
func readDirectTextSearchBatchTx(ctx context.Context, tx pgx.Tx, scope historyReadContext, cid string, after int64, limit int) (historyReadBatch, error) {
	return readDirectHistoryBatchTxMode(ctx, tx, scope, cid, after, limit, historyReadTextOnly)
}
func readGroupTextSearchBatchTx(ctx context.Context, tx pgx.Tx, scope historyReadContext, cid string, after int64, limit int) (historyReadBatch, error) {
	return readGroupHistoryBatchTxMode(ctx, tx, scope, cid, after, limit, historyReadTextOnly)
}
func historyTypePredicate(mode historyReadMode) string {
	if mode == historyReadTextOnly {
		return " AND m.message_type='text'"
	}
	return ""
}
