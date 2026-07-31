SHELL := /bin/bash
GOFILES := $(shell find . -name '*.go' -not -path './vendor/*')
ACTIONLINT := $(shell command -v actionlint)

.PHONY: fmt vet test plugin image lint-workflows validate tools

fmt:
	gofmt -w $(GOFILES)

vet:
	go vet ./...

test:
	go test ./...

# Sanity-check that the backend still satisfies backend.Backend and links as a
# plugin. Requires CGO; -buildmode=plugin does not work with CGO_ENABLED=0.
plugin:
	CGO_ENABLED=1 go build -buildmode=plugin -o out/oci.so .

image:
	docker build -t versitygw-oci:dev .

lint-workflows:
ifndef ACTIONLINT
	$(error actionlint not found. Install with: brew install actionlint || go install github.com/rhysd/actionlint/cmd/actionlint@latest)
endif
	$(ACTIONLINT)

validate: fmt vet test plugin lint-workflows

tools:
	@echo "actionlint: $(ACTIONLINT)"
