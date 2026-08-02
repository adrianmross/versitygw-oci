// versitygw backend plugin for OCI Object Storage.
//
// Speaks S3 to clients (versitygw handles routing, headers and SigV4) and the
// OCI *native* Object Storage API to the backend. That distinction is the whole
// point: OCI's S3-compatibility endpoint authenticates only with a Customer
// Secret Key, which can only be issued against an IAM user, whereas the native
// API accepts OKE workload identity, instance principals and resource
// principals. Clients that can only speak S3 keep speaking S3, and no long-lived
// credential is needed.
//
// Built as a Go plugin (-buildmode=plugin). The plugin and the versitygw binary
// MUST come from the same module graph and toolchain or the runtime symbol load
// fails; the Dockerfile builds both from a pinned versitygw checkout for that
// reason.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/oracle/oci-go-sdk/v65/common"
	"github.com/oracle/oci-go-sdk/v65/common/auth"
	"github.com/oracle/oci-go-sdk/v65/objectstorage"
	vgwauth "github.com/versity/versitygw/auth"
	"github.com/versity/versitygw/backend"
	"github.com/versity/versitygw/plugins"
	"github.com/versity/versitygw/s3err"
	"github.com/versity/versitygw/s3response"
)

// startupTimeout bounds the one namespace lookup done during plugin load. Plugin
// load blocks the gateway starting, so an unreachable OCI must fail fast.
const startupTimeout = 15 * time.Second

// retryPolicy retries throttles and transient server errors with exponential
// backoff. The SDK applies no retry unless one is set, so without this a single
// 429 mid-upload fails the request outright.
var retryPolicy = common.DefaultRetryPolicyWithoutEventualConsistency()

// Backend is the symbol versitygw looks up in the .so.
var Backend plugins.BackendPlugin = &ociPlugin{}

type ociPlugin struct{}

func (p *ociPlugin) New(config string) (backend.Backend, error) { return newOCI(config) }

// OCI implements the subset of backend.Backend needed by ordinary S3 clients:
// bucket head/list, object get/put/head/delete, listing v1 and v2, copy, and the
// full multipart family. Everything else falls through to BackendUnsupported,
// which returns NotImplemented rather than silently doing the wrong thing.
type OCI struct {
	backend.BackendUnsupported

	client      objectstorage.ObjectStorageClient
	namespace   string
	compartment string
	// bucket name -> owning account access key. Empty means every bucket is
	// root-owned (single-account mode).
	bucketOwners map[string]string
}

// Fails the build if a versitygw bump changes the Backend interface, rather
// than failing at plugin load time in the cluster.
var _ backend.Backend = (*OCI)(nil)

// newOCI builds the client. `config` is the plugin config string from
// versitygw; empty means take everything from the environment.
//
// Auth order: OKE workload identity, then the default OCI chain (config file /
// instance principal) for local development and testing. Workload identity is
// the point of this backend, so it is tried first and only falls back when the
// projected token is absent.
func newOCI(config string) (*OCI, error) {
	cfg := parseConfig(config)

	region := firstNonEmpty(cfg["region"], os.Getenv("OCI_REGION"))
	ns := firstNonEmpty(cfg["namespace"], os.Getenv("OCI_NAMESPACE"))
	compartment := firstNonEmpty(cfg["compartment"], os.Getenv("OCI_COMPARTMENT_ID"))

	provider, err := configurationProvider(cfg["auth"])
	if err != nil {
		return nil, fmt.Errorf("oci auth: %w", err)
	}

	client, err := objectstorage.NewObjectStorageClientWithConfigurationProvider(provider)
	if err != nil {
		return nil, fmt.Errorf("oci object storage client: %w", err)
	}
	// The SDK's default http.Client carries an absolute 60s Timeout that covers
	// the whole exchange INCLUDING the response body, so a GetObject dies
	// mid-stream whenever the client reads slower than size/60s. That is
	// time-based, not size-based: the same 3.86GB object completes when consumed
	// at 110 MB/s and breaks at 62s when consumed at 26 MB/s, at a different byte
	// offset each time.
	//
	// Writes never hit it because multipart splits an upload into sub-60s part
	// requests. Reads are one request, so this only surfaces on a large object
	// with a slow consumer — which is exactly a Postgres base-backup restore,
	// where barman decompresses and untars as it reads.
	//
	// Timeout: 0 means no deadline on the body. The per-phase timeouts below
	// still bound connect, TLS and time-to-first-byte, so a genuinely hung peer
	// is still caught.
	client.HTTPClient = streamingHTTPClient()
	if region != "" {
		client.SetRegion(region)
	}
	// Retry throttles and transient 5xx rather than surfacing them to the S3
	// client. Without this the SDK applies no retry of its own, so a single 429
	// during a large multipart upload fails the whole part.
	client.SetCustomClientConfiguration(common.CustomClientConfiguration{
		RetryPolicy: &retryPolicy,
	})

	o := &OCI{
		client:       client,
		namespace:    ns,
		compartment:  compartment,
		bucketOwners: parseBucketOwners(cfg["bucketowners"]),
	}
	if o.namespace == "" {
		// One call at startup beats requiring the operator to hardcode a value
		// they cannot easily look up. Bounded: this runs during plugin load, and
		// an unreachable OCI must fail the gateway fast rather than hang it.
		ctx, cancel := context.WithTimeout(context.Background(), startupTimeout)
		defer cancel()
		resp, err := client.GetNamespace(ctx, objectstorage.GetNamespaceRequest{})
		if err != nil {
			return nil, fmt.Errorf("resolve object storage namespace: %w", err)
		}
		o.namespace = *resp.Value
	}
	return o, nil
}

// streamingHTTPClient replaces the SDK's default dispatcher for the reason
// documented at its call site: no absolute deadline on the body, but every other
// phase still bounded.
func streamingHTTPClient() *http.Client {
	return &http.Client{
		Timeout: 0,
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          100,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 60 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
		},
	}
}

func configurationProvider(mode string) (common.ConfigurationProvider, error) {
	switch strings.ToLower(mode) {
	case "workload_identity", "oke_workload_identity":
		return auth.OkeWorkloadIdentityConfigurationProvider()
	case "instance_principal":
		return auth.InstancePrincipalConfigurationProvider()
	case "default", "config_file":
		return common.DefaultConfigProvider(), nil
	case "":
		if p, err := auth.OkeWorkloadIdentityConfigurationProvider(); err == nil {
			return p, nil
		}
		return common.DefaultConfigProvider(), nil
	default:
		return nil, fmt.Errorf("unknown auth mode %q", mode)
	}
}

// parseConfig accepts `key=value` pairs separated by commas or whitespace.
//
// versitygw's `plugin --config` passes a *file path*, so a value naming a
// readable file is read and its contents parsed. An inline string is also
// accepted, which keeps tests and local runs simple.
func parseConfig(s string) map[string]string {
	if s != "" {
		if data, err := os.ReadFile(s); err == nil {
			s = string(data)
		}
	}
	out := map[string]string{}
	for _, field := range strings.FieldsFunc(s, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\n' || r == '\t'
	}) {
		k, v, ok := strings.Cut(field, "=")
		if ok {
			out[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return out
}

// parseBucketOwners parses `bucket:account|bucket:account`. Pipe-separated
// because the outer config is already comma/space separated.
func parseBucketOwners(s string) map[string]string {
	out := map[string]string{}
	for _, pair := range strings.Split(s, "|") {
		bucket, account, ok := strings.Cut(strings.TrimSpace(pair), ":")
		bucket, account = strings.TrimSpace(bucket), strings.TrimSpace(account)
		if ok && bucket != "" && account != "" {
			out[bucket] = account
		}
	}
	return out
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func (o *OCI) String() string { return "OCI Object Storage" }
func (o *OCI) Shutdown()      {}

// mapError translates OCI service errors into the S3 errors clients expect.
// Without this every failure surfaces as InternalError and clients cannot tell
// "missing key" from "broken gateway" — container registries in particular,
// whose blob
// upload path depends on distinguishing a 404 from a real fault.
func mapError(err error) error {
	if err == nil {
		return nil
	}
	var svcErr common.ServiceError
	if !errorAs(err, &svcErr) {
		return err
	}
	switch svcErr.GetHTTPStatusCode() {
	case 404:
		switch svcErr.GetCode() {
		case "BucketNotFound", "NamespaceNotFound":
			return s3err.GetAPIError(s3err.ErrNoSuchBucket)
		default:
			return s3err.GetAPIError(s3err.ErrNoSuchKey)
		}
	case 401, 403:
		return s3err.GetAPIError(s3err.ErrAccessDenied)
	case 409:
		return s3err.GetAPIError(s3err.ErrBucketAlreadyExists)
	case 412:
		return s3err.GetAPIError(s3err.ErrPreconditionFailed)
	}
	return err
}

// errorAs unwraps rather than type-asserting. The SDK wraps service errors on
// some paths (notably after a retry), and a bare assertion silently misses those
// — which collapses every mapped error back into InternalError, defeating the
// point of mapError.
func errorAs(err error, target *common.ServiceError) bool {
	return errors.As(err, target)
}

// GetBucketAcl reports bucket ownership.
//
// This is not optional: versitygw runs ParseAcl and AuthorizePublicBucketAccess
// as middleware on EVERY route, both of which call GetBucketAcl. Leaving it to
// BackendUnsupported makes the gateway answer 501 to every request before the
// controller is ever reached, which is not obvious from the symptom.
//
// OCI Object Storage has no S3 ACL model, so ownership comes from the
// `bucketOwners` config instead. This is what makes per-account credentials
// mean anything: versitygw's verifyACL denies when acl.Owner != the requesting
// access key, so a bucket mapped to account A is unreachable by account B.
//
// With no mapping the ACL is empty and versitygw defaults the owner to the root
// account — every bucket is then reachable only by root, which is the
// single-shared-credential model. Mapping a bucket is therefore opt-in, and
// unmapped buckets keep working exactly as before.
func (o *OCI) GetBucketAcl(_ context.Context, in *s3.GetBucketAclInput) ([]byte, error) {
	if in != nil && in.Bucket != nil {
		if owner, ok := o.bucketOwners[*in.Bucket]; ok {
			return json.Marshal(vgwauth.ACL{Owner: owner})
		}
	}
	return json.Marshal(vgwauth.ACL{})
}

// GetObjectLockConfiguration reports that no object lock is configured.
//
// Also not optional: every write path (PutObject, DeleteObject, DeleteObjects,
// CompleteMultipartUpload) runs auth.CheckObjectAccess, which calls this.
// Returning ErrObjectLockConfigurationNotFound is the sanctioned "no lock"
// signal — CheckObjectAccess treats that specific error as "allowed" and any
// other error as a hard failure, so leaving it to BackendUnsupported makes
// every mutating request 501.
//
// OCI has retention rules, but they are not S3 object lock and are enforced
// server-side by OCI regardless of what the gateway reports.
func (o *OCI) GetObjectLockConfiguration(_ context.Context, _ string) ([]byte, error) {
	return nil, s3err.GetAPIError(s3err.ErrObjectLockConfigurationNotFound)
}

// GetBucketVersioning reports versioning as unset.
//
// OCI bucket versioning exists but does not map onto S3 version ids, and this
// backend does not expose per-version operations. Reporting it as unset keeps
// clients from attempting version-aware calls the backend cannot serve.
func (o *OCI) GetBucketVersioning(_ context.Context, _ string) (s3response.GetBucketVersioningOutput, error) {
	return s3response.GetBucketVersioningOutput{}, nil
}

func (o *OCI) ListBuckets(ctx context.Context, in s3response.ListBucketsInput) (s3response.ListAllMyBucketsResult, error) {
	compartment := o.compartment
	if compartment == "" {
		return s3response.ListAllMyBucketsResult{}, fmt.Errorf("compartment must be set (config `compartment=` or OCI_COMPARTMENT_ID) to list buckets")
	}

	var entries []s3response.ListAllMyBucketsEntry
	var page *string
	for {
		resp, err := o.client.ListBuckets(ctx, objectstorage.ListBucketsRequest{
			NamespaceName: &o.namespace,
			CompartmentId: &compartment,
			Page:          page,
		})
		if err != nil {
			return s3response.ListAllMyBucketsResult{}, mapError(err)
		}
		for _, b := range resp.Items {
			name := deref(b.Name)
			if in.Prefix != "" && !strings.HasPrefix(name, in.Prefix) {
				continue
			}
			entry := s3response.ListAllMyBucketsEntry{Name: name}
			if b.TimeCreated != nil {
				entry.CreationDate = b.TimeCreated.Time
			}
			entries = append(entries, entry)
		}
		if resp.OpcNextPage == nil {
			break
		}
		page = resp.OpcNextPage
	}

	return s3response.ListAllMyBucketsResult{
		Owner:   s3response.CanonicalUser{ID: in.Owner},
		Buckets: s3response.ListAllMyBucketsList{Bucket: entries},
		Prefix:  in.Prefix,
	}, nil
}

func (o *OCI) HeadBucket(ctx context.Context, in *s3.HeadBucketInput) (*s3.HeadBucketOutput, error) {
	_, err := o.client.HeadBucket(ctx, objectstorage.HeadBucketRequest{
		NamespaceName: &o.namespace,
		BucketName:    in.Bucket,
	})
	if err != nil {
		return nil, mapError(err)
	}
	return &s3.HeadBucketOutput{}, nil
}

func (o *OCI) PutObject(ctx context.Context, in s3response.PutObjectInput) (s3response.PutObjectOutput, error) {
	body := in.Body
	if body == nil {
		body = strings.NewReader("")
	}
	req := objectstorage.PutObjectRequest{
		NamespaceName: &o.namespace,
		BucketName:    in.Bucket,
		ObjectName:    in.Key,
		PutObjectBody: io.NopCloser(body),
		ContentLength: in.ContentLength,
		ContentType:   in.ContentType,
		OpcMeta:       in.Metadata,
	}
	if in.ContentEncoding != nil {
		req.ContentEncoding = in.ContentEncoding
	}
	if in.CacheControl != nil {
		req.CacheControl = in.CacheControl
	}
	resp, err := o.client.PutObject(ctx, req)
	if err != nil {
		return s3response.PutObjectOutput{}, mapError(err)
	}
	return s3response.PutObjectOutput{ETag: quote(deref(resp.ETag))}, nil
}

func (o *OCI) GetObject(ctx context.Context, in *s3.GetObjectInput) (*s3.GetObjectOutput, error) {
	req := objectstorage.GetObjectRequest{
		NamespaceName: &o.namespace,
		BucketName:    in.Bucket,
		ObjectName:    in.Key,
	}
	// versitygw passes a non-nil empty Range for a whole-object GET; OCI rejects
	// an empty Range header with 416 InvalidRange, which surfaces to clients as
	// a confusing 500 on ordinary reads.
	if in.Range != nil && *in.Range != "" {
		req.Range = in.Range
	}
	resp, err := o.client.GetObject(ctx, req)
	if err != nil {
		return nil, mapError(err)
	}
	out := &s3.GetObjectOutput{
		Body:          resp.Content,
		ContentLength: resp.ContentLength,
		ContentType:   resp.ContentType,
		Metadata:      resp.OpcMeta,
	}
	if resp.ETag != nil {
		out.ETag = ptr(quote(*resp.ETag))
	}
	if resp.LastModified != nil {
		out.LastModified = &resp.LastModified.Time
	}
	return out, nil
}

func (o *OCI) HeadObject(ctx context.Context, in *s3.HeadObjectInput) (*s3.HeadObjectOutput, error) {
	resp, err := o.client.HeadObject(ctx, objectstorage.HeadObjectRequest{
		NamespaceName: &o.namespace,
		BucketName:    in.Bucket,
		ObjectName:    in.Key,
	})
	if err != nil {
		return nil, mapError(err)
	}
	out := &s3.HeadObjectOutput{
		ContentLength: resp.ContentLength,
		ContentType:   resp.ContentType,
		Metadata:      resp.OpcMeta,
	}
	if resp.ETag != nil {
		out.ETag = ptr(quote(*resp.ETag))
	}
	if resp.LastModified != nil {
		out.LastModified = &resp.LastModified.Time
	}
	return out, nil
}

func (o *OCI) DeleteObject(ctx context.Context, in *s3.DeleteObjectInput) (*s3.DeleteObjectOutput, error) {
	_, err := o.client.DeleteObject(ctx, objectstorage.DeleteObjectRequest{
		NamespaceName: &o.namespace,
		BucketName:    in.Bucket,
		ObjectName:    in.Key,
	})
	if err != nil {
		// S3 DeleteObject is idempotent: deleting a missing key is a success.
		if isNotFound(err) {
			return &s3.DeleteObjectOutput{}, nil
		}
		return nil, mapError(err)
	}
	return &s3.DeleteObjectOutput{}, nil
}

func (o *OCI) DeleteObjects(ctx context.Context, in *s3.DeleteObjectsInput) (s3response.DeleteResult, error) {
	var res s3response.DeleteResult
	if in.Delete == nil {
		return res, nil
	}
	// ponytail: serial deletes. OCI has no batch-delete API, and consumers here
	// delete tens of keys, not thousands. Parallelise with a bounded worker pool
	// if a caller ever sends large batches.
	for _, obj := range in.Delete.Objects {
		_, err := o.client.DeleteObject(ctx, objectstorage.DeleteObjectRequest{
			NamespaceName: &o.namespace,
			BucketName:    in.Bucket,
			ObjectName:    obj.Key,
		})
		if err != nil && !isNotFound(err) {
			res.Error = append(res.Error, types.Error{
				Key:     obj.Key,
				Code:    ptr("InternalError"),
				Message: ptr(err.Error()),
			})
			continue
		}
		res.Deleted = append(res.Deleted, types.DeletedObject{Key: obj.Key})
	}
	return res, nil
}

// listPage is the shared body of ListObjects and ListObjectsV2. OCI paginates
// with an opaque `NextStartWith` cursor rather than S3's marker/continuation
// token, so the cursor is passed straight through as the token — it is opaque
// to clients on both sides.
func (o *OCI) listPage(ctx context.Context, bucket *string, prefix, delimiter, start *string, maxKeys int32) (objectstorage.ListObjectsResponse, error) {
	limit := int(maxKeys)
	if limit <= 0 || limit > 1000 {
		limit = 1000
	}
	// OCI rejects empty-string query parameters ("The delimiter parameter '' is
	// not supported"), and versitygw hands us non-nil empty strings for absent
	// options. Same trap as the empty Range header in GetObject.
	req := objectstorage.ListObjectsRequest{
		NamespaceName: &o.namespace,
		BucketName:    bucket,
		Prefix:        nilIfEmpty(prefix),
		Delimiter:     nilIfEmpty(delimiter),
		Start:         nilIfEmpty(start),
		Limit:         &limit,
		Fields:        ptr("name,size,etag,timeModified"),
	}
	return o.client.ListObjects(ctx, req)
}

func (o *OCI) ListObjects(ctx context.Context, in *s3.ListObjectsInput) (s3response.ListObjectsResult, error) {
	maxKeys := int32(1000)
	if in.MaxKeys != nil {
		maxKeys = *in.MaxKeys
	}
	resp, err := o.listPage(ctx, in.Bucket, in.Prefix, in.Delimiter, in.Marker, maxKeys)
	if err != nil {
		return s3response.ListObjectsResult{}, mapError(err)
	}
	contents, prefixes := convertListing(resp)
	truncated := resp.NextStartWith != nil
	return s3response.ListObjectsResult{
		Name:           in.Bucket,
		Prefix:         in.Prefix,
		Marker:         in.Marker,
		NextMarker:     resp.NextStartWith,
		MaxKeys:        &maxKeys,
		Delimiter:      in.Delimiter,
		IsTruncated:    &truncated,
		Contents:       contents,
		CommonPrefixes: prefixes,
	}, nil
}

func (o *OCI) ListObjectsV2(ctx context.Context, in *s3.ListObjectsV2Input) (s3response.ListObjectsV2Result, error) {
	maxKeys := int32(1000)
	if in.MaxKeys != nil {
		maxKeys = *in.MaxKeys
	}
	start := in.ContinuationToken
	if start == nil {
		start = in.StartAfter
	}
	resp, err := o.listPage(ctx, in.Bucket, in.Prefix, in.Delimiter, start, maxKeys)
	if err != nil {
		return s3response.ListObjectsV2Result{}, mapError(err)
	}
	contents, prefixes := convertListing(resp)
	truncated := resp.NextStartWith != nil
	count := int32(len(contents))
	return s3response.ListObjectsV2Result{
		Name:                  in.Bucket,
		Prefix:                in.Prefix,
		StartAfter:            in.StartAfter,
		ContinuationToken:     in.ContinuationToken,
		NextContinuationToken: resp.NextStartWith,
		KeyCount:              &count,
		MaxKeys:               &maxKeys,
		Delimiter:             in.Delimiter,
		IsTruncated:           &truncated,
		Contents:              contents,
		CommonPrefixes:        prefixes,
	}, nil
}

func convertListing(resp objectstorage.ListObjectsResponse) ([]s3response.Object, []types.CommonPrefix) {
	contents := make([]s3response.Object, 0, len(resp.Objects))
	for _, obj := range resp.Objects {
		item := s3response.Object{Key: obj.Name, Size: obj.Size}
		if obj.Etag != nil {
			item.ETag = ptr(quote(*obj.Etag))
		}
		if obj.TimeModified != nil {
			item.LastModified = &obj.TimeModified.Time
		}
		contents = append(contents, item)
	}
	prefixes := make([]types.CommonPrefix, 0, len(resp.Prefixes))
	for i := range resp.Prefixes {
		prefixes = append(prefixes, types.CommonPrefix{Prefix: &resp.Prefixes[i]})
	}
	return contents, prefixes
}

// ListObjectVersions maps OCI's object-version listing onto S3's.
//
// Required by anything doing storage accounting — the obsidian metrics exporter
// issues GET /bucket?versions and reports scrape_success=0 without it, which is
// how this gap was found after the first consumer cutover.
//
// OCI paginates with an opaque page token rather than S3's key/version-id marker
// pair, so the token is carried through NextKeyMarker and read back from
// KeyMarker. It is opaque to clients on both sides.
// convertVersions splits OCI's flat version list into S3's Versions and
// DeleteMarkers, and derives IsLatest.
//
// OCI returns versions newest-first per key, so the first sighting of a key in
// the page is its current version. That is an approximation across page
// boundaries; consumers needing authoritative current-object data use
// ListObjects instead.
func convertVersions(items []objectstorage.ObjectVersionSummary) ([]s3response.ObjectVersion, []types.DeleteMarkerEntry) {
	var versions []s3response.ObjectVersion
	var markers []types.DeleteMarkerEntry
	seen := map[string]bool{}
	for i := range items {
		it := items[i]
		key := deref(it.Name)
		latest := !seen[key]
		seen[key] = true

		if it.IsDeleteMarker != nil && *it.IsDeleteMarker {
			m := types.DeleteMarkerEntry{Key: it.Name, VersionId: it.VersionId, IsLatest: &latest}
			if it.TimeModified != nil {
				m.LastModified = &it.TimeModified.Time
			}
			markers = append(markers, m)
			continue
		}

		v := s3response.ObjectVersion{Key: it.Name, VersionId: it.VersionId, Size: it.Size, IsLatest: &latest}
		if it.Etag != nil {
			v.ETag = ptr(quote(*it.Etag))
		}
		if it.TimeModified != nil {
			v.LastModified = &it.TimeModified.Time
		}
		versions = append(versions, v)
	}
	return versions, markers
}

func (o *OCI) ListObjectVersions(ctx context.Context, in *s3.ListObjectVersionsInput) (s3response.ListVersionsResult, error) {
	maxKeys := int32(1000)
	if in.MaxKeys != nil {
		maxKeys = *in.MaxKeys
	}
	limit := int(maxKeys)
	if limit <= 0 || limit > 1000 {
		limit = 1000
	}
	resp, err := o.client.ListObjectVersions(ctx, objectstorage.ListObjectVersionsRequest{
		NamespaceName: &o.namespace,
		BucketName:    in.Bucket,
		Prefix:        nilIfEmpty(in.Prefix),
		Delimiter:     nilIfEmpty(in.Delimiter),
		Page:          nilIfEmpty(in.KeyMarker),
		Limit:         &limit,
		// Fields is a single-value *enum* here, unlike ListObjects where it is a
		// free-form comma string — the SDK validates it client-side and rejects
		// "name,size,...". Size is the one optional field consumers need (storage
		// accounting); name/versionId/timeModified/isDeleteMarker are mandatory in
		// the response and always returned.
		Fields: objectstorage.ListObjectVersionsFieldsSize,
	})
	if err != nil {
		return s3response.ListVersionsResult{}, mapError(err)
	}

	versions, markers := convertVersions(resp.Items)

	truncated := resp.OpcNextPage != nil
	prefixes := make([]types.CommonPrefix, 0, len(resp.Prefixes))
	for i := range resp.Prefixes {
		prefixes = append(prefixes, types.CommonPrefix{Prefix: &resp.Prefixes[i]})
	}
	return s3response.ListVersionsResult{
		Name:           in.Bucket,
		Prefix:         in.Prefix,
		Delimiter:      in.Delimiter,
		KeyMarker:      in.KeyMarker,
		MaxKeys:        &maxKeys,
		IsTruncated:    &truncated,
		NextKeyMarker:  resp.OpcNextPage,
		Versions:       versions,
		DeleteMarkers:  markers,
		CommonPrefixes: prefixes,
	}, nil
}

func (o *OCI) CreateMultipartUpload(ctx context.Context, in s3response.CreateMultipartUploadInput) (s3response.InitiateMultipartUploadResult, error) {
	resp, err := o.client.CreateMultipartUpload(ctx, objectstorage.CreateMultipartUploadRequest{
		NamespaceName: &o.namespace,
		BucketName:    in.Bucket,
		CreateMultipartUploadDetails: objectstorage.CreateMultipartUploadDetails{
			Object:      in.Key,
			ContentType: in.ContentType,
			Metadata:    in.Metadata,
		},
	})
	if err != nil {
		return s3response.InitiateMultipartUploadResult{}, mapError(err)
	}
	return s3response.InitiateMultipartUploadResult{
		Bucket:   deref(in.Bucket),
		Key:      deref(in.Key),
		UploadId: deref(resp.MultipartUpload.UploadId),
	}, nil
}

func (o *OCI) UploadPart(ctx context.Context, in *s3.UploadPartInput) (*s3.UploadPartOutput, error) {
	if in.PartNumber == nil {
		return nil, s3err.GetAPIError(s3err.ErrInvalidPart)
	}
	partNum := int(*in.PartNumber)
	resp, err := o.client.UploadPart(ctx, objectstorage.UploadPartRequest{
		NamespaceName:  &o.namespace,
		BucketName:     in.Bucket,
		ObjectName:     in.Key,
		UploadId:       in.UploadId,
		UploadPartNum:  &partNum,
		ContentLength:  in.ContentLength,
		UploadPartBody: io.NopCloser(in.Body),
	})
	if err != nil {
		return nil, mapError(err)
	}
	return &s3.UploadPartOutput{ETag: ptr(quote(deref(resp.ETag)))}, nil
}

func (o *OCI) CompleteMultipartUpload(ctx context.Context, in *s3.CompleteMultipartUploadInput) (s3response.CompleteMultipartUploadResult, string, error) {
	var parts []objectstorage.CommitMultipartUploadPartDetails
	if in.MultipartUpload != nil {
		for _, p := range in.MultipartUpload.Parts {
			if p.PartNumber == nil {
				continue
			}
			n := int(*p.PartNumber)
			parts = append(parts, objectstorage.CommitMultipartUploadPartDetails{
				PartNum: &n,
				Etag:    ptr(unquote(deref(p.ETag))),
			})
		}
	}
	resp, err := o.client.CommitMultipartUpload(ctx, objectstorage.CommitMultipartUploadRequest{
		NamespaceName: &o.namespace,
		BucketName:    in.Bucket,
		ObjectName:    in.Key,
		UploadId:      in.UploadId,
		CommitMultipartUploadDetails: objectstorage.CommitMultipartUploadDetails{
			PartsToCommit: parts,
		},
	})
	if err != nil {
		return s3response.CompleteMultipartUploadResult{}, "", mapError(err)
	}
	return s3response.CompleteMultipartUploadResult{
		Bucket: in.Bucket,
		Key:    in.Key,
		ETag:   ptr(quote(deref(resp.ETag))),
	}, "", nil
}

func (o *OCI) AbortMultipartUpload(ctx context.Context, in *s3.AbortMultipartUploadInput) error {
	_, err := o.client.AbortMultipartUpload(ctx, objectstorage.AbortMultipartUploadRequest{
		NamespaceName: &o.namespace,
		BucketName:    in.Bucket,
		ObjectName:    in.Key,
		UploadId:      in.UploadId,
	})
	if err != nil && !isNotFound(err) {
		return mapError(err)
	}
	return nil
}

func (o *OCI) ListParts(ctx context.Context, in *s3.ListPartsInput) (s3response.ListPartsResult, error) {
	// Same pagination requirement as ListMultipartUploads: a large layer can
	// exceed one page of parts, and a short list silently corrupts the commit.
	var items []objectstorage.MultipartUploadPartSummary
	var page *string
	for {
		resp, err := o.client.ListMultipartUploadParts(ctx, objectstorage.ListMultipartUploadPartsRequest{
			NamespaceName: &o.namespace,
			BucketName:    in.Bucket,
			ObjectName:    in.Key,
			UploadId:      in.UploadId,
			Page:          page,
		})
		if err != nil {
			return s3response.ListPartsResult{}, mapError(err)
		}
		items = append(items, resp.Items...)
		if resp.OpcNextPage == nil {
			break
		}
		page = resp.OpcNextPage
	}
	parts := make([]s3response.Part, 0, len(items))
	for _, p := range items {
		part := s3response.Part{}
		if p.PartNumber != nil {
			part.PartNumber = *p.PartNumber
		}
		if p.Size != nil {
			part.Size = *p.Size
		}
		if p.Etag != nil {
			part.ETag = quote(*p.Etag)
		}
		parts = append(parts, part)
	}
	return s3response.ListPartsResult{
		Bucket:   deref(in.Bucket),
		Key:      deref(in.Key),
		UploadID: deref(in.UploadId),
		Parts:    parts,
	}, nil
}

// ListMultipartUploads pages through every in-progress upload.
//
// This MUST be exhaustive. OCI offers no server-side prefix filter, and the
// distribution registry resolves an in-progress blob upload by listing and
// matching on path — so returning only the first page makes a fresh upload
// invisible and the registry fails the PUT with
// "error resolving upload: Path not found: .../_uploads/<uuid>/data".
// A bucket with many concurrent pushes will not have the one we want on page 1.
func (o *OCI) ListMultipartUploads(ctx context.Context, in *s3.ListMultipartUploadsInput) (s3response.ListMultipartUploadsResult, error) {
	var uploads []s3response.Upload
	var page *string
	for {
		resp, err := o.client.ListMultipartUploads(ctx, objectstorage.ListMultipartUploadsRequest{
			NamespaceName: &o.namespace,
			BucketName:    in.Bucket,
			Page:          page,
		})
		if err != nil {
			return s3response.ListMultipartUploadsResult{}, mapError(err)
		}
		for _, u := range resp.Items {
			if in.Prefix != nil && *in.Prefix != "" && !strings.HasPrefix(deref(u.Object), *in.Prefix) {
				continue
			}
			up := s3response.Upload{Key: deref(u.Object), UploadID: deref(u.UploadId)}
			if u.TimeCreated != nil {
				up.Initiated = u.TimeCreated.Time
			}
			uploads = append(uploads, up)
		}
		if resp.OpcNextPage == nil {
			break
		}
		page = resp.OpcNextPage
	}
	return s3response.ListMultipartUploadsResult{
		Bucket:  deref(in.Bucket),
		Uploads: uploads,
	}, nil
}

// CopyObject streams source through the gateway.
//
// ponytail: OCI's native CopyObject is asynchronous (work-request based) and
// aimed at cross-region replication, so it does not match S3's synchronous
// copy contract. Streaming get→put is correct and keeps the semantics right;
// it costs bandwidth through the gateway. Revisit with the async API plus
// work-request polling if large server-side copies ever become hot.
func (o *OCI) CopyObject(ctx context.Context, in s3response.CopyObjectInput) (s3response.CopyObjectOutput, error) {
	srcBucket, srcKey, err := splitCopySource(deref(in.CopySource))
	if err != nil {
		return s3response.CopyObjectOutput{}, err
	}
	src, err := o.client.GetObject(ctx, objectstorage.GetObjectRequest{
		NamespaceName: &o.namespace,
		BucketName:    &srcBucket,
		ObjectName:    &srcKey,
	})
	if err != nil {
		return s3response.CopyObjectOutput{}, mapError(err)
	}
	defer src.Content.Close()

	put, err := o.client.PutObject(ctx, objectstorage.PutObjectRequest{
		NamespaceName: &o.namespace,
		BucketName:    in.Bucket,
		ObjectName:    in.Key,
		PutObjectBody: src.Content,
		ContentLength: src.ContentLength,
		ContentType:   src.ContentType,
	})
	if err != nil {
		return s3response.CopyObjectOutput{}, mapError(err)
	}
	now := time.Now().UTC()
	return s3response.CopyObjectOutput{
		CopyObjectResult: &s3response.CopyObjectResult{
			ETag:         ptr(quote(deref(put.ETag))),
			LastModified: &now,
		},
	}, nil
}

// splitCopySource parses S3's `/bucket/key` (or `bucket/key`) CopySource header.
func splitCopySource(src string) (bucket, key string, err error) {
	trimmed := strings.TrimPrefix(src, "/")
	if i := strings.Index(trimmed, "?"); i >= 0 {
		trimmed = trimmed[:i] // drop ?versionId=...
	}
	bucket, key, ok := strings.Cut(trimmed, "/")
	if !ok || bucket == "" || key == "" {
		return "", "", s3err.GetAPIError(s3err.ErrInvalidCopyDest)
	}
	return bucket, key, nil
}

func isNotFound(err error) bool {
	var svcErr common.ServiceError
	return errorAs(err, &svcErr) && svcErr.GetHTTPStatusCode() == 404
}

func quote(s string) string {
	if s == "" || strings.HasPrefix(s, `"`) {
		return s
	}
	return `"` + s + `"`
}

func unquote(s string) string { return strings.Trim(s, `"`) }

// nilIfEmpty drops empty-string pointers so they are omitted from the OCI
// request rather than sent as `param=`, which OCI rejects with 400.
func nilIfEmpty(p *string) *string {
	if p == nil || *p == "" {
		return nil
	}
	return p
}

func ptr[T any](v T) *T { return &v }

func deref[T any](p *T) T {
	if p == nil {
		var zero T
		return zero
	}
	return *p
}
