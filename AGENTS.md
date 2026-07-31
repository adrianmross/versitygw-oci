# AGENTS

Operational guide for maintaining the `versitygw-oci` backend plugin.

## Scope
- Keep instructions generic and environment-agnostic.
- Do not include personal hostnames, usernames, tenancy OCIDs, bucket names, or
  absolute machine-specific paths. Test fixtures use placeholder values
  (`examplens`, `us-phoenix-1`) on purpose — keep them that way.

## Build model
- The plugin and the versitygw binary MUST come from the same module graph and
  toolchain, or the runtime symbol load fails. The Dockerfile builds
  `cmd/versitygw` as a dependency of this module to guarantee that.
- `go.mod` therefore carries a dependency closure that no package here imports.
  `go mod tidy` prunes it and breaks the image build with "missing go.sum entry".
  Re-add with `go get github.com/versity/versitygw/cmd/versitygw@<version>`.
- CGO is required. `-buildmode=plugin` does not work with `CGO_ENABLED=0`, which
  is why the runtime image is debian-slim and not distroless-static.

## Before changing backend methods
- `var _ backend.Backend = (*OCI)(nil)` fails the build if a versitygw bump
  changes the interface. Keep it.
- `GetBucketAcl`, `GetObjectLockConfiguration` and `GetBucketVersioning` are
  middleware dependencies on every route or every write path. Removing them
  makes the gateway return 501 to everything. `GetObjectLockConfiguration` must
  return `ErrObjectLockConfigurationNotFound` — that error means "allowed".
- Any OCI list call that backs a lookup must paginate exhaustively. OCI has no
  server-side prefix filter for `ListMultipartUploads`, and short-listing
  `ListParts` silently corrupts large objects.

## Testing
- `make validate` runs fmt, vet, test, a real `-buildmode=plugin` build, and
  actionlint.
- Unit tests cover pure logic only (config parsing, key handling, error mapping,
  listing conversion). They deliberately do not hit OCI.
- **SDK-level tests are not sufficient before a release.** They pass an uploadId
  directly and never exercise resolve-by-listing, which is where the multipart
  bugs live. Validate against a real S3 client that performs a chunked upload.

## Releasing
- CI runs on push and PR to `main`, `develop`, `release/**`.
- Tagging `v*` publishes `ghcr.io/<owner>/versitygw-oci:<tag>` AND creates a GitHub
  release pinning the image digest. Tags must be semver (`v1.2.3`, `v1.2.3-rc.1`).
- Implementation gotchas belong in `docs/traps.md`, not the README.
- **Never re-push an existing tag.** Consumers that pin a tag with
  `imagePullPolicy: IfNotPresent` will not pick it up, and if the pod template is
  unchanged no rollout happens at all — the result is a fix that is merged, built,
  green, and not running. Verify a rollout by comparing image digests, not tags.
