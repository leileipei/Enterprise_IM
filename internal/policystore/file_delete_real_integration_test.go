package policystore_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
	"github.com/jackc/pgx/v5"
	"github.com/leileipei/Enterprise_IM/internal/filecleanup"
	"github.com/leileipei/Enterprise_IM/internal/files"
	"github.com/leileipei/Enterprise_IM/internal/filetransfer"
	"github.com/leileipei/Enterprise_IM/internal/objectstore"
	"github.com/leileipei/Enterprise_IM/internal/policystore"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"sync/atomic"
	"testing"
	"time"
)

func realCleanupAdapter(t *testing.T, endpoint string) objectstore.VersionDeleter {
	t.Helper()
	d, e := objectstore.NewS3VersionDeleter(objectstore.Config{Endpoint: endpoint, Bucket: os.Getenv("IM_TEST_S3_BUCKET"), Region: "us-east-1", PathStyle: true, CredentialSource: "cleanup_environment"})
	if e != nil {
		t.Fatal(e)
	}
	return d
}
func roleClient(t *testing.T, key, secret string) *s3.Client {
	t.Helper()
	if key == "" || secret == "" {
		t.Fatal("explicit role required")
	}
	return s3.NewFromConfig(aws.Config{Region: "us-east-1", Credentials: credentials.NewStaticCredentialsProvider(key, secret, ""), Retryer: func() aws.Retryer { return aws.NopRetryer{} }}, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(os.Getenv("IM_TEST_S3_ENDPOINT"))
		o.UsePathStyle = true
	})
}
func assertAccessDenied(t *testing.T, e error) {
	t.Helper()
	var api smithy.APIError
	if !errors.As(e, &api) || api.ErrorCode() != "AccessDenied" {
		t.Fatal("real operation was not denied")
	}
}
func realAttemptVersion(t *testing.T, c *pgx.Conn, m files.Metadata, seal bool) objectstore.VersionRef {
	t.Helper()
	attempt := runtimeAttempt(t, c, m)
	data := []byte("x")
	digest := sha256.Sum256(data)
	run(t, c, `UPDATE file_upload_attempts SET phase='received',actual_size_bytes=1,sha256=$2,detected_media_type='text/plain' WHERE id=$1`, attempt, digest[:])
	run(t, c, "UPDATE file_upload_attempts SET phase='storing' WHERE id=$1", attempt)
	ref, e := realTransferObjects(t).PutVersion(context.Background(), objectstore.Location{TenantID: m.TenantID, FileID: m.ID}, attempt, files.Measurement{SizeBytes: 1, SHA256: digest, DetectedMediaType: "text/plain"}, bytes.NewReader(data))
	if e != nil {
		t.Fatal("owned version upload", e)
	}
	if seal {
		run(t, c, "UPDATE file_upload_attempts SET phase='sealed',object_version_id=$2 WHERE id=$1", attempt, ref.VersionID)
	}
	return ref
}

// Only the expired reservation/upload history is constructed at the DB boundary.
// Bytes, versions, role permissions and every ready decision are actual storage
// and fresh scanner operations. No fake clean decision or disabled trigger.
func realDeleteFixture(t *testing.T) (*pgx.Conn, policystore.Service, files.Metadata, objectstore.VersionRef) {
	t.Helper()
	dedicatedUpload(t)
	c := fileMessageDB(t)
	seedDirectConversation(t, c)
	runtimePolicyEdit(t, c)
	m := expiredRealReservation(t, c)
	ref := realAttemptVersion(t, c, m, true)
	m = fileNext(m, "uploaded")
	m.ObjectVersionID = ref.VersionID
	digest := sha256.Sum256([]byte("x"))
	m.SHA256 = digest[:]
	m.DetectedMediaType = "text/plain"
	if e := writeFile(c, m, false); e != nil {
		t.Fatal(e)
	}
	repo := policystore.Service{DB: c}
	worker := filetransfer.ScanWorker{Repo: repo, Objects: scannerObjects(t), Scanner: actualScanner(t), SpoolDir: t.TempDir() + "/scan", OwnerID: uploadOwner}
	if found, e := worker.RunOnce(context.Background()); e != nil || !found {
		t.Fatal(found, e)
	}
	ready, e := repo.GetOwnFile(context.Background(), publisher(), m.ID)
	if e != nil || ready.State != files.StateReady {
		t.Fatal("real cleanup scanner failed", e)
	}
	cleanupPolicy(t, c, 365, true)
	return c, repo, ready, ref
}
func TestFileDeleteRealVersionsIAM(t *testing.T) {
	c, repo, m, first := realDeleteFixture(t)
	second := realAttemptVersion(t, c, m, true)
	d := realCleanupAdapter(t, os.Getenv("IM_TEST_S3_ENDPOINT"))
	ctx := context.Background()
	key := "tenants/" + m.TenantID + "/files/" + m.ID
	bucket := aws.String(os.Getenv("IM_TEST_S3_BUCKET"))
	cleanup := roleClient(t, os.Getenv("IM_FILE_CLEANUP_S3_ACCESS_KEY"), os.Getenv("IM_FILE_CLEANUP_S3_SECRET_KEY"))
	scan := roleClient(t, os.Getenv("IM_TEST_FILE_WORKER_ACCESS_KEY"), os.Getenv("IM_TEST_FILE_WORKER_SECRET_KEY"))
	upload := roleClient(t, os.Getenv("IM_TEST_FILE_UPLOAD_ACCESS_KEY"), os.Getenv("IM_TEST_FILE_UPLOAD_SECRET_KEY"))
	_, e := cleanup.PutObject(ctx, &s3.PutObjectInput{Bucket: bucket, Key: aws.String(key), Body: bytes.NewReader([]byte("denied"))})
	assertAccessDenied(t, e)
	_, e = scan.PutObject(ctx, &s3.PutObjectInput{Bucket: bucket, Key: aws.String(key), Body: bytes.NewReader([]byte("denied"))})
	assertAccessDenied(t, e)
	for _, client := range []*s3.Client{scan, upload} {
		_, e = client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: bucket, Key: aws.String(key), VersionId: aws.String(first.VersionID)})
		assertAccessDenied(t, e)
	}
	for _, version := range []*string{nil, aws.String(""), aws.String("null")} {
		_, e = cleanup.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: bucket, Key: aws.String(key), VersionId: version})
		assertAccessDenied(t, e)
	}
	page, e := d.ListVersions(ctx, first.Location, objectstore.VersionCursor{}, 1)
	if e != nil || page.Exhausted || page.Next.KeyMarker == "" {
		t.Fatal("actual bounded pagination", e)
	}
	worker, e := filecleanup.NewWorker(repo, d, uploadOwner)
	if e != nil {
		t.Fatal(e)
	}
	for i := 0; i < 3; i++ {
		found, e := worker.Step(ctx)
		if e != nil {
			var phase, reason string
			c.QueryRow(ctx, "SELECT phase,COALESCE(reason_code,'') FROM file_delete_jobs WHERE file_id=$1", m.ID).Scan(&phase, &reason)
			t.Fatal("real exact cleanup", i, phase, reason, e)
		}
		if !found {
			break
		}
	}
	for _, ref := range []objectstore.VersionRef{first, second} {
		p, e := d.ProbeVersion(ctx, ref)
		if e != nil || p != objectstore.VersionAbsent {
			t.Fatal("fixed version not absent", p, e)
		}
	}
	var state string
	var unresolved, finals int
	if e = c.QueryRow(ctx, `SELECT state,(SELECT count(*) FROM file_delete_versions WHERE phase<>'absent'),(SELECT count(*) FROM file_worker_audit_events WHERE operation='delete_finalize') FROM file_objects WHERE id=$1`, m.ID).Scan(&state, &unresolved, &finals); e != nil || state != "deleted" || unresolved != 0 || finals != 1 {
		t.Fatal(state, unresolved, finals, e)
	}
	// Anonymous HTTP cannot read a private object, including after physical deletion.
	res, e := http.Get(os.Getenv("IM_TEST_S3_ENDPOINT") + "/" + *bucket + "/" + key)
	if e != nil {
		t.Fatal(e)
	}
	res.Body.Close()
	if res.StatusCode != 403 {
		t.Fatal("anonymous access", res.StatusCode)
	}
}
func deletionFaultProxy(t *testing.T, deny *atomic.Bool, entered, release chan struct{}, deletes *atomic.Int32) *httptest.Server {
	t.Helper()
	origin, e := url.Parse(os.Getenv("IM_TEST_S3_ENDPOINT"))
	if e != nil {
		t.Fatal(e)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "DELETE" && deny.Load() {
			w.WriteHeader(503)
			return
		}
		if r.Method == "DELETE" {
			deletes.Add(1)
			if entered != nil {
				select {
				case entered <- struct{}{}:
				default:
				}
				select {
				case <-release:
				case <-r.Context().Done():
					return
				}
			}
		}
		r.URL.Scheme = origin.Scheme
		r.URL.Host = origin.Host
		r.RequestURI = ""
		res, e := http.DefaultTransport.RoundTrip(r)
		if e != nil {
			w.WriteHeader(503)
			return
		}
		defer res.Body.Close()
		if r.Method == "DELETE" && deny.Load() {
			io.Copy(io.Discard, res.Body)
			w.WriteHeader(503)
			return
		}
		for k, v := range res.Header {
			w.Header()[k] = v
		}
		w.WriteHeader(res.StatusCode)
		io.Copy(w, res.Body)
	}))
	t.Cleanup(server.Close)
	return server
}
func TestFileDeleteRealUnknownDelete(t *testing.T) {
	c, repo, m, ref := realDeleteFixture(t)
	var deny atomic.Bool
	var deletes atomic.Int32
	proxy := deletionFaultProxy(t, &deny, nil, nil, &deletes)
	// Inventory is successful; the proxy loses the real DELETE response and then
	// refuses probes. It never replaces a failure with an empty successful list.
	d := realCleanupAdapter(t, proxy.URL)
	ticket := claimDelete(t, repo)
	page, e := d.ListVersions(context.Background(), ref.Location, objectstore.VersionCursor{}, 100)
	if e != nil || !page.Exhausted {
		t.Fatal(e)
	}
	in := filecleanup.Inventory{Exhausted: true}
	for _, v := range page.Versions {
		in.Versions = append(in.Versions, filecleanup.Version{VersionID: v.Ref.VersionID, AttemptID: v.AttemptID})
	}
	if e = repo.RecordFileDeleteInventory(context.Background(), ticket, in); e != nil {
		t.Fatal(e)
	}
	permit := commitDelete(t, repo, ticket, ref.VersionID)
	deny.Store(true)
	if e = d.DeleteVersion(context.Background(), ref); e == nil {
		t.Fatal("lost DELETE accepted")
	}
	if p, e := d.ProbeVersion(context.Background(), ref); e == nil || p == objectstore.VersionAbsent {
		t.Fatal("failed probe settled", p, e)
	}
	if e = rawSchemaHold(c, m.ConversationID); e == nil {
		t.Fatal("hold protected unknown DELETE")
	}
	cleanupPolicy(t, c, 365, false)
	run(t, c, "UPDATE file_delete_jobs SET lease_expires_at=created_at+interval '1 microsecond',updated_at=clock_timestamp() WHERE id=$1", ticket.JobID)
	deny.Store(false)
	worker, _ := filecleanup.NewWorker(repo, d, clientB)
	if found, e := worker.Step(context.Background()); e != nil || !found {
		t.Fatal("paused exact reconciliation", found, e)
	}
	var id, phase string
	if e = c.QueryRow(context.Background(), "SELECT commitment_id::text,phase FROM file_delete_versions WHERE job_id=$1", ticket.JobID).Scan(&id, &phase); e != nil || id != permit.CommitmentID || phase != "absent" || deletes.Load() != 1 {
		t.Fatal("recovery replaced permit", phase, deletes.Load(), e)
	}
	if e = rawSchemaHold(c, m.ConversationID); e != nil {
		t.Fatal(e)
	}
	var state string
	if e = c.QueryRow(context.Background(), "SELECT state FROM file_objects WHERE id=$1", m.ID).Scan(&state); e != nil || state != "delete_pending" {
		t.Fatal("pause released quota", state, e)
	}
}
func TestFileDeleteRealHoldOrdering(t *testing.T) {
	for _, first := range []string{"hold", "delete"} {
		t.Run(first, func(t *testing.T) {
			c, repo, m, ref := realDeleteFixture(t)
			var deny atomic.Bool
			var deletes atomic.Int32
			entered := make(chan struct{}, 1)
			release := make(chan struct{})
			proxy := deletionFaultProxy(t, &deny, entered, release, &deletes)
			d := realCleanupAdapter(t, proxy.URL)
			worker, _ := filecleanup.NewWorker(repo, d, uploadOwner)
			if first == "hold" {
				if e := rawSchemaHold(c, m.ConversationID); e != nil {
					t.Fatal(e)
				}
				if found, e := worker.Step(context.Background()); e != nil || found || deletes.Load() != 0 {
					t.Fatal(found, e)
				}
				p, e := d.ProbeVersion(context.Background(), ref)
				if e != nil || p != objectstore.VersionPresent {
					t.Fatal(p, e)
				}
				close(release)
				return
			}
			done := make(chan error, 1)
			go func() { _, e := worker.Step(context.Background()); done <- e }()
			select {
			case <-entered:
			case <-time.After(10 * time.Second):
				close(release)
				t.Fatal("actual DELETE never reached owned forwarder")
			}
			if e := rawSchemaHold(c, m.ConversationID); e == nil {
				close(release)
				t.Fatal("late hold crossed committed unsent DELETE")
			}
			cleanupPolicy(t, c, 365, false)
			run(t, c, "UPDATE file_delete_jobs SET lease_expires_at=created_at+interval '1 microsecond',updated_at=clock_timestamp()")
			close(release)
			if e := <-done; !errors.Is(e, filecleanup.ErrLeaseLost) {
				t.Fatal("late former owner settled", e)
			}
			next, _ := filecleanup.NewWorker(repo, realCleanupAdapter(t, os.Getenv("IM_TEST_S3_ENDPOINT")), clientB)
			if found, e := next.Step(context.Background()); e != nil || !found {
				t.Fatal(found, e)
			}
			if e := rawSchemaHold(c, m.ConversationID); e != nil {
				t.Fatal("settled original version retained barrier", e)
			}
			if deletes.Load() != 1 {
				t.Fatal("new DELETE granted during pause", deletes.Load())
			}
		})
	}
}
func TestFileDeleteRealOrphanQuarantine(t *testing.T) {
	dedicatedUpload(t)
	c := fileMessageDB(t)
	seedDirectConversation(t, c)
	runtimePolicyEdit(t, c)
	m := expiredRealReservation(t, c)
	ref := realAttemptVersion(t, c, m, false)
	cleanupPolicy(t, c, 365, true)
	repo := policystore.Service{DB: c}
	d := realCleanupAdapter(t, os.Getenv("IM_TEST_S3_ENDPOINT"))
	worker, _ := filecleanup.NewWorker(repo, d, uploadOwner)
	if found, e := worker.Step(context.Background()); !found || !errors.Is(e, filecleanup.ErrBlocked) {
		t.Fatal("unknown storing automatically deleted", found, e)
	}
	p, e := d.ProbeVersion(context.Background(), ref)
	if e != nil || p != objectstore.VersionPresent {
		t.Fatal(p, e)
	}
	var phase, reason string
	var quota, permits int64
	if e = c.QueryRow(context.Background(), `SELECT phase,reason_code,(SELECT sum(declared_size_bytes) FROM file_objects WHERE state<>'deleted'),(SELECT count(*) FROM file_delete_versions WHERE commitment_id IS NOT NULL) FROM file_delete_jobs WHERE file_id=$1`, m.ID).Scan(&phase, &reason, &quota, &permits); e != nil || phase != "blocked" || reason != "unknown_upload" || quota != 1 || permits != 0 {
		t.Fatal(phase, reason, quota, permits, e)
	}
}

func expiredRealReservation(t *testing.T, c *pgx.Conn) files.Metadata {
	t.Helper()
	m := freshFile()
	if e := c.QueryRow(context.Background(), "SELECT gen_random_uuid()::text,gen_random_uuid()::text").Scan(&m.ID, &m.UploadRequestID); e != nil {
		t.Fatal(e)
	}
	m.OriginalFilename = "集团过期附件.txt"
	m.DeclaredMediaType = "text/plain"
	digest, e := files.CreationDigest(files.CreateParams{TenantID: m.TenantID, ConversationID: m.ConversationID, UploaderUserID: m.UploaderUserID, UploaderMembershipID: m.UploaderMembershipID, UploadRequestID: m.UploadRequestID, OriginalFilename: m.OriginalFilename, DeclaredMediaType: m.DeclaredMediaType, DeclaredSizeBytes: m.DeclaredSizeBytes})
	if e != nil {
		t.Fatal(e)
	}
	m.RequestDigest = digest[:]
	if e = writeFile(c, m, true); e != nil {
		t.Fatal(e)
	}
	return m
}

func TestFileDeleteRealMarker403(t *testing.T) {
	c, repo, m, ref := realDeleteFixture(t)
	ctx := context.Background()
	root := roleClient(t, os.Getenv("IM_TEST_S3_ADMIN_ACCESS_KEY"), os.Getenv("IM_TEST_S3_ADMIN_SECRET_KEY"))
	out, e := root.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(os.Getenv("IM_TEST_S3_BUCKET")), Key: aws.String("tenants/" + m.TenantID + "/files/" + m.ID)})
	if e != nil || !aws.ToBool(out.DeleteMarker) {
		t.Fatal("owned marker creation failed")
	}
	d := realCleanupAdapter(t, os.Getenv("IM_TEST_S3_ENDPOINT"))
	page, e := d.ListVersions(ctx, ref.Location, objectstore.VersionCursor{}, 100)
	if e != nil {
		t.Fatal(e)
	}
	marker := false
	for _, v := range page.Versions {
		marker = marker || v.DeleteMarker
	}
	if !marker {
		t.Fatal("actual marker omitted")
	}
	markerRef := objectstore.VersionRef{Location: ref.Location, VersionID: aws.ToString(out.VersionId)}
	if p, e := d.ProbeVersion(ctx, markerRef); e == nil || p == objectstore.VersionAbsent {
		t.Fatal("marker treated absent", p, e)
	}
	worker, _ := filecleanup.NewWorker(repo, d, uploadOwner)
	if found, e := worker.Step(ctx); !found || !errors.Is(e, filecleanup.ErrBlocked) {
		t.Fatal("marker automatically deleted", found, e)
	}
	if p, e := d.ProbeVersion(ctx, ref); e != nil || p != objectstore.VersionPresent {
		t.Fatal("version under marker changed", p, e)
	}
	denied, e := objectstore.NewS3VersionDeleter(objectstore.Config{Endpoint: os.Getenv("IM_TEST_S3_ENDPOINT"), Region: "us-east-1", Bucket: os.Getenv("IM_TEST_S3_POLICY_BUCKET"), PathStyle: true, CredentialSource: "cleanup_environment"})
	if e != nil {
		t.Fatal(e)
	}
	if p, e := denied.ProbeVersion(ctx, ref); e == nil || p == objectstore.VersionAbsent {
		t.Fatal("actual 403 treated absent", p, e)
	}
	var phase string
	if e = c.QueryRow(ctx, "SELECT phase FROM file_delete_jobs WHERE file_id=$1", m.ID).Scan(&phase); e != nil || phase != "blocked" {
		t.Fatal(phase, e)
	}
}
