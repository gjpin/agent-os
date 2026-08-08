VERSION ?= dev
LDFLAGS ?= -s -w -X github.com/gjpin/agent-os/internal/cli.Version=$(VERSION)

.PHONY: build test race vet fmt check

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/agent-os .

test:
	go test ./...

race:
	go test -race ./...

vet:
	go vet ./...

fmt:
	gofmt -w main.go internal

check: fmt test vet
