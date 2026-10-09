package receiptoutbox

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/leileipei/Enterprise_IM/internal/access"
)

var ErrWorkerUnconfigured = errors.New("receipt worker requires database and publisher")
var ErrOutboxUpdateMissing = errors.New("claimed receipt event was not updated")

// Event is a minimal signal to query authoritative receipt state. It carries
// no user/device list, message content, or recipient counts.
type Event struct {
	ID, TenantID, ConversationID string
	Revision                     int64
}

type Publisher interface {
	Publish(context.Context, Event) error
}

type Worker struct {
	DB        access.Beginner
	Publisher Publisher
	Now       func() time.Time
}

func (w Worker) now() time.Time {
	if w.Now != nil {
		return w.Now().UTC()
	}
	return time.Now().UTC()
}

func retryDelay(failedAttempts int) time.Duration {
	if failedAttempts > 8 {
		return 5 * time.Minute
	}
	return time.Second << failedAttempts
}

// ProcessOne claims one due event. A Redis success followed by a database
// commit failure can be replayed; consumers must deduplicate event IDs.
func (w Worker) ProcessOne(ctx context.Context) (bool, error) {
	if w.DB == nil || w.Publisher == nil {
		return false, ErrWorkerUnconfigured
	}
	tx, err := w.DB.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(context.Background())
	var event Event
	var failedAttempts int
	err = tx.QueryRow(ctx, `
SELECT id::text,tenant_id::text,conversation_id::text,revision,attempt_count
FROM receipt_outbox_events
WHERE status='pending' AND next_retry_at<=$1
ORDER BY next_retry_at,created_at,id
LIMIT 1 FOR UPDATE SKIP LOCKED`, w.now()).
		Scan(&event.ID, &event.TenantID, &event.ConversationID, &event.Revision, &failedAttempts)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	publishCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	publishErr := w.Publisher.Publish(publishCtx, event)
	cancel()
	at := w.now()
	if publishErr != nil {
		tag, updateErr := tx.Exec(ctx, `
UPDATE receipt_outbox_events SET attempt_count=attempt_count+1,next_retry_at=$3
WHERE tenant_id=$1 AND id=$2 AND status='pending'`,
			event.TenantID, event.ID, at.Add(retryDelay(failedAttempts)))
		if updateErr != nil {
			return true, errors.Join(publishErr, updateErr)
		}
		if tag.RowsAffected() != 1 {
			return true, errors.Join(publishErr, ErrOutboxUpdateMissing)
		}
		if err := tx.Commit(ctx); err != nil {
			return true, errors.Join(publishErr, err)
		}
		return true, publishErr
	}
	tag, err := tx.Exec(ctx, `
UPDATE receipt_outbox_events SET status='published',attempt_count=attempt_count+1,published_at=$3
WHERE tenant_id=$1 AND id=$2 AND status='pending'`, event.TenantID, event.ID, at)
	if err != nil {
		return true, err
	}
	if tag.RowsAffected() != 1 {
		return true, ErrOutboxUpdateMissing
	}
	if err := tx.Commit(ctx); err != nil {
		return true, err
	}
	return true, nil
}
