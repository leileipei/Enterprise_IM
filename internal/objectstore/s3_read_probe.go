package objectstore

import (
	"context"
	"io"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/leileipei/Enterprise_IM/internal/files"
)

const fileReadProbeKey = "_im_runtime/read-probe/v1"
const fileReadProbeContent = "enterprise-im-file-read-probe-v1\n"

func (s *s3ReadOnlyStore) ValidateReadProbe(ctx context.Context, version string) error {
	if !versionValid(version) || ctx.Err() != nil {
		return files.ErrDependencyUnavailable
	}
	out, e := s.base.client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(s.base.config.Bucket), Key: aws.String(fileReadProbeKey), VersionId: aws.String(version)})
	if e != nil {
		return files.ErrDependencyUnavailable
	}
	return validateReadProbeResponse(out, version)
}

func validateReadProbeResponse(out *s3.GetObjectOutput, version string) (err error) {
	if out == nil || out.Body == nil {
		return files.ErrDependencyUnavailable
	}
	defer func() {
		if out.Body.Close() != nil {
			err = files.ErrDependencyUnavailable
		}
	}()
	if aws.ToString(out.VersionId) != version || out.ContentLength != nil && *out.ContentLength != int64(len(fileReadProbeContent)) {
		return files.ErrDependencyUnavailable
	}
	// A single extra byte proves an oversized response; never consume its rest.
	body, e := io.ReadAll(io.LimitReader(out.Body, int64(len(fileReadProbeContent)+1)))
	if e != nil || string(body) != fileReadProbeContent {
		return files.ErrDependencyUnavailable
	}
	return nil
}
