# SPDX-License-Identifier: Apache-2.0

BINARY  := liops
VERSION ?= dev
LDFLAGS := -s -w -X github.com/logisticinfotech/liops/app/version.Version=$(VERSION)

.PHONY: build build-linux test lint vuln testenv integration integration-mysql testenv-down dev-up dev dev-down

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

integration:      ## Setup, backups, dumps, emails and restores in the Docker test environment
	./test/integration.sh

integration-mysql: ## The same for MySQL 8.4: setup, dumps, users and grants, restores, emails
	./test/integration-mysql.sh

testenv-down:     ## Stop the test environment and delete its data
	docker compose -f test/compose.yaml --profile mysql down -v

dev-up:           ## Laptop dev environment without Docker: MinIO, Mailpit, dev/config.yaml (docs/development.md)
	./scripts/dev.sh up

dev: build        ## Serve the UI and scheduler against the dev environment
	LIOPS_DEV=1 ./$(BINARY) serve

dev-down:         ## Stop MinIO and Mailpit (dev/ is kept)
	./scripts/dev.sh down
