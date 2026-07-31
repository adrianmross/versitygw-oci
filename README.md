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
```

| Key | Values | Notes |
|---|---|---|
| `namespace` | Object Storage namespace | Resolved automatically via `GetNamespace` if omitted |
| `region` | e.g. `us-phoenix-1` | Falls back to `OCI_REGION` |
| `auth` | `workload_identity`, `instance_principal`, `default` | Empty tries workload identity, then the default OCI chain |

`OCI_COMPARTMENT_ID` is required only for `ListBuckets`.

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

