VERSION ?= dev
LDFLAGS ?= -s -w -X github.com/gjpin/agent-os/internal/cli.Version=$(VERSION)

.PHONY: build test race vet fmt check e2e

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

e2e:
	AGENT_OS_E2E=1 go test -tags=e2e ./e2e -v -timeout 1h
