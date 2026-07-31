# versitygw + the OCI Object Storage backend plugin.
#
# Both binaries are built from the SAME module (this repo's go.mod). Go plugins
# require the host binary and the .so to agree on every shared package version;
# building the gateway as a dependency of this module makes that true by
# construction rather than by pinning two checkouts and hoping.
#
# CGO is required: -buildmode=plugin does not work with CGO_ENABLED=0, which is
# also why the runtime is debian-slim rather than a static distroless image.
FROM golang:1.26-bookworm AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . ./
ENV CGO_ENABLED=1
RUN go vet ./... \
 && go test ./... \
 && go build -buildmode=plugin -o /out/oci.so . \
 && go build -o /out/versitygw github.com/versity/versitygw/cmd/versitygw

FROM debian:bookworm-slim

RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates \
 && rm -rf /var/lib/apt/lists/* \
 && useradd --uid 10001 --system --no-create-home --shell /usr/sbin/nologin versitygw

COPY --from=build /out/versitygw /usr/local/bin/versitygw
COPY --from=build /out/oci.so    /usr/local/lib/versitygw/oci.so

USER 10001
EXPOSE 7070
ENTRYPOINT ["/usr/local/bin/versitygw"]
