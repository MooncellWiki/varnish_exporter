GO ?= go
GOLANGCI_LINT ?= golangci-lint
GORELEASER ?= goreleaser
GOVULNCHECK_VERSION ?= v1.9.0
FUZZTIME ?= 10s

.PHONY: build test race coverage vet fmt fmt-check modules-check check lint vuln fuzz integration release-check snapshot clean

build:
	$(GO) build -trimpath -o bin/varnish_exporter .

test:
	$(GO) test -count=1 ./...

race:
	$(GO) test -race -count=1 ./...

coverage:
	$(GO) test -race -count=1 -covermode=atomic -coverprofile=coverage.out ./...
	$(GO) tool cover -func=coverage.out

vet:
	$(GO) vet ./...

fmt:
	gofmt -w .

fmt-check:
	@test -z "$$(gofmt -l .)" || { gofmt -l .; exit 1; }

modules-check:
	$(GO) mod tidy -diff
	$(GO) mod verify

check: fmt-check modules-check vet coverage build

lint:
	$(GOLANGCI_LINT) run ./...

vuln:
	$(GO) run golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION) ./...

fuzz:
	$(GO) test -run='^$$' -fuzz='^FuzzScrapeVarnishFrom$$' -fuzztime=$(FUZZTIME) .
	$(GO) test -run='^$$' -fuzz='^FuzzVarnishVersion$$' -fuzztime=$(FUZZTIME) .

integration:
	CONTINUOUS_INTEGRATION=true $(GO) test -count=1 -run='^Test_VarnishMetrics_CI$$' -v ./...

release-check:
	$(GORELEASER) check

snapshot:
	$(GORELEASER) release --snapshot --clean

clean:
	rm -rf bin dist coverage.out
