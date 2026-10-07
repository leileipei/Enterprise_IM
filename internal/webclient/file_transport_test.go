package webclient

import (
	"context"
	"github.com/leileipei/Enterprise_IM/internal/httpserver"
	"github.com/leileipei/Enterprise_IM/internal/testfixtures"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func runFileUnit(t *testing.T, name, script string) {
	t.Helper()
	source, e := assets.ReadFile("assets/" + name + ".js")
	if e != nil {
		t.Fatal("file module missing", e)
	}
	node := os.Getenv("IM_TEST_BROWSER_NODE")
	if node == "" {
		node, e = exec.LookPath("node")
		if e != nil {
			t.Fatal("Node required for file unit tests")
		}
	}
	p := filepath.Join(t.TempDir(), name+".js")
	if e = os.WriteFile(p, source, 0600); e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, node, script)
	cmd.Env = testfixtures.BrowserEnvironment(map[string]string{"FILE_UNIT_SOURCE": p})
	out, e := cmd.CombinedOutput()
	if e != nil {
		t.Fatalf("%s unit: %v: %s", name, e, out)
	}
	t.Log(string(out))
}
func TestWebFileTransport(t *testing.T) {
	runFileUnit(t, "file-transport", "e2e/file_transport_unit.cjs")
	h, e := NewHandler(http.NotFoundHandler(), verifierFunc(func(context.Context, string) (httpserver.VerifiedIdentity, error) {
		return httpserver.VerifiedIdentity{}, nil
	}), webConfig("https://sso.example.test/token"), nil)
	if e != nil {
		t.Fatal(e)
	}
	page := httptest.NewRecorder()
	h.ServeHTTP(page, httptest.NewRequest("GET", "/web/", nil))
	js := httptest.NewRecorder()
	h.ServeHTTP(js, httptest.NewRequest("GET", "/web/file-transport.js", nil))
	if js.Code != 200 || js.Header().Get("Content-Type") != "text/javascript; charset=utf-8" || js.Header().Get("Content-Security-Policy") != page.Header().Get("Content-Security-Policy") {
		t.Fatal(js.Code, js.Header())
	}
	html := page.Body.String()
	if strings.Index(html, "/web/file-transport.js") < 0 || strings.Index(html, "/web/file-transport.js") > strings.Index(html, "/web/app.js") {
		t.Fatal("missing defer dependency")
	}
}
