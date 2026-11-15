.PHONY: run build test vet lint dry-run

GO ?= go

run:
	@$(GO) run ./cmd/main.go serve

build:
	@$(GO) build -o bin/digitalclone ./cmd/main.go

test:
	@$(GO) test -v -race ./...

vet:
	@$(GO) vet ./...

lint:
	@golangci-lint run

dry-run:
	@$(GO) run ./cmd/main.go trigger --repo . --commit HEAD --dry-run
