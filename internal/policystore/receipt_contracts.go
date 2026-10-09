package policystore

import "time"

type ReceiptState struct {
	ScannedSeq, DeliveredSeq, ReadSeq, ReceiptRevision int64
	Duplicate                                          bool
}
type ContentLease struct{ ContentExpiresAt, CacheUntil time.Time }
type DeviceSyncPage struct {
	Page                       MessagePage
	SyncProof                  string
	ProofExpiresAt, ServerTime time.Time
	ReceiptRevision            int64
	ContentLeases              map[int64]ContentLease
}
type ReceiptSummary struct {
	Seq                                                            int64
	Status                                                         string
	SummaryAvailable                                               bool
	VisibleRecipientCount, VisibleDeliveredCount, VisibleReadCount int64
	DeliveredAt, ReadAt                                            *time.Time
}
type ReceiptUser struct {
	UserID, DisplayName string
	DeliveredAt, ReadAt *time.Time
}
type ReceiptUserPage struct {
	Users      []ReceiptUser
	HasMore    bool
	NextCursor string
}
type UnreadState struct {
	UnreadCount, HeadSeq, ReceiptRevision, PolicyVersion int64
	Exact                                                bool
}
