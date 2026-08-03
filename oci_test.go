package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/oracle/oci-go-sdk/v65/common"
	"github.com/oracle/oci-go-sdk/v65/objectstorage"
	vgwauth "github.com/versity/versitygw/auth"
	"github.com/versity/versitygw/s3err"
)

func TestParseConfig(t *testing.T) {
	got := parseConfig("namespace=examplens, region=us-phoenix-1 auth=workload_identity")
	for k, want := range map[string]string{
		"namespace": "examplens",
		"region":    "us-phoenix-1",
		"auth":      "workload_identity",
	} {
		if got[k] != want {
			t.Errorf("parseConfig()[%q] = %q, want %q", k, got[k], want)
		}
	}
	if len(parseConfig("")) != 0 {
		t.Error("empty config should parse to an empty map")
	}
	// A bare token with no '=' must be skipped, not panic or half-parse.
	if len(parseConfig("garbage")) != 0 {
		t.Error("token without '=' should be ignored")
	}
}

func TestSplitCopySource(t *testing.T) {
	tests := []struct {
		in          string
		bucket, key string
		wantErr     bool
	}{
		{in: "/bucket/some/deep/key.txt", bucket: "bucket", key: "some/deep/key.txt"},
		{in: "bucket/key", bucket: "bucket", key: "key"},
		{in: "/bucket/key?versionId=abc", bucket: "bucket", key: "key"},
		{in: "/onlybucket", wantErr: true},
		{in: "", wantErr: true},
		{in: "/", wantErr: true},
	}
	for _, tc := range tests {
		b, k, err := splitCopySource(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("splitCopySource(%q) = (%q,%q), want error", tc.in, b, k)
			}
			continue
		}
		if err != nil {
			t.Errorf("splitCopySource(%q) unexpected error: %v", tc.in, err)
			continue
		}
		if b != tc.bucket || k != tc.key {
			t.Errorf("splitCopySource(%q) = (%q,%q), want (%q,%q)", tc.in, b, k, tc.bucket, tc.key)
		}
	}
}

func TestQuoteRoundTrip(t *testing.T) {
	// S3 clients expect quoted ETags; OCI returns them bare. Double-quoting
	// breaks If-Match, and committing a quoted ETag back to OCI fails the part.
	if got := quote("abc"); got != `"abc"` {
		t.Errorf("quote(abc) = %s", got)
	}
	if got := quote(`"abc"`); got != `"abc"` {
		t.Errorf("quote should not double-quote, got %s", got)
	}
	if got := quote(""); got != "" {
		t.Errorf("quote(empty) = %q, want empty", got)
	}
	if got := unquote(`"abc"`); got != "abc" {
		t.Errorf("unquote = %s", got)
	}
}

func TestConvertListing(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	size := int64(42)
	etag := "deadbeef"
	name := "a/b.txt"
	resp := objectstorage.ListObjectsResponse{
		ListObjects: objectstorage.ListObjects{
			Objects: []objectstorage.ObjectSummary{
				{Name: &name, Size: &size, Etag: &etag, TimeModified: &common.SDKTime{Time: now}},
			},
			Prefixes: []string{"a/", "c/"},
		},
	}
	contents, prefixes := convertListing(resp)
	if len(contents) != 1 {
		t.Fatalf("got %d contents, want 1", len(contents))
	}
	if *contents[0].Key != name || *contents[0].Size != size {
		t.Errorf("object not mapped: %+v", contents[0])
	}
	if *contents[0].ETag != `"deadbeef"` {
		t.Errorf("ETag = %s, want quoted", *contents[0].ETag)
	}
	if !contents[0].LastModified.Equal(now) {
		t.Errorf("LastModified = %v, want %v", contents[0].LastModified, now)
	}
	if len(prefixes) != 2 || *prefixes[0].Prefix != "a/" || *prefixes[1].Prefix != "c/" {
		t.Errorf("common prefixes not mapped: %+v", prefixes)
	}
	// Aliasing bug guard: taking &resp.Prefixes[i] is required, not &loopVar.
	if prefixes[0].Prefix == prefixes[1].Prefix {
		t.Error("common prefixes alias the same pointer")
	}
}

type fakeSvcErr struct {
	status int
	code   string
}

func (f fakeSvcErr) Error() string           { return f.code }
func (f fakeSvcErr) GetHTTPStatusCode() int  { return f.status }
func (f fakeSvcErr) GetMessage() string      { return f.code }
func (f fakeSvcErr) GetCode() string         { return f.code }
func (f fakeSvcErr) GetOpcRequestID() string { return "" }

func TestMapError(t *testing.T) {
	var _ common.ServiceError = fakeSvcErr{}

	if mapError(nil) != nil {
		t.Error("nil should map to nil")
	}

	// A missing bucket and a missing key are both 404 from OCI; clients must be
	// able to tell them apart or a registry's blob upload path misbehaves.
	bucketErr := mapError(fakeSvcErr{status: 404, code: "BucketNotFound"})
	if bucketErr != s3err.GetAPIError(s3err.ErrNoSuchBucket) {
		t.Errorf("BucketNotFound mapped to %v", bucketErr)
	}
	keyErr := mapError(fakeSvcErr{status: 404, code: "ObjectNotFound"})
	if keyErr != s3err.GetAPIError(s3err.ErrNoSuchKey) {
		t.Errorf("ObjectNotFound mapped to %v", keyErr)
	}
	if denied := mapError(fakeSvcErr{status: 403, code: "NotAuthenticated"}); denied != s3err.GetAPIError(s3err.ErrAccessDenied) {
		t.Errorf("403 mapped to %v", denied)
	}

	// Anything unrecognised must pass through rather than be flattened into a
	// misleading S3 code.
	orig := fakeSvcErr{status: 500, code: "InternalServerError"}
	if got := mapError(orig); got != error(orig) {
		t.Errorf("unmapped error should pass through, got %v", got)
	}
}

func TestIsNotFound(t *testing.T) {
	if !isNotFound(fakeSvcErr{status: 404, code: "ObjectNotFound"}) {
		t.Error("404 should be not-found")
	}
	if isNotFound(fakeSvcErr{status: 500, code: "boom"}) {
		t.Error("500 is not not-found")
	}
}

func TestParseConfigFromFile(t *testing.T) {
	// versitygw passes `--config` as a file path, so a path must be read rather
	// than parsed as if it were the config itself.
	dir := t.TempDir()
	path := dir + "/plugin.conf"
	if err := os.WriteFile(path, []byte("namespace=examplens\nregion=us-phoenix-1\nauth=workload_identity\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got := parseConfig(path)
	if got["namespace"] != "examplens" || got["region"] != "us-phoenix-1" || got["auth"] != "workload_identity" {
		t.Errorf("config file not parsed: %+v", got)
	}
}

func TestNilIfEmpty(t *testing.T) {
	// Regression: versitygw hands the backend non-nil empty strings for absent
	// Prefix/Delimiter/Start and for a whole-object Range. OCI rejects those as
	// `param=` with 400 InvalidObjectName ("The delimiter parameter '' is not
	// supported") and 416 InvalidRange, which surface to clients as a 500 on
	// plain ListObjectsV2 and GetObject. Both were caught only in live e2e.
	if nilIfEmpty(nil) != nil {
		t.Error("nil should stay nil")
	}
	empty := ""
	if nilIfEmpty(&empty) != nil {
		t.Error("empty string must be dropped, not sent as param=")
	}
	val := "a/"
	if got := nilIfEmpty(&val); got == nil || *got != "a/" {
		t.Error("non-empty value must pass through")
	}
}

func TestConvertVersions(t *testing.T) {
	// Found only after the first consumer cutover: the obsidian exporter issues
	// GET /bucket?versions for storage accounting and reported scrape_success=0
	// while ListObjectVersions fell through to BackendUnsupported.
	now := time.Now().UTC().Truncate(time.Second)
	tru, fls := true, false
	k1, k2 := "a.txt", "b.txt"
	v1, v2, v3 := "v1", "v2", "vd"
	s1, s2 := int64(10), int64(20)
	items := []objectstorage.ObjectVersionSummary{
		{Name: &k1, VersionId: &v1, Size: &s1, IsDeleteMarker: &fls, TimeModified: &common.SDKTime{Time: now}},
		{Name: &k1, VersionId: &v2, Size: &s2, IsDeleteMarker: &fls, TimeModified: &common.SDKTime{Time: now}},
		{Name: &k2, VersionId: &v3, IsDeleteMarker: &tru, TimeModified: &common.SDKTime{Time: now}},
	}
	versions, markers := convertVersions(items)

	if len(versions) != 2 || len(markers) != 1 {
		t.Fatalf("split wrong: %d versions, %d markers", len(versions), len(markers))
	}
	// Sizes must survive: the exporter sums them, and a nil here silently
	// reports a bucket as 0 bytes rather than failing loudly.
	if *versions[0].Size != 10 || *versions[1].Size != 20 {
		t.Errorf("sizes not mapped: %+v", versions)
	}
	// First sighting of a key is latest; the second version of the same key is not.
	if !*versions[0].IsLatest {
		t.Error("first version of a key should be IsLatest")
	}
	if *versions[1].IsLatest {
		t.Error("second version of the same key must not be IsLatest")
	}
	if markers[0].Key == nil || *markers[0].Key != k2 {
		t.Errorf("delete marker not mapped: %+v", markers[0])
	}
	if vs, ms := convertVersions(nil); len(vs) != 0 || len(ms) != 0 {
		t.Error("nil input should produce no versions or markers")
	}
}

// The SDK wraps service errors on some paths (notably after a retry). A bare
// type assertion misses those and collapses them into InternalError, which is
// exactly the failure mapError exists to prevent. This is the regression test
// for that: it passes with errors.As and fails with a type assertion.
func TestMapErrorUnwrapsWrapped(t *testing.T) {
	wrapped := fmt.Errorf("after 3 retries: %w", fakeSvcErr{status: 404, code: "BucketNotFound"})
	if got := mapError(wrapped); got != s3err.GetAPIError(s3err.ErrNoSuchBucket) {
		t.Errorf("wrapped BucketNotFound mapped to %v, want NoSuchBucket", got)
	}

	deep := fmt.Errorf("outer: %w", fmt.Errorf("inner: %w", fakeSvcErr{status: 403, code: "NotAuthenticated"}))
	if got := mapError(deep); got != s3err.GetAPIError(s3err.ErrAccessDenied) {
		t.Errorf("doubly wrapped 403 mapped to %v, want AccessDenied", got)
	}

	if !isNotFound(fmt.Errorf("wrapped: %w", fakeSvcErr{status: 404, code: "ObjectNotFound"})) {
		t.Error("isNotFound should see through a wrapped error")
	}
}

// fakeSvcErr must satisfy the SDK interface by VALUE for errors.As to bind it.
func TestFakeSvcErrSatisfiesServiceError(t *testing.T) {
	var target common.ServiceError
	if !errors.As(error(fakeSvcErr{status: 500, code: "x"}), &target) {
		t.Fatal("errors.As could not bind fakeSvcErr — the other tests would be vacuous")
	}
}

// A slow reader of a large object must not be killed part-way through.
//
// This is the bug that made Postgres base backups unrestorable: the SDK's
// default dispatcher sets an absolute http.Client.Timeout covering the response
// body, so a GetObject died at ~62s regardless of how much was left. It is
// time-based, not size-based — the same object completed when read fast and
// broke at a different offset each time when read slowly.
//
// The server here trickles its body for longer than the deadline given to the
// "default-like" client, so a client with an absolute timeout must fail and
// streamingHTTPClient must not.
func TestStreamingHTTPClientSurvivesASlowBody(t *testing.T) {
	const chunks = 12
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		for i := 0; i < chunks; i++ {
			_, _ = w.Write([]byte("payload"))
			w.(http.Flusher).Flush()
			time.Sleep(50 * time.Millisecond)
		}
	}))
	defer srv.Close()

	read := func(c *http.Client) (int, error) {
		resp, err := c.Get(srv.URL)
		if err != nil {
			return 0, err
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		return len(b), err
	}

	// Stand-in for the SDK default: an absolute timeout shorter than the body.
	if _, err := read(&http.Client{Timeout: 200 * time.Millisecond}); err == nil {
		t.Fatal("a client with an absolute timeout should have failed on a slow body; " +
			"if this stops failing the test no longer proves anything")
	}

	n, err := read(streamingHTTPClient())
	if err != nil {
		t.Fatalf("streamingHTTPClient failed on a slow body: %v", err)
	}
	if want := chunks * len("payload"); n != want {
		t.Errorf("read %d bytes, want %d", n, want)
	}
}

func TestStreamingHTTPClientKeepsPerPhaseTimeouts(t *testing.T) {
	c := streamingHTTPClient()
	if c.Timeout != 0 {
		t.Errorf("Timeout = %v, want 0 — an absolute deadline breaks large reads", c.Timeout)
	}
	tr, ok := c.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("Transport is %T, want *http.Transport", c.Transport)
	}
	// Dropping the absolute timeout is only safe because these still bound a
	// hung peer. Losing them would turn a dead connection into a hang forever.
	if tr.ResponseHeaderTimeout == 0 || tr.TLSHandshakeTimeout == 0 {
		t.Error("per-phase timeouts must stay set when the absolute timeout is removed")
	}
}

func TestParseBucketOwners(t *testing.T) {
	got := parseBucketOwners("bucket-a:acct1 | bucket-b:acct2|  :skipme |nope|bucket-c:")
	want := map[string]string{"bucket-a": "acct1", "bucket-b": "acct2"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("owner[%q] = %q, want %q", k, got[k], v)
		}
	}
	if len(parseBucketOwners("")) != 0 {
		t.Error("empty config should map nothing")
	}
}

// This is the whole point of per-account credentials: a mapped bucket reports
// its owner, so versitygw's verifyACL denies any other account. An unmapped
// bucket stays root-owned, which is the previous single-account behaviour — so
// adding a mapping is opt-in and cannot break an unmapped consumer.
func TestGetBucketAclOwnership(t *testing.T) {
	o := &OCI{bucketOwners: map[string]string{"registry": "harbor-acct"}}

	b, err := o.GetBucketAcl(context.Background(), &s3.GetBucketAclInput{Bucket: ptr("registry")})
	if err != nil {
		t.Fatal(err)
	}
	var mapped vgwauth.ACL
	if err := json.Unmarshal(b, &mapped); err != nil {
		t.Fatal(err)
	}
	if mapped.Owner != "harbor-acct" {
		t.Errorf("mapped bucket owner = %q, want harbor-acct", mapped.Owner)
	}

	b, err = o.GetBucketAcl(context.Background(), &s3.GetBucketAclInput{Bucket: ptr("unmapped")})
	if err != nil {
		t.Fatal(err)
	}
	var unmapped vgwauth.ACL
	if err := json.Unmarshal(b, &unmapped); err != nil {
		t.Fatal(err)
	}
	if unmapped.Owner != "" {
		t.Errorf("unmapped bucket owner = %q, want empty (root)", unmapped.Owner)
	}

	// Must not panic on the nil input versitygw middleware can pass.
	if _, err := o.GetBucketAcl(context.Background(), nil); err != nil {
		t.Errorf("nil input: %v", err)
	}
}

// Drives the REAL versitygw authorization chain rather than GetBucketAcl alone.
// Asserting on the ACL in isolation is what let an unimplemented
// GetBucketPolicy ship: VerifyAccess consults the policy first and fails the
// request outright on any error but ErrNoSuchBucketPolicy, so ownership was
// correct and unreachable. Anything that breaks the chain — a missing method, a
// changed short-circuit — fails here, which is the only assertion that matches
// what a client actually experiences.
//
// Note versitygw must run with `--disable-acl`. An owner-only ACL carries no
// grantees, and with ACLs enabled verifyACL matches grantees and denies even the
// owner; --disable-acl selects the `acl.Owner != access` comparison instead.
func TestVerifyAccessSeparatesAccounts(t *testing.T) {
	be := &OCI{bucketOwners: map[string]string{"owned": "acct-a"}}
	ctx := context.Background()

	access := func(bucket, account string, disableACL bool) error {
		aclBytes, err := be.GetBucketAcl(ctx, &s3.GetBucketAclInput{Bucket: ptr(bucket)})
		if err != nil {
			t.Fatal(err)
		}
		acl, err := vgwauth.ParseACL(aclBytes)
		if err != nil {
			t.Fatal(err)
		}
		return vgwauth.VerifyAccess(ctx, be, vgwauth.AccessOptions{
			Acl:           acl,
			AclPermission: vgwauth.PermissionRead,
			Acc:           vgwauth.Account{Access: account, Role: vgwauth.RoleUser},
			Bucket:        bucket,
			Actions:       []vgwauth.Action{vgwauth.GetObjectAction},
			DisableACL:    disableACL,
		})
	}

	if err := access("owned", "acct-a", true); err != nil {
		t.Errorf("owner denied on its own bucket: %v", err)
	}

	denied := s3err.GetAPIError(s3err.ErrAccessDenied)
	// The assertion the whole feature exists for.
	if err := access("owned", "acct-b", true); err != denied {
		t.Errorf("non-owner on a mapped bucket = %v, want AccessDenied", err)
	}
	// Unmapped buckets are root-owned, so a named account reaches nothing —
	// this is what keeps un-migrated consumers on the root credential.
	if err := access("unmapped", "acct-a", true); err != denied {
		t.Errorf("named account on an unmapped bucket = %v, want AccessDenied", err)
	}
}
