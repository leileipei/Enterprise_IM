package importapply

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/leileipei/Enterprise_IM/internal/access"
	p "github.com/leileipei/Enterprise_IM/internal/importpreflight"
)

var (
	ErrBusy                = errors.New("IMPORT_BUSY")
	ErrNotRecorded         = errors.New("IMPORT_NOT_RECORDED")
	ErrKeyConflict         = errors.New("IMPORT_KEY_CONFLICT")
	ErrInvalidInput        = errors.New("IMPORT_INPUT_INVALID")
	ErrForbidden           = errors.New("IMPORT_FORBIDDEN")
	ErrDatabaseUnavailable = errors.New("IMPORT_DATABASE_UNAVAILABLE")
	ErrRetryable           = errors.New("IMPORT_RETRYABLE")
	ErrAuditUnavailable    = errors.New("IMPORT_AUDIT_UNAVAILABLE")
	ErrCommitUnknown       = errors.New("IMPORT_COMMIT_UNKNOWN")
)

func publicError(e error) error {
	if e == nil {
		return nil
	}
	if errors.Is(e, access.ErrInvalidIdentity) {
		return ErrForbidden
	}
	for _, v := range []error{ErrBusy, ErrNotRecorded, ErrKeyConflict, ErrInvalidInput, ErrForbidden, ErrDatabaseUnavailable, ErrRetryable, ErrAuditUnavailable, ErrCommitUnknown} {
		if errors.Is(e, v) {
			return v
		}
	}
	var failure p.Failure
	if errors.As(e, &failure) && (failure.Code == "TIMEOUT" || failure.Code == "CANCELED") {
		return ErrRetryable
	}
	var pe *pgconn.PgError
	if errors.As(e, &pe) {
		switch pe.Code {
		case "40001", "40P01", "55P03", "57014":
			return ErrRetryable
		}
	}
	if errors.Is(e, context.Canceled) || errors.Is(e, context.DeadlineExceeded) || errors.Is(e, errSQLBudget) {
		return ErrRetryable
	}
	return ErrDatabaseUnavailable
}
func constraintError(e error) bool {
	var pe *pgconn.PgError
	if !errors.As(e, &pe) {
		return false
	}
	switch pe.Code {
	case "23502", "23503", "23505", "23514", "23P01":
		return true
	}
	return false
}
