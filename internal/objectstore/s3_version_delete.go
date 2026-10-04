package objectstore

import (
	"context"
	"errors"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
	smithyhttp "github.com/aws/smithy-go/transport/http"
	"github.com/leileipei/Enterprise_IM/internal/files"
)

type s3VersionDeleter struct {
	client *s3.Client
	config Config
}

func NewS3VersionDeleter(c Config) (VersionDeleter, error) {
	client, e := newS3Client(c, "cleanup_environment", "IM_FILE_CLEANUP_S3_ACCESS_KEY", "IM_FILE_CLEANUP_S3_SECRET_KEY")
	if e != nil {
		return nil, e
	}
	return &s3VersionDeleter{client, c}, nil
}
func deleteCallContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, 30*time.Second)
}
func validVersionCursor(key string, c VersionCursor) bool {
	if c.KeyMarker == "" {
		return c.VersionMarker == ""
	}
	return strings.HasPrefix(c.KeyMarker, key) && len(c.KeyMarker) <= 2048 && utf8.ValidString(c.KeyMarker) && strings.IndexFunc(c.KeyMarker, unicode.IsControl) < 0 && (c.VersionMarker == "" || versionValid(c.VersionMarker))
}
func (d *s3VersionDeleter) ListVersions(parent context.Context, l Location, cursor VersionCursor, limit int) (VersionPage, error) {
	key, e := l.key()
	if e != nil || limit < 1 || limit > 100 || !validVersionCursor(key, cursor) {
		return VersionPage{}, files.ErrInvalidMetadata
	}
	ctx, cancel := deleteCallContext(parent)
	defer cancel()
	in := &s3.ListObjectVersionsInput{Bucket: aws.String(d.config.Bucket), Prefix: aws.String(key), MaxKeys: aws.Int32(int32(limit))}
	if cursor.KeyMarker != "" {
		in.KeyMarker = aws.String(cursor.KeyMarker)
	}
	if cursor.VersionMarker != "" {
		in.VersionIdMarker = aws.String(cursor.VersionMarker)
	}
	out, e := d.client.ListObjectVersions(ctx, in)
	if e != nil || out == nil || out.IsTruncated == nil || len(out.Versions)+len(out.DeleteMarkers) > limit {
		return VersionPage{}, files.ErrDependencyUnavailable
	}
	page := VersionPage{Exhausted: !aws.ToBool(out.IsTruncated)}
	if !page.Exhausted {
		page.Next = VersionCursor{aws.ToString(out.NextKeyMarker), aws.ToString(out.NextVersionIdMarker)}
		if page.Next.KeyMarker == "" || !validVersionCursor(key, page.Next) || page.Next == cursor || cursor.KeyMarker != "" && page.Next.KeyMarker < cursor.KeyMarker {
			return VersionPage{}, files.ErrDependencyUnavailable
		}
	}
	seen := map[string]bool{}
	for _, v := range out.Versions {
		vkey, vid := aws.ToString(v.Key), aws.ToString(v.VersionId)
		if !strings.HasPrefix(vkey, key) {
			return VersionPage{}, files.ErrDependencyUnavailable
		}
		if vkey != key {
			continue
		}
		if !versionValid(vid) {
			return VersionPage{}, files.ErrDependencyUnavailable
		}
		if seen[vid] {
			return VersionPage{}, files.ErrDependencyUnavailable
		}
		seen[vid] = true
		ref := VersionRef{l, vid}
		h, e := d.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(d.config.Bucket), Key: aws.String(key), VersionId: aws.String(vid)})
		if e != nil || h == nil || aws.ToString(h.VersionId) != vid || aws.ToBool(h.DeleteMarker) || !uuid.MatchString(h.Metadata["attempt-id"]) {
			return VersionPage{}, files.ErrDependencyUnavailable
		}
		page.Versions = append(page.Versions, InventoryVersion{Ref: ref, AttemptID: h.Metadata["attempt-id"]})
	}
	for _, v := range out.DeleteMarkers {
		vkey, vid := aws.ToString(v.Key), aws.ToString(v.VersionId)
		if !strings.HasPrefix(vkey, key) {
			return VersionPage{}, files.ErrDependencyUnavailable
		}
		if vkey != key {
			continue
		}
		if !versionValid(vid) {
			return VersionPage{}, files.ErrDependencyUnavailable
		}
		if seen[vid] {
			return VersionPage{}, files.ErrDependencyUnavailable
		}
		seen[vid] = true
		page.Versions = append(page.Versions, InventoryVersion{Ref: VersionRef{l, vid}, DeleteMarker: true})
	}
	if ctx.Err() != nil {
		return VersionPage{}, files.ErrDependencyUnavailable
	}
	return page, nil
}
func (d *s3VersionDeleter) ProbeVersion(parent context.Context, ref VersionRef) (VersionPresence, error) {
	key, e := ref.Location.key()
	if e != nil || !versionValid(ref.VersionID) {
		return VersionUnknown, files.ErrInvalidMetadata
	}
	ctx, cancel := deleteCallContext(parent)
	defer cancel()
	out, e := d.client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(d.config.Bucket), Key: aws.String(key), VersionId: aws.String(ref.VersionID), Range: aws.String("bytes=0-0")})
	if e == nil {
		if out != nil && out.Body != nil {
			defer out.Body.Close()
		}
		if out == nil || out.Body == nil || aws.ToString(out.VersionId) != ref.VersionID || aws.ToBool(out.DeleteMarker) || !uuid.MatchString(out.Metadata["attempt-id"]) || ctx.Err() != nil {
			return VersionUnknown, files.ErrDependencyUnavailable
		}
		return VersionPresent, nil
	}
	var api smithy.APIError
	var response *smithyhttp.ResponseError
	if !errors.As(e, &api) || api.ErrorCode() != "NoSuchVersion" || !errors.As(e, &response) || response.HTTPStatusCode() != 404 {
		return VersionUnknown, files.ErrDependencyUnavailable
	}
	// An authorized version-specific negative result is combined with an
	// exhaustive version listing. No generic key/bucket 404 can satisfy this.
	cursor := VersionCursor{}
	seen := map[VersionCursor]bool{}
	for n := 0; n < 10; n++ {
		if seen[cursor] {
			return VersionUnknown, files.ErrDependencyUnavailable
		}
		seen[cursor] = true
		page, e := d.ListVersions(ctx, ref.Location, cursor, 100)
		if e != nil {
			return VersionUnknown, e
		}
		for _, v := range page.Versions {
			if v.Ref.VersionID == ref.VersionID {
				return VersionUnknown, files.ErrDependencyUnavailable
			}
		}
		if page.Exhausted && ctx.Err() == nil {
			return VersionAbsent, nil
		}
		cursor = page.Next
	}
	return VersionUnknown, files.ErrDependencyUnavailable
}
func (d *s3VersionDeleter) DeleteVersion(parent context.Context, ref VersionRef) error {
	key, e := ref.Location.key()
	if e != nil || !versionValid(ref.VersionID) {
		return files.ErrInvalidMetadata
	}
	ctx, cancel := deleteCallContext(parent)
	defer cancel()
	// A successful request is not absence proof. ProbeVersion must settle it.
	_, e = d.client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(d.config.Bucket), Key: aws.String(key), VersionId: aws.String(ref.VersionID)})
	if e != nil || ctx.Err() != nil {
		return files.ErrDependencyUnavailable
	}
	return nil
}
