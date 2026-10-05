package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/leileipei/Enterprise_IM/internal/access"
	"github.com/leileipei/Enterprise_IM/internal/filedownload"
	"github.com/leileipei/Enterprise_IM/internal/filetransfer"
	"github.com/leileipei/Enterprise_IM/internal/objectstore"
	"github.com/leileipei/Enterprise_IM/internal/policystore"
)

func TestFileBusinessStartupOrder(t *testing.T) {
	calls := 0
	rt, e := startFileRuntime(context.Background(), nil, func(string) string { calls++; panic("disabled runtime accessed environment") }, false, objectstore.Config{}, "", fileBusinessConfig{})
	if e != nil || rt == nil || calls != 0 {
		t.Fatal("disabled runtime not inert", e)
	}
	if e = rt.CheckHealth(context.Background()); e != nil {
		t.Fatal(e)
	}
	if e = rt.Close(); e != nil {
		t.Fatal(e)
	}
	dir := filepath.Join(t.TempDir(), "download")
	_, e = startFileRuntime(context.Background(), nil, env(nil), false, objectstore.Config{}, "", fileBusinessConfig{Enabled: true, SpoolDir: dir})
	if e == nil {
		t.Fatal("missing database accepted")
	}
	if _, e = os.Stat(dir); !os.IsNotExist(e) {
		t.Fatal("spool constructed before schema")
	}
}
func TestFileBusinessStartupBudget(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	<-ctx.Done()
	_, e := startFileRuntime(ctx, nil, env(nil), true, objectstore.Config{}, "", fileBusinessConfig{})
	if e == nil {
		t.Fatal("cancelled initialization accepted")
	}
}
func TestFileBusinessAssembly(t *testing.T) {
	for _, u := range []bool{false, true} {
		for _, b := range []bool{false, true} {
			rt := &fileRuntime{uploadEnabled: u, business: fileBusinessConfig{Enabled: b}}
			if u {
				rt.transfer = &filetransfer.Service{}
			}
			if b {
				rt.download = &filedownload.Service{}
			}
			h, e := assembleFileRoutes(http.NotFoundHandler(), fileSearchAssemblyAuth{}, policystore.Service{}, access.Service{}, rt)
			if e != nil {
				t.Fatal(e)
			}
			for _, method := range []string{"HEAD", "OPTIONS", "POST", "PUT"} {
				if method == "PUT" && u {
					continue
				}
				w := httptest.NewRecorder()
				h.ServeHTTP(w, httptest.NewRequest(method, "/api/v1/files/00000000-0000-4000-8000-000000000001/content", nil))
				allow := "GET"
				if u {
					allow += ", PUT"
				}
				if w.Code != 405 || w.Header().Get("Allow") != allow {
					t.Fatal(u, b, method, w.Code, w.Header())
				}
			}
			for _, path := range []string{"/api/v1/files/search?q=x", "/api/v1/conversations/11111111-1111-4111-8111-111111111111/files/search?q=x", "/api/v1/groups/11111111-1111-4111-8111-111111111111/files/search?q=x"} {
				req := httptest.NewRequest("GET", path, nil)
				req.Header.Set("Authorization", "Bearer fixture")
				req.Header.Set("X-Acting-Membership-ID", "33333333-3333-4333-8333-333333333333")
				w := httptest.NewRecorder()
				h.ServeHTTP(w, req)
				if w.Code != 400 {
					t.Fatal("search swallowed by generic route", w.Code)
				}
			}
		}
	}
	if _, e := assembleFileRoutes(http.NotFoundHandler(), fileSearchAssemblyAuth{}, policystore.Service{}, access.Service{}, &fileRuntime{business: fileBusinessConfig{Enabled: true}}); e == nil {
		t.Fatal("partial business assembly accepted")
	}
}
func TestFileBusinessSpoolIsolation(t *testing.T) {
	root := t.TempDir()
	d := filepath.Join(root, "download")
	if e := os.Mkdir(d, 0700); e != nil {
		t.Fatal(e)
	}
	alias := filepath.Join(root, "alias")
	if e := os.Symlink(d, alias); e != nil {
		t.Fatal(e)
	}
	for _, other := range []string{d, root, filepath.Join(d, "child"), alias} {
		if e := checkDownloadSpoolIsolation(d, other); e == nil {
			t.Fatal("overlap accepted")
		}
	}
	if e := checkDownloadSpoolIsolation(d, filepath.Join(root, "independent")); e != nil {
		t.Fatal(e)
	}
}
