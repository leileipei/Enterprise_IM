package oidcauth

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/leileipei/Enterprise_IM/internal/httpserver"
)

type queryRower interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

type Store struct{ DB queryRower }

func (s Store) Lookup(ctx context.Context, issuer, subject string) (httpserver.VerifiedIdentity, error) {
	if s.DB == nil || issuer == "" || subject == "" {
		return httpserver.VerifiedIdentity{}, errors.New("identity store unavailable")
	}
	var id httpserver.VerifiedIdentity
	err := s.DB.QueryRow(ctx, `
SELECT e.tenant_id::text, e.user_id::text
FROM external_identities e
JOIN tenants t ON t.id=e.tenant_id
JOIN users u ON u.tenant_id=e.tenant_id AND u.id=e.user_id
WHERE e.issuer=$1 AND e.subject=$2
  AND e.status='active' AND t.status='active' AND u.status='active'`, issuer, subject).Scan(&id.TenantID, &id.UserID)
	if errors.Is(err, pgx.ErrNoRows) {
		return httpserver.VerifiedIdentity{}, ErrIdentityNotFound
	}
	if err != nil {
		return httpserver.VerifiedIdentity{}, err
	}
	return id, nil
}
