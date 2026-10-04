package objectstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	"github.com/leileipei/Enterprise_IM/internal/files"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type s3Store struct {
	client *s3.Client
	config Config
}

func NewS3(c Config) (Store, error) {
	u, e := url.Parse(c.Endpoint)
	if e != nil || u.User != nil || u.Host == "" || u.RawQuery != "" || u.Fragment != "" || u.Path != "" && u.Path != "/" || u.Scheme != "https" && (u.Scheme != "http" || u.Hostname() != "localhost" && u.Hostname() != "127.0.0.1" && u.Hostname() != "::1") {
		return nil, files.ErrDependencyUnavailable
	}
	if !regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`).MatchString(c.Bucket) || !regexp.MustCompile(`^[a-z0-9-]{1,64}$`).MatchString(c.Region) || c.CredentialSource != "environment" {
		return nil, files.ErrDependencyUnavailable
	}
	key, secret := os.Getenv("IM_FILE_S3_ACCESS_KEY"), os.Getenv("IM_FILE_S3_SECRET_KEY")
	if key == "" || secret == "" {
		return nil, files.ErrDependencyUnavailable
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = 30 * time.Second
	httpClient := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("object redirect refused") }}
	cfg := aws.Config{Region: c.Region, Credentials: credentials.NewStaticCredentialsProvider(key, secret, ""), HTTPClient: httpClient, Retryer: func() aws.Retryer { return aws.NopRetryer{} }, RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired, ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired}
	client := s3.NewFromConfig(cfg, func(o *s3.Options) { o.BaseEndpoint = aws.String(c.Endpoint); o.UsePathStyle = c.PathStyle })
	return &s3Store{client, c}, nil
}
func (s *s3Store) ValidateCapabilities(ctx context.Context) error {
	v, e := s.client.GetBucketVersioning(ctx, &s3.GetBucketVersioningInput{Bucket: aws.String(s.config.Bucket)})
	if e != nil || v.Status != types.BucketVersioningStatusEnabled {
		return files.ErrDependencyUnavailable
	}
	acl, e := s.client.GetBucketAcl(ctx, &s3.GetBucketAclInput{Bucket: aws.String(s.config.Bucket)})
	if e != nil {
		return files.ErrDependencyUnavailable
	}
	for _, g := range acl.Grants {
		if g.Grantee == nil || g.Grantee.Type != types.TypeCanonicalUser {
			return files.ErrDependencyUnavailable
		}
	}
	p, e := s.client.GetBucketPolicy(ctx, &s3.GetBucketPolicyInput{Bucket: aws.String(s.config.Bucket)})
	if e != nil {
		var api smithy.APIError
		if !errors.As(e, &api) || api.ErrorCode() != "NoSuchBucketPolicy" {
			return files.ErrDependencyUnavailable
		}
	} else {
		var policy struct {
			Statement []struct {
				Effect       string
				Principal    json.RawMessage
				NotPrincipal json.RawMessage
			}
		}
		if p.Policy == nil || len(*p.Policy) > 65536 || json.Unmarshal([]byte(*p.Policy), &policy) != nil {
			return files.ErrDependencyUnavailable
		}
		for _, stmt := range policy.Statement {
			if stmt.Effect == "Allow" && (len(stmt.NotPrincipal) > 0 || len(stmt.Principal) == 0 || strings.Contains(string(stmt.Principal), "*")) {
				return files.ErrDependencyUnavailable
			}
		}
	}
	return nil
}
func digestReader(r io.Reader, size int64) ([32]byte, error) {
	h := sha256.New()
	n, e := io.Copy(h, io.LimitReader(r, size+1))
	if e != nil || n != size {
		return [32]byte{}, files.ErrInvalidFileSize
	}
	var d [32]byte
	copy(d[:], h.Sum(nil))
	return d, nil
}
func (s *s3Store) PutVersion(ctx context.Context, l Location, attempt string, m files.Measurement, r io.ReadSeeker) (VersionRef, error) {
	var zero VersionRef
	switch m.DetectedMediaType {
	case "text/plain", "image/png", "image/jpeg", "application/pdf", "application/octet-stream":
	default:
		return zero, files.ErrInvalidMetadata
	}
	key, e := l.key()
	if e != nil || !uuid.MatchString(attempt) || m.SizeBytes < 1 || m.SizeBytes > files.MaxFileSizeBytes || r == nil {
		return zero, files.ErrInvalidMetadata
	}
	if _, e = r.Seek(0, io.SeekStart); e != nil {
		return zero, files.ErrDependencyUnavailable
	}
	d, e := digestReader(r, m.SizeBytes)
	if e != nil {
		return zero, e
	}
	if d != m.SHA256 {
		return zero, files.ErrUploadConflict
	}
	if _, e = r.Seek(0, io.SeekStart); e != nil {
		return zero, files.ErrDependencyUnavailable
	}
	out, e := s.client.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(s.config.Bucket), Key: aws.String(key), Body: r, ContentLength: aws.Int64(m.SizeBytes), ContentType: aws.String("application/octet-stream"), Metadata: map[string]string{"attempt-id": attempt, "size-bytes": strconv.FormatInt(m.SizeBytes, 10), "sha256": hex.EncodeToString(m.SHA256[:]), "detected-media-type": m.DetectedMediaType}}, func(o *s3.Options) { o.Retryer = aws.NopRetryer{} })
	if e != nil || out.VersionId == nil || !versionValid(*out.VersionId) {
		return zero, files.ErrDependencyUnavailable
	}
	ref := VersionRef{Location: l, VersionID: *out.VersionId}
	body, e := s.ReadVersion(ctx, ref)
	if e != nil {
		return zero, e
	}
	d, e = digestReader(body, m.SizeBytes)
	closeErr := body.Close()
	if e != nil || closeErr != nil || d != m.SHA256 {
		return zero, files.ErrDependencyUnavailable
	}
	return ref, nil
}
func (s *s3Store) getVersion(ctx context.Context, ref VersionRef) (*s3.GetObjectOutput, error) {
	key, e := ref.Location.key()
	if e != nil || !versionValid(ref.VersionID) {
		return nil, files.ErrInvalidMetadata
	}
	out, e := s.client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(s.config.Bucket), Key: aws.String(key), VersionId: aws.String(ref.VersionID)})
	if e != nil {
		return nil, files.ErrDependencyUnavailable
	}
	if out.Body == nil || aws.ToString(out.VersionId) != ref.VersionID || out.ContentLength != nil && (*out.ContentLength < 1 || *out.ContentLength > files.MaxFileSizeBytes) {
		if out.Body != nil {
			out.Body.Close()
		}
		return nil, files.ErrDependencyUnavailable
	}
	return out, nil
}

type boundedBody struct {
	io.ReadCloser
	remaining int64
}

func (b *boundedBody) Read(p []byte) (int, error) {
	if b.remaining <= 0 {
		return 0, files.ErrFileTooLarge
	}
	if int64(len(p)) > b.remaining {
		p = p[:b.remaining]
	}
	n, e := b.ReadCloser.Read(p)
	b.remaining -= int64(n)
	return n, e
}
func (s *s3Store) ReadVersion(ctx context.Context, ref VersionRef) (io.ReadCloser, error) {
	out, e := s.getVersion(ctx, ref)
	if e != nil {
		return nil, e
	}
	return &boundedBody{out.Body, files.MaxFileSizeBytes + 1}, nil
}
func (s *s3Store) FindAttemptVersions(ctx context.Context, l Location, attempt string, limit int) ([]VersionRef, error) {
	key, e := l.key()
	if e != nil || !uuid.MatchString(attempt) || limit < 1 || limit > 100 {
		return nil, files.ErrInvalidMetadata
	}
	var results []VersionRef
	var keyMarker, versionMarker *string
	total := 0
	for page := 0; page < 10; page++ {
		out, e := s.client.ListObjectVersions(ctx, &s3.ListObjectVersionsInput{Bucket: aws.String(s.config.Bucket), Prefix: aws.String(key), MaxKeys: aws.Int32(10), KeyMarker: keyMarker, VersionIdMarker: versionMarker})
		if e != nil {
			return nil, files.ErrDependencyUnavailable
		}
		total += len(out.Versions) + len(out.DeleteMarkers)
		if total > limit {
			return nil, files.ErrRecoveryPending
		}
		for _, v := range out.Versions {
			if aws.ToString(v.Key) != key {
				continue
			}
			ref := VersionRef{Location: l, VersionID: aws.ToString(v.VersionId)}
			obj, e := s.getVersion(ctx, ref)
			if e != nil {
				return nil, e
			}
			if obj.Metadata["attempt-id"] != attempt {
				obj.Body.Close()
				continue
			}
			size, e := strconv.ParseInt(obj.Metadata["size-bytes"], 10, 64)
			hash, e2 := hex.DecodeString(obj.Metadata["sha256"])
			if e != nil || e2 != nil || len(hash) != 32 || size < 1 || size > files.MaxFileSizeBytes || strconv.FormatInt(size, 10) != obj.Metadata["size-bytes"] {
				obj.Body.Close()
				return nil, files.ErrRecoveryPending
			}
			d, e := digestReader(obj.Body, size)
			closeErr := obj.Body.Close()
			if e != nil || closeErr != nil || hex.EncodeToString(d[:]) != hex.EncodeToString(hash) {
				return nil, files.ErrRecoveryPending
			}
			results = append(results, ref)
		}
		if !aws.ToBool(out.IsTruncated) {
			return results, nil
		}
		if total >= limit || out.NextKeyMarker == nil || out.NextVersionIdMarker == nil || aws.ToString(out.NextKeyMarker) == aws.ToString(keyMarker) && aws.ToString(out.NextVersionIdMarker) == aws.ToString(versionMarker) {
			return nil, files.ErrRecoveryPending
		}
		keyMarker, versionMarker = out.NextKeyMarker, out.NextVersionIdMarker
	}
	return nil, files.ErrRecoveryPending
}
