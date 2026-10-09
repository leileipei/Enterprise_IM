//go:build nativeclientintegration && (darwin || linux)

package policystore_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/leileipei/Enterprise_IM/internal/outbox"
	"github.com/leileipei/Enterprise_IM/internal/receiptoutbox"
	"github.com/redis/go-redis/v9"
)

// This explicitly selected gate uses real Dart/SQLite/TLS/API workers. Breaking
// own-device enforcement, wire DTOs, or user aggregation must fail its assertions.
func TestNativeSharedCoreTwoDevicesAgainstProductionAPI(t *testing.T) {
	for _, key := range []string{"IM_TEST_DATABASE_URL", "IM_TEST_REDIS_URL", "IM_TEST_NATIVE_DART", "IM_TEST_NATIVE_PUB_CACHE"} {
		if os.Getenv(key) == "" {
			t.Fatalf("explicit native gate requires %s", key)
		}
	}
	ctx := context.Background()
	conn := db(t)
	seedDirectConversation(t, conn)
	var schema string
	if err := conn.QueryRow(ctx, "SELECT current_schema()").Scan(&schema); err != nil {
		t.Fatal(err)
	}
	dsn := processDatabaseURL(t, schema)
	authEnv, token := fileOIDCIssuer(t, conn)
	options, err := redis.ParseURL(os.Getenv("IM_TEST_REDIS_URL"))
	if err != nil {
		t.Fatal(err)
	}
	redisClient := redis.NewClient(options)
	t.Cleanup(func() { _ = redisClient.Close() })
	stream := "enterprise-im:test:native:" + schema
	receiptStream := stream + ":receipts"
	t.Cleanup(func() {
		if err := redisClient.Del(ctx, stream, receiptStream, outbox.PublisherPresenceKey(stream), receiptoutbox.PublisherPresenceKey(receiptStream)).Err(); err != nil {
			t.Error("Redis cleanup failed")
		}
	})
	worker := fileProductCommand(productionTestBinary(t, "im-outbox-worker"), []string{"IM_DATABASE_URL=" + dsn, "IM_OUTBOX_REDIS_URL=" + os.Getenv("IM_TEST_REDIS_URL"), "IM_OUTBOX_STREAM=" + stream})
	startRealtimeProcess(t, worker, func() bool { return redisClient.Exists(ctx, outbox.PublisherPresenceKey(stream)).Val() == 1 })
	receiptWorker := fileProductCommand(productionTestBinary(t, "im-receipt-worker"), []string{"IM_DATABASE_URL=" + dsn, "IM_RECEIPT_REDIS_URL=" + os.Getenv("IM_TEST_REDIS_URL"), "IM_RECEIPT_STREAM=" + receiptStream})
	startRealtimeProcess(t, receiptWorker, func() bool {
		return redisClient.Exists(ctx, receiptoutbox.PublisherPresenceKey(receiptStream)).Val() == 1
	})
	apiEnv := append(authEnv, "IM_DATABASE_URL="+dsn, "IM_HTTP_ADDR=127.0.0.1:0", "IM_RECEIPTS_ENABLED=true", "IM_RECEIPT_WRITES_PER_SECOND=100", "IM_REALTIME_REDIS_URL="+os.Getenv("IM_TEST_REDIS_URL"), "IM_REALTIME_STREAM="+stream, "IM_RECEIPT_STREAM="+receiptStream)
	upstream, err := url.Parse(startProductionAPI(t, productionTestBinary(t, "im-api"), apiEnv))
	if err != nil {
		t.Fatal(err)
	}
	proxy, ca := nativeProbeTLS(t, httputil.NewSingleHostReverseProxy(upstream))
	dir := t.TempDir()
	caPath := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(caPath, ca, 0600); err != nil {
		t.Fatal(err)
	}
	client := nativeProbeClientCopy(t, filepath.Join(dir, "client"))
	input, err := json.Marshal(map[string]any{
		"origin": proxy.URL, "ca": caPath, "storage": filepath.Join(dir, "storage"), "tenant": tenantA, "conversation": directA,
		"sender":        map[string]string{"token": token("admin"), "membership": adminM, "user": adminA},
		"bob":           map[string]string{"token": token("ordinary"), "membership": targetM2, "user": personA},
		"registrations": []string{clientUUIDv7(time.Now(), 9101), clientUUIDv7(time.Now(), 9102), clientUUIDv7(time.Now(), 9103)},
		"messageIds":    []string{clientUUIDv7(time.Now(), 9111), clientUUIDv7(time.Now(), 9112), clientUUIDv7(time.Now(), 9113)},
	})
	if err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(dir, "input.json")
	if err := os.WriteFile(configPath, input, 0600); err != nil {
		t.Fatal(err)
	}
	probeCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(probeCtx, os.Getenv("IM_TEST_NATIVE_DART"), "--packages=.dart_tool/package_config.json", "run", "tool/receipt_backend_probe.dart", configPath)
	cmd.Dir = client
	cmd.Env = processChildEnv(map[string]string{"CI": "true", "FLUTTER_SUPPRESS_ANALYTICS": "true", "PUB_CACHE": os.Getenv("IM_TEST_NATIVE_PUB_CACHE")})
	logPath := filepath.Join(dir, "probe.log")
	log, err := os.OpenFile(logPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = log.Close() })
	cmd.Stdout, cmd.Stderr = log, log
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	registration := trackFileProductProcess(t, cmd)
	if err = registration.Ready(); err != nil {
		t.Fatal(err)
	}
	err = cmd.Wait()
	output, readErr := os.ReadFile(logPath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	// Probe emits only static assertions/public summary. Never print private input.
	if err != nil {
		t.Fatalf("Dart shared protocol probe failed: %v\n%s", err, output)
	}
	var report struct {
		Status   string `json:"status"`
		Messages int    `json:"messages"`
		Devices  int    `json:"bob_devices"`
		Unread   int    `json:"remaining_unread"`
		TLS      bool   `json:"tls_verified"`
		Ready    bool   `json:"websocket_ready"`
		Notice   bool   `json:"websocket_notification"`
	}
	if err = json.Unmarshal(output, &report); err != nil || report.Status != "passed" || report.Messages != 3 || report.Devices != 2 || report.Unread != 1 || !report.TLS || !report.Ready || !report.Notice {
		t.Fatal("missing complete Dart protocol report")
	}
	var deviceDelivery, deviceRead, userRead, readCursor int
	if err = conn.QueryRow(ctx, `SELECT count(*),count(*) FILTER (WHERE read_at IS NOT NULL) FROM message_device_receipts WHERE tenant_id=$1 AND user_id=$2 AND conversation_id=$3`, tenantA, personA, directA).Scan(&deviceDelivery, &deviceRead); err != nil {
		t.Fatal(err)
	}
	if err = conn.QueryRow(ctx, `SELECT count(*) FROM message_user_receipts WHERE tenant_id=$1 AND user_id=$2 AND conversation_id=$3 AND read_at IS NOT NULL`, tenantA, personA, directA).Scan(&userRead); err != nil {
		t.Fatal(err)
	}
	if err = conn.QueryRow(ctx, `SELECT read_seq FROM conversation_user_read_cursors WHERE tenant_id=$1 AND user_id=$2 AND conversation_id=$3`, tenantA, personA, directA).Scan(&readCursor); err != nil {
		t.Fatal(err)
	}
	if deviceDelivery != 6 || deviceRead != 3 || userRead != 2 || readCursor != 2 {
		t.Fatalf("independent receipt facts: delivered=%d deviceRead=%d userRead=%d cursor=%d", deviceDelivery, deviceRead, userRead, readCursor)
	}
	t.Log("real Go/Dart gate: TLS and WS verified; 2 device deliveries; exact visibility reads; 1 recipient per message; 1 unread; database facts confirmed")
}

func nativeProbeTLS(t *testing.T, handler *httputil.ReverseProxy) (*httptest.Server, []byte) {
	t.Helper()
	caKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "owned native fixture CA"}, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	leafKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	leaf := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "127.0.0.1"}, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour), BasicConstraintsValid: true, KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}}
	leafDER, err := x509.CreateCertificate(rand.Reader, leaf, ca, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	proxy := httptest.NewUnstartedServer(handler)
	proxy.TLS = &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{{Certificate: [][]byte{leafDER, caDER}, PrivateKey: leafKey}}}
	proxy.StartTLS()
	t.Cleanup(proxy.Close)
	return proxy, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
}

func nativeProbeClientCopy(t *testing.T, dest string) string {
	t.Helper()
	source, err := filepath.Abs("../../clients/enterprise_im")
	if err != nil {
		t.Fatal(err)
	}
	// Explicit source copy excludes SDK state/build products/secrets. The driver
	// records source and working-tree hashes; final acceptance uses a Git archive.
	for _, part := range []string{"lib", "tool", "pubspec.yaml", "pubspec.lock", ".dart_tool/package_config.json"} {
		path := filepath.Join(source, part)
		err = filepath.WalkDir(path, func(p string, e os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			rel, err := filepath.Rel(source, p)
			if err != nil {
				return err
			}
			target := filepath.Join(dest, rel)
			if e.Type()&os.ModeSymlink != 0 {
				return os.ErrInvalid
			}
			if e.IsDir() {
				return os.MkdirAll(target, 0700)
			}
			if err = os.MkdirAll(filepath.Dir(target), 0700); err != nil {
				return err
			}
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			return os.WriteFile(target, b, 0600)
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	configPath := filepath.Join(dest, ".dart_tool/package_config.json")
	bytes, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	var config map[string]any
	if err = json.Unmarshal(bytes, &config); err != nil {
		t.Fatal(err)
	}
	base := (&url.URL{Scheme: "file", Path: source + "/.dart_tool/"})
	for _, entry := range config["packages"].([]any) {
		p := entry.(map[string]any)
		if p["name"] == "enterprise_im" {
			p["rootUri"] = "../"
			continue
		}
		u, err := url.Parse(p["rootUri"].(string))
		if err != nil {
			t.Fatal(err)
		}
		p["rootUri"] = base.ResolveReference(u).String()
	}
	bytes, err = json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(configPath, bytes, 0600); err != nil {
		t.Fatal(err)
	}
	return dest
}
