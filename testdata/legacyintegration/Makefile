.PHONY: check test-race
check:
	test -z "$$(gofmt -l .)"
	go mod tidy -diff
	go test -count=1 ./...
test-race:
	go test -race -count=1 ./...
