# versitygw-oci

An [OCI Object Storage](https://docs.oracle.com/en-us/iaas/Content/Object/home.htm) backend for
[versitygw](https://github.com/versity/versitygw), so S3-only clients can reach OCI buckets
**without a long-lived credential**.

## Why

OCI Object Storage has two front doors:

| Endpoint | Auth | Workload identity? |
|---|---|---|
| `<ns>.compat.objectstorage.<region>.oraclecloud.com` (S3-compat) | SigV4 with a Customer Secret Key | **no** |
| `objectstorage.<region>.oraclecloud.com` (native) | OCI request signing | **yes** |

A Customer Secret Key can only be issued against an **IAM user**. So anything that speaks S3 to
OCI needs a user and a static key — which is exactly what most compliance regimes are trying to
get rid of.

The native API has no such limitation: it accepts OKE workload identity, instance principals and
resource principals. This plugin puts versitygw in between. Clients keep speaking S3; the gateway
talks native OCI and holds no secret.

The constraint is the **S3 protocol shim, not OCI and not Kubernetes**.

## Usage

The published image bundles versitygw and the plugin, built from one module:

```
ghcr.io/adrianmross/versitygw-oci:<tag>
```

```sh
versitygw --region <oci-region> --port 0.0.0.0:7070 \
  plugin --config /etc/versitygw/oci.conf /usr/local/lib/versitygw/oci.so
```

`--config` takes a **file path**, and urfave/cli wants flags *before* the positional plugin path.

Config file — `key=value`, comma or whitespace separated:

```
namespace=<object-storage-namespace>
region=<oci-region>
auth=workload_identity
compartment=<compartment-ocid>   # only for ListBuckets
```

| Key | Values | Notes |
|---|---|---|
| `namespace` | Object Storage namespace | Resolved automatically via `GetNamespace` if omitted |
| `region` | e.g. `us-phoenix-1` | Falls back to `OCI_REGION` |
| `auth` | `workload_identity`, `instance_principal`, `default` | Empty tries workload identity, then the default OCI chain |
| `compartment` | Compartment OCID | Only needed for `ListBuckets`. Falls back to `OCI_COMPARTMENT_ID` |
| `bucketOwners` | `bucket:account\|bucket:account` | Per-bucket ownership. Unmapped buckets stay root-owned |

Throttles and transient 5xx are retried with exponential backoff; the OCI SDK applies no retry
of its own. The one namespace lookup at startup is bounded at 15s so an unreachable OCI fails
the gateway fast instead of hanging plugin load.

### Per-account access

By default every bucket is owned by the root account, so one credential reaches everything the
gateway fronts. To separate consumers, give versitygw an account list (`--iam-dir` with a
`users.json`) and map buckets to accounts:

```
bucketOwners=registry:harbor-acct|docs:techdocs-acct
```

`GetBucketAcl` then reports that owner, and versitygw's `verifyACL` denies any other account —
so a credential leaked from one consumer cannot reach another's bucket. **Unmapped buckets keep
the previous behaviour**, so mapping is opt-in and cannot break a consumer you have not migrated.

### Region matters more than it looks

versitygw verifies the SigV4 credential scope against its own `--region`. Set it to the region your
clients already sign with, or every request fails the signature check. The default is `us-east-1`.

## Building

```sh
make validate     # fmt, vet, test, plugin build, actionlint
make image
```

Go plugins require the host binary and the `.so` to agree on **every shared package version**. The
Dockerfile therefore builds `cmd/versitygw` *as a dependency of this module*, which makes that true
by construction. It is also why `go.mod` carries a dependency closure nothing here imports — see the
note at the top of that file before running `go mod tidy`.

CGO is required; `-buildmode=plugin` does not work with `CGO_ENABLED=0`.

## Traps

Implementation gotchas that cost real debugging time — multipart pagination, the three
backend methods that are mandatory even though nothing calls them, empty query parameters,
and the Go SDK's resource-principal environment — are in **[docs/traps.md](docs/traps.md)**.

## Not implemented

Object tagging, ACL writes, versioning, object lock, bucket create/delete, and website/CORS config
fall through to `BackendUnsupported` and return `NotImplemented`. That is deliberate — an honest 501
beats a silent wrong answer.

Versioning in particular is **not** passthrough: buckets with versioning enabled keep versioning
server-side in OCI, but this backend exposes no version-aware operations.

`CopyObject` streams source through the gateway. OCI's native copy is asynchronous and
work-request based, so it does not match S3's synchronous copy contract; streaming keeps the
semantics correct at the cost of bandwidth.

