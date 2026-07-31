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
