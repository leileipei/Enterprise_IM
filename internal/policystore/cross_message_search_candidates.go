package policystore

import (
	"context"
	"sort"

	"github.com/jackc/pgx/v5"
	"github.com/leileipei/Enterprise_IM/internal/access"
)

type historyCandidate struct{ ID, Kind string }

func listCrossSearchCandidatesTx(ctx context.Context, tx pgx.Tx, id access.TrustedIdentity, kind string, position crossSearchPosition) ([]historyCandidate, error) {
	candidates := make([]historyCandidate, 0, 63)
	add := func(sql, k string) error {
		args := []any{id.TenantID, id.UserID}
		if position.Conversation != "" {
			op := ">"
			if position.Phase == "within" {
				op = ">="
			}
			sql += " AND c.id" + op + "$3::uuid"
			args = append(args, position.Conversation)
		}
		sql += " ORDER BY c.id LIMIT 21"
		rows, err := tx.Query(ctx, sql, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var value string
			if err := rows.Scan(&value); err != nil {
				return err
			}
			candidates = append(candidates, historyCandidate{value, k})
		}
		return rows.Err()
	}
	if kind == "all" || kind == "direct" {
		for _, side := range []string{"low", "high"} {
			if err := add(`SELECT c.id::text FROM conversations c WHERE c.tenant_id=$1 AND c.kind='direct' AND c.direct_user_`+side+`_id=$2`, "direct"); err != nil {
				return nil, err
			}
		}
	}
	if kind == "all" || kind == "group" {
		// Distinct IDs are grouped along the tenant/user/conversation interval index.
		sql := `SELECT c.id::text FROM (SELECT i.conversation_id AS id FROM conversation_membership_intervals i WHERE i.tenant_id=$1 AND i.user_id=$2`
		args := []any{id.TenantID, id.UserID}
		if position.Conversation != "" {
			op := ">"
			if position.Phase == "within" {
				op = ">="
			}
			sql += " AND i.conversation_id" + op + "$3::uuid"
			args = append(args, position.Conversation)
		}
		sql += ` GROUP BY i.conversation_id ORDER BY i.conversation_id LIMIT 21) c JOIN conversations g ON g.tenant_id=$1 AND g.id=c.id AND g.kind='group' ORDER BY c.id`
		rows, err := tx.Query(ctx, sql, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var value string
			if err := rows.Scan(&value); err != nil {
				rows.Close()
				return nil, err
			}
			candidates = append(candidates, historyCandidate{value, "group"})
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].ID < candidates[j].ID })
	unique := make([]historyCandidate, 0, 21)
	for _, c := range candidates {
		if len(unique) > 0 && unique[len(unique)-1].ID == c.ID {
			continue
		}
		unique = append(unique, c)
		if len(unique) == 21 {
			break
		}
	}
	return unique, nil
}
