package importapply

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"github.com/jackc/pgx/v5"
	"os"
	"testing"
)

const fixtureTenant = "90000000-0000-4000-8000-000000000001"
const fixtureActor = "90000000-0000-4000-8000-000000000002"
const fixtureMembership = "90000000-0000-4000-8000-000000000003"
const fixtureRequest = "90000000-0000-4000-8000-000000000004"

func TestAppendPGMigration(t *testing.T) {
	for _, base := range []int{1, 21} {
		t.Run(fmt.Sprintf("base%d", base), func(t *testing.T) {
			f := appendDB(t, base)
			ctx := context.Background()
			up, e := os.ReadFile("../../db/migrations/000022_import_batches.up.sql")
			if e != nil {
				t.Fatal(e)
			}
			if _, e = f.Admin.PgConn().Exec(ctx, string(up)).ReadAll(); e != nil {
				t.Fatal(e)
			}
			_, e = f.Admin.Exec(ctx, "INSERT INTO tenants(id,code,name) VALUES($1,'fixture','Fixture')", fixtureTenant)
			if e != nil {
				t.Fatal(e)
			}
			_, e = f.Admin.Exec(ctx, "GRANT SELECT,INSERT,UPDATE,DELETE ON import_batches TO im_import_writer")
			if e != nil {
				t.Fatal(e)
			}
			tx, e := f.Pool.Begin(ctx)
			if e != nil {
				t.Fatal(e)
			}
			defer tx.Rollback(ctx)
			batch := StoredBatch{Binding: BatchBinding{TenantID: fixtureTenant, RequestID: fixtureRequest, ActorUserID: fixtureActor, ActingMembershipID: fixtureMembership, ProtocolVersion: "controlled_append_v1", InputSHA256: sha256.Sum256([]byte(`{}`))}, Receipt: receiptFixture()}
			if e = insertReceipt(ctx, tx, f.Schema, batch); e != nil {
				t.Fatal(e)
			}
			got, found, e := lookupReceipt(ctx, tx, f.Schema, fixtureTenant, fixtureRequest)
			if e != nil || !found || !MatchBinding(got.Binding, batch.Binding) || got.Receipt.Counts["total"].Inserted != 1 {
				t.Fatal("stored receipt lost binding/counts")
			}
			if e = tx.Commit(ctx); e != nil {
				t.Fatal(e)
			}
			table := pgx.Identifier{f.Schema, "import_batches"}.Sanitize()
			receipt, _ := json.Marshal(batch.Receipt)
			invalid := []struct {
				digest        []byte
				state, reason string
				raw           []byte
			}{
				{make([]byte, 31), "applied", "NONE", receipt}, {make([]byte, 32), "running", "NONE", receipt}, {make([]byte, 32), "rejected", "NONE", receipt}, {make([]byte, 32), "applied", "NONE", []byte(`[]`)}, {make([]byte, 32), "applied", "NONE", []byte(`{}`)},
			}
			for _, v := range invalid {
				_, err := f.Pool.Exec(ctx, "INSERT INTO "+table+"(tenant_id,request_id,actor_user_id,acting_membership_id,protocol_version,input_sha256,state,reason,receipt,completed_at) VALUES($1,gen_random_uuid(),$2,$3,'controlled_append_v1',$4,$5,$6,$7,$8)", fixtureTenant, fixtureActor, fixtureMembership, v.digest, v.state, v.reason, v.raw, batch.Receipt.CompletedAt)
				if err == nil {
					t.Fatal("invalid stored receipt accepted")
				}
			}
			_, e = f.Pool.Exec(ctx, "INSERT INTO "+table+" SELECT * FROM "+table)
			if e == nil {
				t.Fatal("duplicate batch accepted")
			}
			_, e = f.Pool.Exec(ctx, "UPDATE "+table+" SET reason=reason")
			if e == nil {
				t.Fatal("immutable receipt changed")
			}
			down, e := os.ReadFile("../../db/migrations/000022_import_batches.down.sql")
			if e != nil {
				t.Fatal(e)
			}
			if _, e = f.Admin.PgConn().Exec(ctx, string(down)).ReadAll(); e != nil {
				t.Fatal(e)
			}
		})
	}
}
