package policystore_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"github.com/coder/websocket"
	"github.com/golang-jwt/jwt/v5"
	"github.com/leileipei/Enterprise_IM/internal/outbox"
	"github.com/leileipei/Enterprise_IM/internal/receiptoutbox"
	"github.com/leileipei/Enterprise_IM/internal/testfixtures"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

type receiptChild struct {
	t        *testing.T
	id, name string
	proof    *testfixtures.IntegrationProcess
	stopped  bool
	secrets  []string
}

func receiptDocker(t *testing.T, args ...string) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", args...)
	b, e := cmd.CombinedOutput()
	if e != nil {
		t.Fatalf("receipt container operation failed (%s)", args[0])
	}
	return b
}
func startReceiptChild(t *testing.T, dir, name, binary, ca string, env map[string]string) *receiptChild {
	t.Helper()
	owner := os.Getenv("IM_TEST_INTEGRATION_OWNER")
	source := os.Getenv("IM_TEST_INTEGRATION_SOURCE_SHA")
	if owner == "" || source == "" {
		t.Fatal("owned process evidence environment required")
	}
	ef := filepath.Join(dir, name+".env")
	lines := []string{}
	secrets := []string{}
	for k, v := range env {
		if strings.ContainsAny(v, "\r\n") {
			t.Fatal("invalid fixture environment")
		}
		lines = append(lines, k+"="+v)
		if strings.HasSuffix(k, "URL") {
			u, _ := url.Parse(v)
			if u != nil && u.User != nil {
				p, _ := u.User.Password()
				if p != "" {
					secrets = append(secrets, p)
				}
			}
		}
	}
	if e := os.WriteFile(ef, []byte(strings.Join(lines, "\n")+"\n"), 0600); e != nil {
		t.Fatal(e)
	}
	args := []string{"create", "--label", "im.integration.owner=" + owner, "--label", "im.integration.source=" + source, "--env-file", ef, "--mount", "type=bind,source=" + binary + ",target=/run/product,readonly"}
	if ca != "" {
		args = append(args, "--mount", "type=bind,source="+ca+",target=/run/ca.pem,readonly", "--publish", "127.0.0.1::8080")
	}
	args = append(args, "alpine:3.22", "/run/product")
	id := strings.TrimSpace(string(receiptDocker(t, args...)))
	proof, e := testfixtures.RegisterIntegrationContainer(id, os.Getenv("IM_TEST_INTEGRATION_GATE"), t.Name())
	if e != nil {
		receiptDocker(t, "rm", "-f", id)
		t.Fatal(e)
	}
	child := &receiptChild{t: t, id: id, name: name, proof: proof, secrets: secrets}
	t.Cleanup(func() {
		if !child.stopped {
			child.stop(0)
		}
	})
	receiptDocker(t, "start", id)
	return child
}
func (c *receiptChild) stop(expected int) {
	c.t.Helper()
	if c.stopped {
		return
	}
	c.stopped = true
	if expected == 137 {
		receiptDocker(c.t, "kill", "--signal", "KILL", c.id)
	} else {
		receiptDocker(c.t, "stop", "--time", "10", c.id)
	}
	b := receiptDocker(c.t, "inspect", c.id)
	var rows []struct {
		State struct {
			ExitCode int
			Running  bool
		}
	}
	if e := json.Unmarshal(b, &rows); e != nil || len(rows) != 1 || rows[0].State.Running {
		c.t.Fatal("child exit unproved")
	}
	actual := rows[0].State.ExitCode
	logs := receiptDocker(c.t, "logs", c.id)
	text := string(logs)
	for _, secret := range c.secrets {
		text = strings.ReplaceAll(text, secret, "[REDACTED]")
	}
	prefix := os.Getenv("IM_TEST_RECEIPT_ARTIFACT_PREFIX")
	if prefix != "" {
		if e := os.WriteFile(prefix+"-"+c.name+".log", []byte(text), 0600); e != nil {
			c.t.Fatal(e)
		}
	}
	if e := c.proof.Exited("exit:"+strconv.Itoa(expected), "exit:"+strconv.Itoa(actual)); e != nil {
		c.t.Fatal(e)
	}
	c.t.Logf("child %s actual_exit=%d expected_exit=%d log_sha256=%x", c.name, actual, expected, sha256.Sum256([]byte(text)))
	if actual != expected {
		c.t.Fatalf("child %s unexpected exit %d", c.name, actual)
	}
}
func (c *receiptChild) url() string {
	b := receiptDocker(c.t, "inspect", c.id)
	var rows []struct {
		NetworkSettings struct {
			Ports map[string][]struct{ HostIP, HostPort string }
		}
	}
	if e := json.Unmarshal(b, &rows); e != nil || len(rows) != 1 {
		c.t.Fatal("port evidence invalid")
	}
	p := rows[0].NetworkSettings.Ports["8080/tcp"]
	if len(p) != 1 || p[0].HostIP != "127.0.0.1" {
		c.t.Fatal("nonloopback API fixture")
	}
	return "http://127.0.0.1:" + p[0].HostPort
}
func (c *receiptChild) ready() {
	if e := c.proof.Ready(); e != nil {
		c.t.Fatal(e)
	}
}
func buildReceiptProduct(t *testing.T, dir, repo, pkg, name string) string {
	t.Helper()
	binary := filepath.Join(dir, name)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	goBinary, e := exec.LookPath("go")
	if e != nil {
		t.Fatal("Go compiler fixture unavailable")
	}
	cmd := exec.CommandContext(ctx, goBinary, "-C", repo, "build", "-o", binary, pkg)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOOS=linux", "GOARCH="+runtime.GOARCH, "CGO_ENABLED=0", "GOWORK=off", "GOFLAGS=-mod=readonly")
	b, e := cmd.CombinedOutput()
	proof, regerr := testfixtures.RegisterIntegrationProcess(cmd, os.Getenv("IM_TEST_INTEGRATION_GATE"), t.Name())
	if regerr != nil {
		t.Fatal(regerr)
	}
	if e != nil {
		t.Fatalf("product build failed: %s", b)
	}
	if e = proof.Exited("exit:0", "exit:"+strconv.Itoa(cmd.ProcessState.ExitCode())); e != nil {
		t.Fatal(e)
	}
	bytes, e := os.ReadFile(binary)
	if e != nil {
		t.Fatal(e)
	}
	t.Logf("product %s sha256=%x", pkg, sha256.Sum256(bytes))
	return binary
}
func TestReceiptWorkerCrashReplayTwoAPIProcesses(t *testing.T) {
	root := os.Getenv("IM_TEST_INTEGRATION_PRIVATE_ROOT")
	if root == "" {
		t.Skip("owned PG/Redis/container process fixture required")
	}
	c, s, d := receiptDirectFixture(t)
	ctx := context.Background()
	receiptDeliver(t, s, d, directA)
	client := receiptRedis(t)
	dir, e := os.MkdirTemp(root, "receipt-process-")
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	repo, e := filepath.Abs("../..")
	if e != nil {
		t.Fatal(e)
	}
	apiBinary := buildReceiptProduct(t, dir, repo, "./cmd/im-api", "im-api")
	workerBinary := buildReceiptProduct(t, dir, repo, "./cmd/im-receipt-worker", "im-receipt-worker")
	key, e := rsa.GenerateKey(rand.Reader, 2048)
	if e != nil {
		t.Fatal(e)
	}
	certificate := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: "receipt-local-fixture"}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, DNSNames: []string{"host.docker.internal"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}}
	der, e := x509.CreateCertificate(rand.Reader, certificate, certificate, &key.PublicKey, key)
	if e != nil {
		t.Fatal(e)
	}
	ca := filepath.Join(dir, "ca.pem")
	if e = os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0600); e != nil {
		t.Fatal(e)
	}
	issuerServer := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/keys" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"keys": []any{map[string]any{"kty": "RSA", "kid": "receipt-local", "alg": "RS256", "use": "sig", "n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()), "e": "AQAB"}}})
	}))
	issuerServer.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}, MinVersion: tls.VersionTLS12}
	issuerServer.StartTLS()
	defer issuerServer.Close()
	_, port, e := net.SplitHostPort(issuerServer.Listener.Addr().String())
	if e != nil {
		t.Fatal(e)
	}
	issuer := "https://host.docker.internal:" + port
	migration, e := os.ReadFile(filepath.Join(repo, "db/migrations/000004_external_identities.up.sql"))
	if e != nil {
		t.Fatal(e)
	}
	if _, e = c.PgConn().Exec(ctx, string(migration)).ReadAll(); e != nil {
		t.Fatal(e)
	}
	run(t, c, "INSERT INTO external_identities(issuer,subject,tenant_id,user_id) VALUES($1,'owner',$2,$3),($1,'foreign',$4,$5)", issuer, tenantA, adminA, tenantB, personB)
	tokenFor := func(subject string) string {
		now := time.Now()
		token := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{"iss": issuer, "sub": subject, "aud": "enterprise-im-api", "exp": now.Add(time.Hour).Unix(), "iat": now.Add(-time.Second).Unix(), "jti": "receipt-" + subject, "client_id": "enterprise-im-web"})
		token.Header["typ"] = "at+jwt"
		token.Header["kid"] = "receipt-local"
		raw, e := token.SignedString(key)
		if e != nil {
			t.Fatal(e)
		}
		return raw
	}
	var schema string
	if e = c.QueryRow(ctx, "SELECT current_schema()").Scan(&schema); e != nil {
		t.Fatal(e)
	}
	databaseURL, e := url.Parse(os.Getenv("IM_TEST_DATABASE_URL"))
	if e != nil {
		t.Fatal("invalid process database fixture")
	}
	databaseURL.Host = "host.docker.internal:" + databaseURL.Port()
	params := databaseURL.Query()
	params.Set("search_path", schema+",public")
	databaseURL.RawQuery = params.Encode()
	redisURL, e := url.Parse(os.Getenv("IM_TEST_REDIS_URL"))
	if e != nil {
		t.Fatal("invalid process Redis fixture")
	}
	redisURL.Host = "host.docker.internal:" + redisURL.Port()
	oldStream := "im:test:receipt-process:" + strconv.FormatInt(time.Now().UnixNano(), 10)
	receiptStream := oldStream + ":receipts"
	t.Cleanup(func() {
		client.Del(context.Background(), oldStream, receiptStream, outbox.PublisherPresenceKey(oldStream), receiptoutbox.PublisherPresenceKey(receiptStream))
	})
	presenceCtx, presenceCancel := context.WithCancel(ctx)
	defer presenceCancel()
	if e = outbox.RefreshPublisherPresence(ctx, client, oldStream); e != nil {
		t.Fatal(e)
	}
	go func() {
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-presenceCtx.Done():
				return
			case <-ticker.C:
				outbox.RefreshPublisherPresence(presenceCtx, client, oldStream)
			}
		}
	}()
	environment := map[string]string{"IM_DATABASE_URL": databaseURL.String(), "IM_HTTP_ADDR": "0.0.0.0:8080", "IM_OIDC_ENABLED": "true", "IM_OIDC_ISSUER": issuer, "IM_OIDC_JWKS_URL": issuer + "/keys", "IM_OIDC_AUDIENCE": "enterprise-im-api", "IM_OIDC_ALLOWED_CLIENT_IDS": "enterprise-im-web", "SSL_CERT_FILE": "/run/ca.pem", "IM_RECEIPTS_ENABLED": "true", "IM_REALTIME_REDIS_URL": redisURL.String(), "IM_REALTIME_STREAM": oldStream, "IM_RECEIPT_STREAM": receiptStream}
	first := startReceiptChild(t, dir, "api-1", apiBinary, ca, environment)
	second := startReceiptChild(t, dir, "api-2", apiBinary, ca, environment)
	waitReady := func(child *receiptChild) {
		deadline := time.Now().Add(15 * time.Second)
		for time.Now().Before(deadline) {
			response, e := http.Get(child.url() + "/health/ready")
			if e == nil {
				response.Body.Close()
				if response.StatusCode == 200 {
					child.ready()
					return
				}
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Fatal("real API process did not become ready")
	}
	waitReady(first)
	waitReady(second)
	socket := func(child *receiptChild, token, acting string) *websocket.Conn {
		req, _ := http.NewRequest("POST", child.url()+"/api/v1/realtime/tickets", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("X-Acting-Membership-ID", acting)
		response, e := http.DefaultClient.Do(req)
		if e != nil {
			t.Fatal("ticket request failed")
		}
		defer response.Body.Close()
		var ticket struct {
			Ticket string `json:"ticket"`
		}
		if e = json.NewDecoder(response.Body).Decode(&ticket); e != nil || response.StatusCode != 200 || ticket.Ticket == "" {
			t.Fatal("verified OIDC ticket failed", response.StatusCode)
		}
		connectCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		ws, _, e := websocket.Dial(connectCtx, strings.Replace(child.url(), "http://", "ws://", 1)+"/api/v1/realtime", &websocket.DialOptions{Subprotocols: []string{"enterprise-im.v1", "ticket." + ticket.Ticket}})
		if e != nil {
			t.Fatal("real API socket failed")
		}
		readCtx, cancel2 := context.WithTimeout(ctx, 5*time.Second)
		defer cancel2()
		_, b, e := ws.Read(readCtx)
		if e != nil || !strings.Contains(string(b), "ready") {
			t.Fatal("socket readiness missing")
		}
		t.Cleanup(func() { ws.CloseNow() })
		return ws
	}
	ownerToken := tokenFor("owner")
	ws1 := socket(first, ownerToken, adminM)
	ws2 := socket(second, ownerToken, adminM)
	foreign := socket(first, tokenFor("foreign"), otherM)
	run(t, c, `CREATE FUNCTION fail_receipt_publication() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected publication rollback'; END $$`)
	run(t, c, `CREATE TRIGGER fail_receipt_publication BEFORE UPDATE ON receipt_outbox_events FOR EACH ROW EXECUTE FUNCTION fail_receipt_publication()`)
	workerEnv := map[string]string{"IM_DATABASE_URL": databaseURL.String(), "IM_RECEIPT_REDIS_URL": redisURL.String(), "IM_RECEIPT_STREAM": receiptStream}
	worker := startReceiptChild(t, dir, "worker-crash", workerBinary, "", workerEnv)
	readSignal := func(ws *websocket.Conn) {
		readCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
		defer cancel()
		_, b, e := ws.Read(readCtx)
		if e != nil || !strings.Contains(string(b), "sync_required") {
			t.Fatal("real-process receipt signal missing")
		}
	}
	readSignal(ws1)
	readSignal(ws2)
	worker.ready()
	var status string
	if e = c.QueryRow(ctx, "SELECT status FROM receipt_outbox_events").Scan(&status); e != nil || status != "pending" {
		t.Fatal("injected failure committed", status, e)
	}
	worker.stop(137)
	run(t, c, "DROP TRIGGER fail_receipt_publication ON receipt_outbox_events")
	run(t, c, "DROP FUNCTION fail_receipt_publication()")
	recovered := startReceiptChild(t, dir, "worker-recovered", workerBinary, "", workerEnv)
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if e = c.QueryRow(ctx, "SELECT status FROM receipt_outbox_events").Scan(&status); e != nil {
			t.Fatal(e)
		}
		if status == "published" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if status != "published" {
		t.Fatal("restart did not publish pending event")
	}
	recovered.ready()
	events, e := client.XRange(ctx, receiptStream, "-", "+").Result()
	if e != nil || len(events) < 2 {
		t.Fatal("real-process replay not observed", e)
	}
	for _, event := range events {
		if event.Values["event_id"] != events[0].Values["event_id"] {
			t.Fatal("replay changed durable event id")
		}
	}
	if _, e = s.ConfirmRead(ctx, groupMemberIdentity(), d, directA, []int64{1}); e != nil {
		t.Fatal(e)
	}
	readSignal(ws1)
	readSignal(ws2)
	readCtx, cancel := context.WithTimeout(ctx, 250*time.Millisecond)
	_, _, e = foreign.Read(readCtx)
	cancel()
	if e == nil {
		t.Fatal("foreign tenant got receipt notification")
	}
	first.stop(0)
	second.stop(0)
	recovered.stop(0)
}
