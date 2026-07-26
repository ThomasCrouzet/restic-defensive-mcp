VERSION ?= 0.1.0-dev
BINARY  := bin/restic-defensive-mcp
GOFLAGS := -trimpath
LDFLAGS := -s -w -X main.version=$(VERSION)
FUZZTIME ?= 5s
export CGO_ENABLED := 0

.PHONY: all build test race vet fmt lint fuzz-smoke integration demo clean help

all: build test

build:
	mkdir -p bin
	go build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o $(BINARY) ./cmd/restic-defensive-mcp

test:
	go test ./...

race:
	go test -race ./...

vet:
	go vet ./...

fmt:
	gofmt -w $$(find . -name '*.go' -not -path './.git/*')

lint:
	go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.1.6 run ./...

fuzz-smoke:
	go test ./internal/config -run '^$$' -fuzz '^FuzzParse$$' -fuzztime=$(FUZZTIME)
	go test ./internal/policy -run '^$$' -fuzz '^FuzzCleanPath$$' -fuzztime=$(FUZZTIME)
	go test ./internal/redaction -run '^$$' -fuzz '^FuzzSanitize$$' -fuzztime=$(FUZZTIME)
	go test ./internal/restic -run '^$$' -fuzz '^FuzzParseSnapshots$$' -fuzztime=$(FUZZTIME)
	go test ./internal/restic -run '^$$' -fuzz '^FuzzParseLS$$' -fuzztime=$(FUZZTIME)
	go test ./internal/restic -run '^$$' -fuzz '^FuzzMapExit$$' -fuzztime=$(FUZZTIME)

integration:
	go test -tags=integration -count=1 -timeout 5m -v .

demo:
	go test -tags=integration -count=1 -timeout 5m -run TestIntegrationRealRestic -v .

clean:
	rm -f $(BINARY)
	rmdir bin 2>/dev/null || true
	go clean -testcache

help:
	@echo "targets: build test race vet fmt lint fuzz-smoke integration demo clean"
