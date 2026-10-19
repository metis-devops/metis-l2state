.PHONY: build test test-race test-race-root lint fmt-check fixture-check ci ci-root
build:
	go build -trimpath -o bin/ ./cmd/...
test:
	go test ./...
	go test ./testdata/dumpprofile
test-race-root:
	go test -race ./...
test-race: test-race-root
	$(MAKE) -C testdata/legacyintegration test-race
lint:
	golangci-lint run
fmt-check:
	test -z "$$(gofmt -l cmd internal statefile testdata/readercheck testdata/verifyfile testdata/dumpprofile)"
	go mod tidy -diff
fixture-check:
	$(MAKE) -C testdata/legacyintegration check
ci-root: fmt-check lint test build
ci: ci-root fixture-check
