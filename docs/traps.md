# Traps

Implementation gotchas in this backend. Each cost real debugging time, and none are obvious
from the symptom.

Each of these cost real debugging time and none are obvious from the symptom.

**Multipart list calls must paginate exhaustively.** OCI has no server-side prefix filter for
`ListMultipartUploads`, and the `distribution` registry resolves an in-progress blob upload by
listing and matching on path. Returning one page makes a fresh upload invisible and a push fails
with `error resolving upload: Path not found: .../_uploads/<uuid>/data`. Reads are unaffected, so it
presents as a write-permission problem and is not. An SDK-level test will not catch it either —
`boto3` passes the uploadId straight through and never needs the lookup. Only a real registry push
exercises it. `ListParts` has the same requirement, and short-listing there silently corrupts large
objects rather than failing.

**Three backend methods are mandatory even though nothing calls them directly.** Leave any to
`BackendUnsupported` and the gateway answers **501 to every request**, long before your controller
runs — which looks like the plugin failing to load and is not:

- `GetBucketAcl` — `ParseAcl` / `AuthorizePublicBucketAccess` are middleware on *every* route.
- `GetObjectLockConfiguration` — `auth.CheckObjectAccess` runs on every write path. It must return
  `ErrObjectLockConfigurationNotFound`; that specific error means "allowed", any other is fatal.
- `GetBucketVersioning` — same write path.

**OCI rejects empty query parameters and empty Range headers.** versitygw passes non-nil empty
strings for absent `Prefix`/`Delimiter`/`Start` and for a whole-object `Range`. Forwarding those
yields `400 InvalidObjectName` ("The delimiter parameter '' is not supported") and `416
InvalidRange`, surfacing to clients as a confusing 500 on a plain `ListObjectsV2` or an ordinary
`GetObject`.

**Go's OCI SDK does not discover the resource-principal environment.**
`OkeWorkloadIdentityConfigurationProvider` reads `OCI_RESOURCE_PRINCIPAL_VERSION` (must be `2.2`)
and `OCI_RESOURCE_PRINCIPAL_REGION` from the environment and fails hard without them. The Python
SDK *does* discover them, so a workload-identity call that works from a Python pod will crash-loop
a Go one.

**Abandoned multipart uploads block bucket deletion.** Failed `CompleteMultipartUpload` calls leave
pending uploads that make a bucket look empty while `DeleteBucket` fails with `BucketNotEmpty`.
Give fronted buckets an abort-incomplete-multipart lifecycle rule.

**Newer AWS SDKs send CRC32 trailers by default.** If a client fails with `NotImplemented` on
upload, set `request_checksum_calculation=when_required` (or the equivalent for your SDK).

**The SDK's default HTTP client kills slow reads at 60 seconds.**
`objectstorage.NewObjectStorageClientWithConfigurationProvider` returns a client whose
`http.Client` carries an absolute `Timeout: 60s`, and that deadline covers the response *body*.
A `GetObject` therefore dies mid-stream whenever the consumer reads slower than
`size / 60s` — the failure is **time-based, not size-based**, so it moves around:

```
3.86 GB object, consumed at 110 MB/s   COMPLETE 3855521197 bytes in 35.1s
3.86 GB object, consumed at  33 MB/s   FAILED after 2.07 GB in 62.4s
3.86 GB object, consumed at  26 MB/s   FAILED after 1.62 GB in 62.5s
```

Writes never hit it: multipart splits an upload into part requests that each finish well inside
60s. Reads are a single request, so only a large object with a slow consumer trips it — which is
exactly a PostgreSQL base-backup restore, where `barman-cloud-restore` decompresses and untars
as it reads. The symptom is `Connection broken: IncompleteRead(N bytes read, M more expected)`
in the *client*, with nothing at all in the gateway logs, so it reads like a network fault.

It also means a backup can be written, verified present, and be completely unrestorable. Fixed
by replacing the dispatcher with one that has no absolute timeout and keeps per-phase bounds
(`streamingHTTPClient`).

**A non-root account gets 501 on everything unless `GetBucketPolicy` is implemented.**
`auth.VerifyAccess` returns early for root and for `RoleAdmin`, so single-account mode never
reaches the rest of the function. Any other account does, and the first thing it hits is
`be.GetBucketPolicy` — whose error is fatal unless it is exactly `ErrNoSuchBucketPolicy`.
Left to `BackendUnsupported` that is `ErrNotImplemented`, so every request from a per-consumer
account fails with 501 and `GetBucketAcl` is never consulted. The symptom points at bucket
ownership, which is correct and simply unread.

This is the same shape as `GetBucketAcl` and `GetObjectLockConfiguration`: a middleware
dependency whose omission fails the route rather than the feature. It hid longer only because
it is unreachable until a second account exists.

**Per-bucket ownership does nothing without `--disable-acl`.** `verifyACL` compares
`acl.Owner` to the requesting access key *only* when ACLs are disabled. With ACLs enabled it
matches grantees instead, and an owner-only ACL has none — so the owner is denied along with
everyone else. Test the authorization chain through `auth.VerifyAccess`, not by asserting on
`GetBucketAcl`'s output: the ACL can be perfectly correct while every request 501s or 403s.
