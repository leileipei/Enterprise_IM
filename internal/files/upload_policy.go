package files

import (
	"errors"
	"slices"
)

var ErrInvalidUploadPolicy = errors.New("invalid file upload policy")

type UploadPolicy struct {
	Enabled                  bool
	MaxSizeBytes             int64
	AllowedMediaTypes        []string
	UploadTTLSeconds         int64
	TenantStorageBudgetBytes int64
	Version                  int64
}

func DefaultUploadPolicy() UploadPolicy {
	return UploadPolicy{MaxSizeBytes: MaxFileSizeBytes, AllowedMediaTypes: []string{"application/pdf", "image/jpeg", "image/png", "text/plain"}, UploadTTLSeconds: 900, TenantStorageBudgetBytes: 1073741824}
}
func supportedUploadType(t string) bool {
	switch t {
	case "application/pdf", "image/jpeg", "image/png", "text/plain":
		return true
	}
	return false
}
func NormalizeUploadPolicy(p UploadPolicy) (UploadPolicy, error) {
	if p.MaxSizeBytes < 1 || p.MaxSizeBytes > MaxFileSizeBytes || p.UploadTTLSeconds < 60 || p.UploadTTLSeconds > 3600 || p.TenantStorageBudgetBytes < MaxFileSizeBytes || p.TenantStorageBudgetBytes > 1099511627776 || p.Version < 0 || len(p.AllowedMediaTypes) == 0 {
		return UploadPolicy{}, ErrInvalidUploadPolicy
	}
	for _, t := range p.AllowedMediaTypes {
		if !supportedUploadType(t) {
			return UploadPolicy{}, ErrInvalidUploadPolicy
		}
	}
	p.AllowedMediaTypes = slices.Clone(p.AllowedMediaTypes)
	slices.Sort(p.AllowedMediaTypes)
	p.AllowedMediaTypes = slices.Compact(p.AllowedMediaTypes)
	return p, nil
}
