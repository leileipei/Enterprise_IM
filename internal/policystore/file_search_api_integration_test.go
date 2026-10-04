package policystore_test

import (
	"context"
	"errors"
	"github.com/leileipei/Enterprise_IM/internal/httpserver"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

type fileSearchAPIAuth struct{}

func (fileSearchAPIAuth) Authenticate(_ context.Context, token string) (httpserver.VerifiedIdentity, error) {
	if token != "file-search-test" {
		return httpserver.VerifiedIdentity{}, errors.New("invalid")
	}
	return httpserver.VerifiedIdentity{TenantID: tenantA, UserID: personA}, nil
}
func TestFileSearchAPI(t *testing.T) {
	for _, kind := range []string{"direct", "group"} {
		t.Run(kind, func(t *testing.T) {
			c, s, cid, m := fileHistoryFixture(t, kind)
			h, e := httpserver.HandlerWithFileSearch(http.NotFoundHandler(), fileSearchAPIAuth{}, s)
			if e != nil {
				t.Fatal(e)
			}
			server := httptest.NewServer(h)
			defer server.Close()
			route := "conversations"
			if kind == "group" {
				route = "groups"
			}
			for _, path := range []string{"/api/v1/" + route + "/" + cid + "/files/search?q=" + url.QueryEscape(m.OriginalFilename), "/api/v1/files/search?q=" + url.QueryEscape(m.OriginalFilename) + "&kind=" + kind} {
				p := filePublicRequest(t, "GET", server.URL, path, "file-search-test", targetM2, "", "", 200)
				items, ok := p["matches"].([]any)
				if !ok || len(items) != 1 {
					t.Fatal(p)
				}
				got := items[0].(map[string]any)
				if got["file_id"] != m.ID || got["seq"] != "2" || len(got) != 10 {
					t.Fatal(got)
				}
			}
			run(t, c, "UPDATE user_organizations SET status='suspended' WHERE id=$1", targetM2)
			filePublicRequest(t, "GET", server.URL, "/api/v1/files/search?q=ab", "file-search-test", targetM2, "", "", 403)
		})
	}
}
