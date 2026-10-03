package files

import (
	"crypto/sha256"
	"errors"
	"math"
	"strings"
	"testing"
)

func validCreateFixture() CreateParams {
	return CreateParams{TenantID: "10000000-0000-4000-8000-00000000000a", ConversationID: "10000000-0000-4000-8000-00000000000b", UploaderUserID: "10000000-0000-4000-8000-00000000000c", UploaderMembershipID: "10000000-0000-4000-8000-00000000000d", UploadRequestID: "10000000-0000-4000-8000-00000000000e", OriginalFilename: "报告.pdf", DeclaredMediaType: "application/pdf", DeclaredSizeBytes: 1}
}
func TestNormalizeCreateBoundaries(t *testing.T) {
	for _, size := range []int64{1, 26214400} {
		p := validCreateFixture()
		p.DeclaredSizeBytes = size
		if got, e := NormalizeCreate(p); e != nil || got != p {
			t.Fatalf("valid size: %v %v", got, e)
		}
	}
	for _, size := range []int64{0, -1, 26214401, math.MaxInt64} {
		p := validCreateFixture()
		p.DeclaredSizeBytes = size
		if got, e := NormalizeCreate(p); !errors.Is(e, ErrInvalidMetadata) || got != (CreateParams{}) {
			t.Fatalf("invalid size produced params: %v %v", got, e)
		}
	}
	for _, name := range []string{strings.Repeat("中", 85), "内部 空格.pdf", "\ufeff报告.pdf", "e\u0301.pdf"} {
		p := validCreateFixture()
		p.OriginalFilename = name
		if _, e := NormalizeCreate(p); e != nil {
			t.Errorf("valid filename %q: %v", name, e)
		}
	}
	for _, name := range []string{"", strings.Repeat("中", 85) + "a", ".", "..", "a/b", "a\\b", "a:b", "a\x00b", "a\x7fb", "a\u0085b", "a\u009fb", "\u00a0a.pdf", "a.pdf\u3000", " a.pdf", "a.pdf\n", string([]byte{0xff})} {
		p := validCreateFixture()
		p.OriginalFilename = name
		if got, e := NormalizeCreate(p); !errors.Is(e, ErrInvalidMetadata) || got != (CreateParams{}) {
			t.Errorf("invalid filename %q: %v %v", name, got, e)
		}
	}
	for field := 0; field < 5; field++ {
		for _, id := range []string{"", "short", " 10000000-0000-4000-8000-00000000000a", "{10000000-0000-4000-8000-00000000000a}", "10000000-0000-4000-8000-00000000000g"} {
			p := validCreateFixture()
			slots := []*string{&p.TenantID, &p.ConversationID, &p.UploaderUserID, &p.UploaderMembershipID, &p.UploadRequestID}
			*slots[field] = id
			if _, e := NormalizeCreate(p); !errors.Is(e, ErrInvalidMetadata) {
				t.Errorf("invalid UUID field %d accepted", field)
			}
		}
	}
	p := validCreateFixture()
	p.TenantID = strings.ToUpper(p.TenantID)
	before := p
	got, e := NormalizeCreate(p)
	if e != nil || got.TenantID != strings.ToLower(p.TenantID) || p != before {
		t.Fatal("normalization must lower UUID only", got, e)
	}
}
func TestNormalizeCreateMIME(t *testing.T) {
	for _, typ := range []string{"application/pdf", "image/png", "application/vnd.openxmlformats-officedocument.wordprocessingml.document", "application/x-a!#$%&'*+-.^_`|~", "a/" + strings.Repeat("b", 125)} {
		p := validCreateFixture()
		p.DeclaredMediaType = typ
		if _, e := NormalizeCreate(p); e != nil {
			t.Errorf("valid MIME %q: %v", typ, e)
		}
	}
	for _, typ := range []string{"", "application/PDF", "APPLICATION/pdf", "text/plain; charset=utf-8", " text/plain", "text/plain ", "text /plain", "text/plain\n", "文/plain", "plain", "/plain", "text/", "a/" + strings.Repeat("b", 126)} {
		p := validCreateFixture()
		p.DeclaredMediaType = typ
		if got, e := NormalizeCreate(p); !errors.Is(e, ErrInvalidMetadata) || got != (CreateParams{}) {
			t.Errorf("invalid MIME %q: %v %v", typ, got, e)
		}
	}
}
func TestCreationDigestCanonicalJSON(t *testing.T) {
	p := validCreateFixture()
	raw := `["v1","10000000-0000-4000-8000-00000000000a","10000000-0000-4000-8000-00000000000b","10000000-0000-4000-8000-00000000000c","10000000-0000-4000-8000-00000000000d","报告.pdf","application/pdf","1"]`
	want := sha256.Sum256([]byte(raw))
	got, e := CreationDigest(p)
	if e != nil || got != want {
		t.Fatalf("canonical digest %x want %x: %v", got, want, e)
	}
	upper := p
	upper.TenantID = strings.ToUpper(p.TenantID)
	upper.ConversationID = strings.ToUpper(p.ConversationID)
	upper.UploaderUserID = strings.ToUpper(p.UploaderUserID)
	upper.UploaderMembershipID = strings.ToUpper(p.UploaderMembershipID)
	if d, e := CreationDigest(upper); e != nil || d != want {
		t.Fatal("UUID case altered digest")
	}
	req := p
	req.UploadRequestID = "10000000-0000-4000-8000-000000000009"
	if d, e := CreationDigest(req); e != nil || d != want {
		t.Fatal("request ID must be key, not digest content")
	}
	for _, mut := range []func(*CreateParams){func(p *CreateParams) { p.TenantID = "10000000-0000-4000-8000-000000000001" }, func(p *CreateParams) { p.ConversationID = "10000000-0000-4000-8000-000000000001" }, func(p *CreateParams) { p.UploaderUserID = "10000000-0000-4000-8000-000000000001" }, func(p *CreateParams) { p.UploaderMembershipID = "10000000-0000-4000-8000-000000000001" }, func(p *CreateParams) { p.OriginalFilename = "不同.pdf" }, func(p *CreateParams) { p.DeclaredMediaType = "image/png" }, func(p *CreateParams) { p.DeclaredSizeBytes = 2 }} {
		next := p
		mut(&next)
		if d, e := CreationDigest(next); e != nil || d == want {
			t.Fatal("changed identity/content reused digest", next, e)
		}
	}
	p.OriginalFilename = "中\"<>&.pdf"
	raw = `["v1","10000000-0000-4000-8000-00000000000a","10000000-0000-4000-8000-00000000000b","10000000-0000-4000-8000-00000000000c","10000000-0000-4000-8000-00000000000d","中\"\u003c\u003e\u0026.pdf","application/pdf","1"]`
	if d, e := CreationDigest(p); e != nil || d != sha256.Sum256([]byte(raw)) {
		t.Fatal("JSON escaping mismatch", e)
	}
	a, b := validCreateFixture(), validCreateFixture()
	a.OriginalFilename = "é.pdf"
	b.OriginalFilename = "e\u0301.pdf"
	da, _ := CreationDigest(a)
	db, _ := CreationDigest(b)
	if da == db {
		t.Fatal("Unicode filename must not be normalized")
	}
	p.DeclaredSizeBytes = 26214401
	if d, e := CreationDigest(p); !errors.Is(e, ErrInvalidMetadata) || d != ([32]byte{}) {
		t.Fatal("invalid input produced digest")
	}
}
