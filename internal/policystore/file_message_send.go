package policystore

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/leileipei/Enterprise_IM/internal/access"
	"time"
)

func (s Service) SendMessage(ctx context.Context, id access.TrustedIdentity, conversationID string, req MessageSendRequest) (MessageACK, error) {
	// Preserve the established text path's validation and error ordering.
	attempts := 1
	if req.MessageType == MessageTypeFile {
		var err error
		id, err = fileIdentity(id)
		if err != nil {
			return MessageACK{}, ErrForbidden
		}
		attempts = 3
	}
	for attempt := 0; attempt < attempts; attempt++ {
		ack, err := s.sendDirectMessageOnce(ctx, id, conversationID, req)
		var pe *pgconn.PgError
		retry := errors.As(err, &pe) && (pe.Code == "40P01" || pe.Code == "55P03")
		if !retry || attempt == attempts-1 || ctx.Err() != nil {
			return ack, err
		}
	}
	return MessageACK{}, ErrFileMessageUnavailable
}
func (s Service) finishFileMessageSend(ctx context.Context, tx pgx.Tx, id access.TrustedIdentity, conversationID, reason string, at time.Time, recheck func(time.Time) error) error {
	if err := auditMessageSend(ctx, tx, id, conversationID, "allow", reason, at); err != nil {
		return errors.Join(ErrAuditUnavailable, err)
	}
	if err := recheck(s.now()); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s Service) finishExistingFileMessage(ctx context.Context, tx pgx.Tx, id access.TrustedIdentity, ack MessageACK, same bool, at time.Time, recheck func(time.Time) error) (MessageACK, error) {
	if !same {
		return finishExistingMessage(ctx, tx, id, ack, false, at)
	}
	if _, err := tx.Exec(ctx, "SAVEPOINT file_replay_provisional"); err != nil {
		return MessageACK{}, err
	}
	err := s.finishFileMessageSend(ctx, tx, id, ack.ConversationID, "idempotent_replay", at, recheck)
	if err != nil {
		if errors.Is(err, ErrForbidden) || errors.Is(err, ErrMessageNotAvailable) || errors.Is(err, ErrConversationContextChanged) {
			if _, rollbackErr := tx.Exec(ctx, "ROLLBACK TO SAVEPOINT file_replay_provisional"); rollbackErr != nil {
				return MessageACK{}, rollbackErr
			}
			if auditErr := finishMessageSend(ctx, tx, id, ack.ConversationID, "deny", "authorization_changed", s.now()); auditErr != nil {
				return MessageACK{}, auditErr
			}
		}
		return MessageACK{}, err
	}
	ack.Duplicate = true
	return ack, nil
}
