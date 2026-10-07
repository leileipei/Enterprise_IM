package importcompare

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

func configFrom(dsn, schema string) (Config, error) {
	return LoadConfig(func(k string) string {
		switch k {
		case "IM_IMPORT_COMPARE_DATABASE_URL":
			return dsn
		case "IM_IMPORT_COMPARE_SCHEMA":
			return schema
		default:
			panic("unexpected configuration source")
		}
	})
}
func TestCompareUnitExplicitConfig(t *testing.T) {
	good := []string{
		"host=127.0.0.1 port=5432 dbname=im user=readonly sslmode=verify-full",
		"postgresql://readonly:quoted%27pass@localhost:5432/im?sslmode=verify-full&connect_timeout=1",
		"postgres://readonly@[::1]:5432/im?sslmode=verify-full",
		"host=/private/tmp/p4_29 dbname=im user=readonly sslmode=disable password='escaped\\\\password\\'quote'",
	}
	for _, dsn := range good {
		c, err := configFrom(dsn, "")
		if err != nil || c.Schema != "public" {
			t.Fatal("explicit valid connection")
		}
	}
	base := "host=localhost port=5432 dbname=im user=reader sslmode=verify-full"
	for _, schema := range []string{"a", strings.Repeat("a", 63)} {
		if _, err := configFrom(base, schema); err != nil {
			t.Fatal("valid schema")
		}
	}
	for _, schema := range []string{"A", "a.b", "a;secret", strings.Repeat("a", 64), "a\x00"} {
		if _, err := configFrom(base, schema); err == nil {
			t.Fatal("bad schema")
		}
	}
	for _, dsn := range []string{"", strings.Repeat("a", 8193), "host=localhost dbname=im user=reader sslmode=verify-full", base + " connect_timeout=4", base + " connect_timeout=0", "host=localhost port=0 dbname=im user=reader sslmode=verify-full", "host=localhost port=65536 dbname=im user=reader sslmode=verify-full", "host=localhost port=5432 dbname=im sslmode=verify-full", "host=localhost port=5432 user=reader sslmode=verify-full", "host=localhost port=5432 dbname=im user=reader sslmode=disable"} {
		if _, err := configFrom(dsn, ""); err == nil {
			t.Fatal("missing or unsafe explicit setting")
		}
	}
	dsn := base + " password='" + strings.Repeat("x", 8192-len(base)-12) + "'"
	if len(dsn) != 8192 {
		t.Fatal("fixture byte count")
	}
	if _, err := configFrom(dsn, ""); err != nil {
		t.Fatal("8 KiB explicit boundary")
	}
}
func TestCompareUnitNoAmbiguousDSN(t *testing.T) {
	base := "host=localhost port=5432 dbname=im user=reader sslmode=verify-full"
	for _, suffix := range []string{" service=bad service=", " passfile=/secret", " options='-c search_path=secret'", " search_path=secret", " ssl=true", " host=other", " port=5432", " unknown=private", " connect_timeout=1 connect_timeout=2"} {
		if _, err := configFrom(base+suffix, ""); err == nil {
			t.Fatal("ambiguous raw key accepted")
		}
	}
	for _, dsn := range []string{"postgresql://reader@localhost:5432/im?sslmode=verify-full&host=other", "postgresql://reader@localhost:5432/im?sslmode=verify-full&sslmode=disable", "postgresql://reader@localhost:5432/im?sslmode=verify-full&service=bad&service=", "host=one,two port=5432 dbname=im user=reader sslmode=verify-full", "postgresql://reader@localhost:5432/im?sslmode=verify-full#secret"} {
		if _, err := configFrom(dsn, ""); err == nil {
			t.Fatal("ambiguous target accepted")
		}
	}
}
func TestCompareUnitDriverEnvironmentGuard(t *testing.T) {
	c, err := configFrom("host=localhost port=5432 dbname=im user=reader sslmode=verify-full", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PGSERVICEFILE", "/private/marker")
	t.Setenv("HOME", "/private/marker")
	_, err = driverConfig(c)
	if err == nil || err.Error() != "DATABASE_CONFIG_INVALID" || os.Getenv("PGSERVICEFILE") != "/private/marker" {
		t.Fatal("driver default environment used or changed")
	}
}
func TestCompareUnitConfigRedaction(t *testing.T) {
	marker := "private_config_marker"
	c, e := configFrom("host=localhost port=5432 dbname="+marker+" user="+marker+" password="+marker+" sslmode=verify-full", marker)
	if e != nil {
		t.Fatal(e)
	}
	if strings.Contains(fmt.Sprintf("%v %#v", c, c), marker) {
		t.Fatal("configuration formatted secrets")
	}
	_, e = configFrom("service="+marker, "")
	if e == nil || strings.Contains(e.Error(), marker) {
		t.Fatal("parser raw error exposed")
	}
}
