package filecleanup

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/leileipei/Enterprise_IM/internal/objectstore"
)

type cleanupWorkerRepo struct {
	ticket                        Ticket
	inventory                     Inventory
	recovery                      *Commitment
	committed, settled, finalized bool
	calls                         []string
	recordErr, errorOnGet         error
}

func (r *cleanupWorkerRepo) ClaimFileDelete(_ context.Context, owner string) (Ticket, bool, error) {
	r.calls = append(r.calls, "claim")
	return r.ticket, !r.finalized, nil
}
func (r *cleanupWorkerRepo) RecordFileDeleteInventory(_ context.Context, t Ticket, in Inventory) error {
	r.calls = append(r.calls, "inventory")
	if r.recordErr != nil {
		return r.recordErr
	}
	r.inventory = in
	return nil
}
func (r *cleanupWorkerRepo) GetFileDeleteInventory(context.Context, Ticket) (Inventory, error) {
	return r.inventory, r.errorOnGet
}
func (r *cleanupWorkerRepo) CommitFileDeleteVersion(_ context.Context, t Ticket, v string) (Commitment, error) {
	r.calls = append(r.calls, "commit")
	r.committed = true
	return Commitment{Ticket: t, CommitmentID: "00000000-0000-4000-8000-000000000008", VersionID: v}, nil
}
func (r *cleanupWorkerRepo) ClaimFileDeleteRecovery(context.Context, string) (Commitment, bool, error) {
	r.calls = append(r.calls, "recovery")
	if r.recovery != nil {
		r.committed = true
		return *r.recovery, true, nil
	}
	return Commitment{}, false, nil
}
func (r *cleanupWorkerRepo) SettleFileDeleteVersion(_ context.Context, c Commitment, p AbsenceProof) error {
	r.calls = append(r.calls, "settle")
	if !p.Absent || p.VersionID != c.VersionID {
		return errors.New("wrong proof")
	}
	r.settled = true
	r.inventory = Inventory{Reason: "inventory_incomplete"}
	return nil
}
func (r *cleanupWorkerRepo) FinalizeFileDelete(context.Context, Ticket) error {
	r.calls = append(r.calls, "finalize")
	if !r.settled || !r.inventory.Exhausted || len(r.inventory.Versions) != 0 {
		return errors.New("no fresh exhaustive proof")
	}
	r.finalized = true
	return nil
}

type cleanupWorkerObjects struct {
	t                            *testing.T
	repo                         *cleanupWorkerRepo
	pages                        []objectstore.VersionPage
	listErr, deleteErr, probeErr error
	probes                       []objectstore.VersionPresence
	deletes                      []objectstore.VersionRef
	lists                        int
}

func (o *cleanupWorkerObjects) ListVersions(_ context.Context, l objectstore.Location, c objectstore.VersionCursor, n int) (objectstore.VersionPage, error) {
	o.lists++
	if n != 100 || l.TenantID != o.repo.ticket.TenantID || l.FileID != o.repo.ticket.FileID {
		o.t.Fatal("incorrect bounded location")
	}
	if o.listErr != nil {
		return objectstore.VersionPage{}, o.listErr
	}
	if len(o.pages) == 0 {
		return objectstore.VersionPage{Exhausted: true}, nil
	}
	p := o.pages[0]
	o.pages = o.pages[1:]
	return p, nil
}
func (o *cleanupWorkerObjects) ProbeVersion(_ context.Context, v objectstore.VersionRef) (objectstore.VersionPresence, error) {
	if o.probeErr != nil {
		return objectstore.VersionUnknown, o.probeErr
	}
	if len(o.probes) == 0 {
		return objectstore.VersionAbsent, nil
	}
	p := o.probes[0]
	o.probes = o.probes[1:]
	return p, nil
}
func (o *cleanupWorkerObjects) DeleteVersion(_ context.Context, v objectstore.VersionRef) error {
	if !o.repo.committed || v.VersionID != "fixed-v1" || v.Location.TenantID != o.repo.ticket.TenantID || v.Location.FileID != o.repo.ticket.FileID {
		o.t.Fatal("delete without exact persistent commitment", v)
	}
	o.deletes = append(o.deletes, v)
	return o.deleteErr
}
func cleanupWorkerFixture(t *testing.T) (*cleanupWorkerRepo, *cleanupWorkerObjects) {
	t.Helper()
	ticket := cleanupTicket()
	ticket.LeaseExpiresAt = time.Now().Add(120 * time.Second)
	r := &cleanupWorkerRepo{ticket: ticket}
	o := &cleanupWorkerObjects{t: t, repo: r, pages: []objectstore.VersionPage{{Versions: []objectstore.InventoryVersion{{Ref: objectstore.VersionRef{Location: objectstore.Location{TenantID: ticket.TenantID, FileID: ticket.FileID}, VersionID: "fixed-v1"}}}, Exhausted: true}}}
	return r, o
}
func TestFileDeleteWorkerFixedVersion(t *testing.T) {
	r, o := cleanupWorkerFixture(t)
	w, e := NewWorker(r, o, r.ticket.OwnerID)
	if e != nil {
		t.Fatal(e)
	}
	found, e := w.Step(context.Background())
	if e != nil || !found || len(o.deletes) != 1 || !r.settled || !r.finalized || o.lists != 2 {
		t.Fatal(found, e, r.calls, o.lists)
	}
}
func TestFileDeleteWorkerUnknownResponse(t *testing.T) {
	for _, why := range []string{"delete+unknown", "success+unknown", "lost+absent"} {
		t.Run(why, func(t *testing.T) {
			r, o := cleanupWorkerFixture(t)
			if why != "success+unknown" {
				o.deleteErr = errors.New("response lost")
			}
			if why != "lost+absent" {
				o.probeErr = errors.New("probe unavailable")
			}
			w, _ := NewWorker(r, o, r.ticket.OwnerID)
			found, e := w.Step(context.Background())
			if !found {
				t.Fatal("candidate not claimed")
			}
			if why == "lost+absent" {
				if e != nil || !r.settled {
					t.Fatal(e, r.calls)
				}
			} else if e == nil || r.settled || r.finalized || !r.committed {
				t.Fatal("unknown result settled", e, r.calls)
			}
		})
	}
}
func TestFileDeleteWorkerPausedReconcile(t *testing.T) {
	r, o := cleanupWorkerFixture(t)
	r.recovery = &Commitment{Ticket: r.ticket, CommitmentID: "00000000-0000-4000-8000-000000000008", VersionID: "fixed-v1"}
	o.probes = []objectstore.VersionPresence{objectstore.VersionPresent, objectstore.VersionAbsent}
	w, _ := NewWorker(r, o, r.ticket.OwnerID)
	found, e := w.Step(context.Background())
	if e != nil || !found || !r.settled || r.finalized || o.lists != 0 || len(o.deletes) != 1 {
		t.Fatal(found, e, r.calls)
	}
	for _, c := range r.calls {
		if c == "claim" || c == "commit" || c == "inventory" {
			t.Fatal("recovery acquired new work", r.calls)
		}
	}
}
func TestFileDeleteWorkerInventoryBounds(t *testing.T) {
	for _, why := range []string{"list failure", "marker", "neighbor", "repeat", "ten pages", "get failure"} {
		t.Run(why, func(t *testing.T) {
			r, o := cleanupWorkerFixture(t)
			switch why {
			case "list failure":
				o.listErr = errors.New("list failed")
			case "get failure":
				r.errorOnGet = errors.New("database down")
			case "marker":
				o.pages[0].Versions[0].DeleteMarker = true
			case "neighbor":
				o.pages[0].Versions[0].Ref.Location.FileID = "00000000-0000-4000-8000-000000000099"
			case "repeat", "ten pages":
				key := "tenants/" + r.ticket.TenantID + "/files/" + r.ticket.FileID
				o.pages = nil
				for i := 0; i < 11; i++ {
					marker := "v1"
					if why == "ten pages" {
						marker = string(rune('a' + i))
					}
					o.pages = append(o.pages, objectstore.VersionPage{Next: objectstore.VersionCursor{KeyMarker: key, VersionMarker: marker}})
				}
			}
			w, _ := NewWorker(r, o, r.ticket.OwnerID)
			_, e := w.Step(context.Background())
			if e == nil || r.committed || r.settled || r.finalized || len(o.deletes) != 0 || o.lists > 10 {
				t.Fatal("incomplete listing acquired permit", e, r.calls, o.lists)
			}
		})
	}
}
