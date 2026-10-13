# SPDX-License-Identifier: Apache-2.0

BINARY  := pgtijori
VERSION ?= dev
LDFLAGS := -s -w -X github.com/logisticinfotech/pgtijori/app/version.Version=$(VERSION)

.PHONY: build build-linux test lint vuln testenv testenv-down

build:            ## Build for this machine
	go build -trimpath -ldflags "$(LDFLAGS)" -o $(BINARY) .

build-linux:      ## Build the static linux/amd64 release binary into dist/
	mkdir -p dist
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "$(LDFLAGS)" -o dist/$(BINARY)-linux-amd64 .

test:             ## Unit tests
	go test -race -count=1 ./...

lint:             ## gofmt + go vet
	@test -z "$$(gofmt -l .)" || (echo "Not gofmt'ed:"; gofmt -l .; exit 1)
	go vet ./...

vuln:             ## Check dependencies for known vulnerabilities
	go run golang.org/x/vuln/cmd/govulncheck@latest ./...

testenv:          ## Start the Docker test environment and run the smoke test
	./test/smoke.sh

testenv-down:     ## Stop the test environment and delete its data
	docker compose -f test/compose.yaml down -v
