.PHONY: build test

build:
	go build -o conduit-plugin-file cmd/file/main.go

test:
	go test $(GOTEST_FLAGS) -race ./...

