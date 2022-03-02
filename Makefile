.PHONY: build test

build:
	go build -o conduit-connector-file cmd/file/main.go

test:
	go test $(GOTEST_FLAGS) -race ./...

