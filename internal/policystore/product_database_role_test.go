package policystore_test

import (
	"context"
	"github.com/jackc/pgx/v5"
	"os"
	"testing"
)

// The actual scoped product connection must not use fixture administrator authority.
func TestIntegrationProductDatabaseRole(t *testing.T) {
	if os.Getenv("IM_TEST_INTEGRATION_REGISTRY") == "" {
		t.Skip("controlled integration profile required")
	}
	admin := db(t)
	var schema string
	if err := admin.QueryRow(context.Background(), "SELECT current_schema()").Scan(&schema); err != nil {
		t.Fatal("fixture schema unavailable")
	}
	run(t, admin, "CREATE TABLE product_role_probe(value integer)")
	conn, err := pgx.Connect(context.Background(), processDatabaseURL(t, schema))
	if err != nil {
		t.Fatal("product scoped connection unavailable")
	}
	t.Cleanup(func() { conn.Close(context.Background()) })
	var elevated bool
	if err := conn.QueryRow(context.Background(), "SELECT rolsuper OR rolcreaterole OR rolcreatedb OR rolbypassrls OR rolreplication FROM pg_roles WHERE rolname=current_user").Scan(&elevated); err != nil {
		t.Fatal("actual product role flags unavailable")
	}
	if elevated {
		t.Fatal("official product configuration retains preparation administrator authority")
	}
	if _, err := conn.Exec(context.Background(), "INSERT INTO product_role_probe VALUES(1)"); err != nil {
		t.Fatal("ordinary product INSERT denied")
	}
	if _, err := conn.Exec(context.Background(), "UPDATE product_role_probe SET value=2"); err != nil {
		t.Fatal("ordinary product UPDATE denied")
	}
	var value int
	if err := conn.QueryRow(context.Background(), "SELECT value FROM product_role_probe").Scan(&value); err != nil || value != 2 {
		t.Fatal("ordinary product SELECT denied")
	}
	if _, err := conn.Exec(context.Background(), "CREATE TABLE product_forbidden(value integer)"); err == nil {
		t.Fatal("product role can modify fixture schema")
	}
}
