package objectstore

import (
	"context"
	"io"
	"os"

	"github.com/leileipei/Enterprise_IM/internal/files"
)

// ReadOnlyStore supports bounded runtime probes in addition to fixed-version reads.
// The broad Store interface is retained for existing download-service consumers.
type ReadOnlyStore interface {
	Store
	ValidateReadProbe(context.Context, string) error
}

type s3ReadOnlyStore struct{ base *s3Store }

func NewS3ReadOnly(c Config) (ReadOnlyStore, error) {
	key := os.Getenv("IM_FILE_DOWNLOAD_S3_ACCESS_KEY")
	if key != "" && (key == os.Getenv("IM_FILE_S3_ACCESS_KEY") || key == os.Getenv("IM_FILE_CLEANUP_S3_ACCESS_KEY")) {
		return nil, files.ErrDependencyUnavailable
	}
	client, e := newS3Client(c, "download_environment", "IM_FILE_DOWNLOAD_S3_ACCESS_KEY", "IM_FILE_DOWNLOAD_S3_SECRET_KEY")
	if e != nil {
		return nil, e
	}
	return &s3ReadOnlyStore{base: &s3Store{client: client, config: c}}, nil
}

func (s *s3ReadOnlyStore) ValidateCapabilities(ctx context.Context) error {
	return s.base.ValidateCapabilities(ctx)
}
func (s *s3ReadOnlyStore) ReadVersion(ctx context.Context, ref VersionRef) (io.ReadCloser, error) {
	return s.base.ReadVersion(ctx, ref)
}
func (s *s3ReadOnlyStore) PutVersion(context.Context, Location, string, files.Measurement, io.ReadSeeker) (VersionRef, error) {
	return VersionRef{}, files.ErrDependencyUnavailable
}
func (s *s3ReadOnlyStore) FindAttemptVersions(context.Context, Location, string, int) ([]VersionRef, error) {
	return nil, files.ErrDependencyUnavailable
}
