package policystore_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"github.com/jackc/pgx/v5"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fileOIDCIssuer(t *testing.T, c *pgx.Conn) ([]string, func(string) string) {
	t.Helper()
	migration, e := os.ReadFile("../../db/migrations/000004_external_identities.up.sql")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = c.PgConn().Exec(context.Background(), string(migration)).ReadAll(); e != nil {
		t.Fatal(e)
	}
	key, e := rsa.GenerateKey(rand.Reader, 2048)
	if e != nil {
		t.Fatal(e)
	}
	n := base64.RawURLEncoding.EncodeToString(key.PublicKey.N.Bytes())
	ex := base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.PublicKey.E)).Bytes())
	jwks := fmt.Sprintf(`{"keys":[{"kty":"RSA","use":"sig","alg":"RS256","kid":"test-key","n":"%s","e":"%s"}]}`, n, ex)
	idp := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/keys" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(jwks))
	}))
	t.Cleanup(idp.Close)
	cert := filepath.Join(t.TempDir(), "idp-ca.pem")
	if e = os.WriteFile(cert, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: idp.Certificate().Raw}), 0600); e != nil {
		t.Fatal(e)
	}
	run(t, c, `INSERT INTO external_identities(issuer,subject,tenant_id,user_id) VALUES($1,'ordinary',$2,$3),($1,'admin',$2,$4)`, idp.URL, tenantA, personA, adminA)
	env := []string{"SSL_CERT_FILE=" + cert, "IM_OIDC_ENABLED=true", "IM_OIDC_ISSUER=" + idp.URL, "IM_OIDC_AUDIENCE=enterprise-im-api", "IM_OIDC_JWKS_URL=" + idp.URL + "/keys", "IM_OIDC_ALLOWED_CLIENT_IDS=enterprise-im-web"}
	return env, func(subject string) string { return productionJWT(t, key, idp.URL, subject) }
}
func fileRuntimeEnv(t *testing.T, dsn, dir string) []string {
	t.Helper()
	for _, key := range []string{"IM_TEST_S3_ENDPOINT", "IM_TEST_S3_BUCKET", "IM_FILE_S3_ACCESS_KEY", "IM_FILE_S3_SECRET_KEY", "IM_TEST_SCANNER_MANIFEST", "IM_TEST_CLAMD_SOCKET", "IM_TEST_QPDF_PATH", "IM_TEST_FILE_WORKER_ACCESS_KEY", "IM_TEST_FILE_WORKER_SECRET_KEY"} {
		if os.Getenv(key) == "" {
			t.Skip("controlled file dependencies required; run-all checks environment")
		}
	}
	return []string{"IM_DATABASE_URL=" + dsn, "IM_FILE_SPOOL_DIR=" + dir, "IM_FILE_S3_ENDPOINT=" + os.Getenv("IM_TEST_S3_ENDPOINT"), "IM_FILE_S3_REGION=us-east-1", "IM_FILE_S3_BUCKET=" + os.Getenv("IM_TEST_S3_BUCKET"), "IM_FILE_S3_CREDENTIAL_SOURCE=environment", "IM_FILE_S3_PATH_STYLE=true", "IM_FILE_S3_ACCESS_KEY=" + os.Getenv("IM_FILE_S3_ACCESS_KEY"), "IM_FILE_S3_SECRET_KEY=" + os.Getenv("IM_FILE_S3_SECRET_KEY")}
}
func startFileWorkerProcess(t *testing.T, binary string, env []string) *exec.Cmd {
	t.Helper()
	log, e := os.CreateTemp(t.TempDir(), "file-worker-*.log")
	if e != nil {
		t.Fatal(e)
	}
	cmd := fileProductCommand(binary, env)
	cmd.Stdout = log
	cmd.Stderr = log
	if e = cmd.Start(); e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait(); log.Close() })
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		b, _ := os.ReadFile(log.Name())
		if strings.Contains(string(b), "file worker started") {
			return cmd
		}
		time.Sleep(50 * time.Millisecond)
	}
	b, _ := os.ReadFile(log.Name())
	t.Fatalf("file worker did not start: %s", b)
	return nil
}
func filePublicRequest(t *testing.T, method, base, path, token, membership string, body string, typ string, want int) map[string]any {
	t.Helper()
	req, e := http.NewRequest(method, base+path, strings.NewReader(body))
	if e != nil {
		t.Fatal(e)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("X-Acting-Membership-ID", membership)
	if typ != "" {
		req.Header.Set("Content-Type", typ)
	}
	client := &http.Client{Timeout: 20 * time.Second}
	res, e := client.Do(req)
	if e != nil {
		t.Fatal(e)
	}
	defer res.Body.Close()
	var out map[string]any
	e = json.NewDecoder(res.Body).Decode(&out)
	if res.StatusCode != want {
		t.Fatalf("%s %s status%d want%d response%v", method, path, res.StatusCode, want, out)
	}
	if want == 200 || want == 201 {
		if e != nil {
			t.Fatal(e)
		}
		if res.Header.Get("Cache-Control") != "no-store" || res.Header.Get("X-Content-Type-Options") != "nosniff" {
			t.Fatal("file response headers", res.Header)
		}
	}
	for _, k := range []string{"object_key", "object_version_id", "sha256", "scan_sha256", "scan_engine", "scan_definition_version", "scan_job_id", "lease_token", "url", "download_url"} {
		if _, ok := out[k]; ok {
			t.Fatal("internal file field leaked", k)
		}
	}
	return out
}

// This constructor is shared by official product process fixtures.
func fileProductCommand(binary string, configuration []string) *exec.Cmd {
	cmd := exec.Command(binary)
	values := map[string]string{}
	for _, item := range configuration {
		key, value, ok := strings.Cut(item, "=")
		if !ok || key == "" {
			panic("invalid explicit product configuration")
		}
		values[key] = value
	}
	cmd.Env = processChildEnv(values)
	return cmd
}
