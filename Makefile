BIN ?= bin/agentbox

.PHONY: build test lint generate run install size release-snapshot

build:
	go build -o $(BIN) ./cmd/agentbox

test:
	go test -race ./...

lint:
	golangci-lint run ./...

generate:
	templ generate

run:
	go run ./cmd/agentbox ui

install:
	go install ./cmd/agentbox

size: build
	@du -h $(BIN)

release-snapshot:
	goreleaser release --snapshot --clean
