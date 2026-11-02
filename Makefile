.PHONY: build test vet fmt check

build:
	go build -o bin/server ./cmd/server
	go build -o bin/telegram ./cmd/telegram
	go build -o bin/personal-ai ./cmd/cli

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -l -w .

check:
	@test -z "$$(gofmt -l .)" || (echo "gofmt needed:"; gofmt -l .; exit 1)
	go vet ./...
	go test ./...
