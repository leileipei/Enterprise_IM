package filecleanup

import (
	"context"
	"errors"
	"github.com/leileipei/Enterprise_IM/internal/objectstore"
	"time"
)

// Worker serializes one owner's bounded steps. A recovery step can only consume
// the original durable permit; it cannot obtain permission for another version.
type Worker struct {
	repo    Repository
	objects objectstore.VersionDeleter
	owner   string
	slot    chan struct{}
}

func NewWorker(repo Repository, objects objectstore.VersionDeleter, owner string) (*Worker, error) {
	if repo == nil || objects == nil || !canonicalID.MatchString(owner) {
		return nil, ErrIncomplete
	}
	return &Worker{repo: repo, objects: objects, owner: owner, slot: make(chan struct{}, 1)}, nil
}
func (w *Worker) Step(ctx context.Context) (bool, error) {
	select {
	case w.slot <- struct{}{}:
		defer func() { <-w.slot }()
	case <-ctx.Done():
		return false, ctx.Err()
	default:
		return false, ErrBlocked
	}
	c, found, e := w.repo.ClaimFileDeleteRecovery(ctx, w.owner)
	if e != nil {
		return false, e
	}
	if found {
		if e = ValidateCommitment(c); e != nil {
			return true, e
		}
		bound, stop := context.WithDeadline(ctx, c.Ticket.LeaseExpiresAt)
		defer stop()
		return true, w.reconcile(bound, c, true)
	}
	t, found, e := w.repo.ClaimFileDelete(ctx, w.owner)
	if e != nil || !found {
		return found, e
	}
	if e = ValidateTicket(t); e != nil {
		return true, e
	}
	bound, stop := context.WithDeadline(ctx, t.LeaseExpiresAt)
	defer stop()
	in, e := w.repo.GetFileDeleteInventory(bound, t)
	if e != nil {
		return true, e
	}
	pages := 0
	in, e = w.inventory(bound, t, in, &pages)
	if e != nil {
		return true, e
	}
	if len(in.Versions) > 0 {
		c, e = w.repo.CommitFileDeleteVersion(bound, t, in.Versions[0].VersionID)
		if e != nil {
			return true, e
		}
		if e = ValidateCommitment(c); e != nil {
			return true, e
		}
		if c.Ticket != t || c.VersionID != in.Versions[0].VersionID {
			return true, ErrIncomplete
		}
		if e = w.reconcile(bound, c, false); e != nil {
			return true, e
		}
		in, e = w.repo.GetFileDeleteInventory(bound, t)
		if e != nil {
			return true, e
		}
		if len(in.Versions) > 0 {
			return true, nil
		}
		in, e = w.inventory(bound, t, in, &pages)
		if e != nil {
			return true, e
		}
	}
	if !in.Exhausted || len(in.Versions) > 0 || in.Reason != "" {
		return true, ErrIncomplete
	}
	return true, w.repo.FinalizeFileDelete(bound, t)
}
func (w *Worker) reconcile(ctx context.Context, c Commitment, recovery bool) error {
	ref := objectstore.VersionRef{Location: objectstore.Location{TenantID: c.Ticket.TenantID, FileID: c.Ticket.FileID}, VersionID: c.VersionID}
	if recovery {
		p, e := w.objects.ProbeVersion(ctx, ref)
		if e != nil {
			return e
		}
		if p == objectstore.VersionAbsent {
			return w.settle(ctx, c)
		}
		if p != objectstore.VersionPresent {
			return ErrIncomplete
		}
	}
	deletion := w.objects.DeleteVersion(ctx, ref)
	p, e := w.objects.ProbeVersion(ctx, ref)
	if e != nil || p != objectstore.VersionAbsent {
		return errors.Join(ErrIncomplete, deletion, e)
	}
	// A lost DELETE response is harmless only when a separate exact probe proves absence.
	return w.settle(ctx, c)
}
func (w *Worker) settle(ctx context.Context, c Commitment) error {
	return w.repo.SettleFileDeleteVersion(ctx, c, AbsenceProof{VersionID: c.VersionID, Absent: true, CheckedAt: time.Now().UTC()})
}
func (w *Worker) inventory(ctx context.Context, t Ticket, in Inventory, pages *int) (Inventory, error) {
	if in.Reason != "" && in.Reason != "inventory_incomplete" {
		return in, ErrBlocked
	}
	location := objectstore.Location{TenantID: t.TenantID, FileID: t.FileID}
	cursor := objectstore.VersionCursor{KeyMarker: in.NextKey, VersionMarker: in.NextVersion}
	seen := map[objectstore.VersionCursor]bool{cursor: true}
	for !in.Exhausted {
		if *pages >= 10 {
			return in, ErrIncomplete
		}
		*pages++
		page, e := w.objects.ListVersions(ctx, location, cursor, 100)
		if e != nil {
			return in, e
		}
		next := Inventory{Exhausted: page.Exhausted, NextKey: page.Next.KeyMarker, NextVersion: page.Next.VersionMarker}
		if len(page.Versions) > 100 {
			return in, ErrIncomplete
		}
		for _, v := range page.Versions {
			if v.Ref.Location != location {
				return in, ErrIncomplete
			}
			if v.DeleteMarker {
				next.Reason = "delete_marker"
				continue
			}
			next.Versions = append(next.Versions, Version{VersionID: v.Ref.VersionID, AttemptID: v.AttemptID})
		}
		if ValidateInventory(t, next) != nil || (!page.Exhausted && seen[page.Next]) {
			return in, ErrIncomplete
		}
		if e = w.repo.RecordFileDeleteInventory(ctx, t, next); e != nil {
			return in, e
		}
		if next.Reason != "" {
			return next, ErrBlocked
		}
		seen[page.Next] = true
		cursor = page.Next
		in, e = w.repo.GetFileDeleteInventory(ctx, t)
		if e != nil {
			return in, e
		}
	}
	return in, nil
}
