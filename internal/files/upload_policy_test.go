package files

import (
	"errors"
	"reflect"
	"testing"
)

// A permissive fallback here would let unapproved types and oversize reservations enter the store.
func TestUploadPolicyDefaults(t *testing.T) {
	p := DefaultUploadPolicy()
	want := UploadPolicy{Enabled: false, MaxSizeBytes: 26214400, UploadTTLSeconds: 900, TenantStorageBudgetBytes: 1073741824, Version: 0, AllowedMediaTypes: []string{"application/pdf", "image/jpeg", "image/png", "text/plain"}}
	if !reflect.DeepEqual(p, want) {
		t.Fatalf("default=%+v", p)
	}
	if _, err := NormalizeUploadPolicy(p); err != nil {
		t.Fatal(err)
	}
	p.AllowedMediaTypes[0] = "application/zip"
	if DefaultUploadPolicy().AllowedMediaTypes[0] != "application/pdf" {
		t.Fatal("shared mutable defaults")
	}
}
func TestUploadPolicyBounds(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*UploadPolicy)
	}{
		{"zero size", func(p *UploadPolicy) { p.MaxSizeBytes = 0 }},
		{"over cap", func(p *UploadPolicy) { p.MaxSizeBytes = 26214401 }},
		{"ttl short", func(p *UploadPolicy) { p.UploadTTLSeconds = 59 }},
		{"ttl long", func(p *UploadPolicy) { p.UploadTTLSeconds = 3601 }},
		{"budget short", func(p *UploadPolicy) { p.TenantStorageBudgetBytes = 26214399 }},
		{"budget long", func(p *UploadPolicy) { p.TenantStorageBudgetBytes = 1099511627777 }},
		{"negative version", func(p *UploadPolicy) { p.Version = -1 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := DefaultUploadPolicy()
			tc.edit(&p)
			got, err := NormalizeUploadPolicy(p)
			if !errors.Is(err, ErrInvalidUploadPolicy) || !reflect.DeepEqual(got, UploadPolicy{}) {
				t.Fatalf("invalid policy escaped: %+v %v", got, err)
			}
		})
	}
	for _, tc := range []struct{ size, ttl, budget int64 }{{1, 60, 26214400}, {26214400, 3600, 1099511627776}} {
		p := DefaultUploadPolicy()
		p.Enabled = true
		p.MaxSizeBytes = tc.size
		p.UploadTTLSeconds = tc.ttl
		p.TenantStorageBudgetBytes = tc.budget
		if _, err := NormalizeUploadPolicy(p); err != nil {
			t.Fatal(err)
		}
	}
}
func TestUploadPolicyTypeSubset(t *testing.T) {
	p := DefaultUploadPolicy()
	p.AllowedMediaTypes = []string{"text/plain", "image/png", "text/plain"}
	got, err := NormalizeUploadPolicy(p)
	if err != nil || !reflect.DeepEqual(got.AllowedMediaTypes, []string{"image/png", "text/plain"}) {
		t.Fatal(got, err)
	}
	if !reflect.DeepEqual(p.AllowedMediaTypes, []string{"text/plain", "image/png", "text/plain"}) {
		t.Fatal("mutated caller")
	}
	for _, v := range [][]string{nil, {}, {"application/zip"}, {"image/svg+xml"}, {"text/html"}, {"text/plain; charset=utf-8"}, {"IMAGE/PNG"}, {" text/plain"}, {""}} {
		p.AllowedMediaTypes = v
		if _, err := NormalizeUploadPolicy(p); !errors.Is(err, ErrInvalidUploadPolicy) {
			t.Fatalf("types %q accepted", v)
		}
	}
}
