.PHONY: build test

VERSION=`git describe --tags --dirty --always`

build:
	go build -ldflags "-X 'github.com/conduitio/conduit-connector-file.version=${VERSION}'" -o conduit-connector-file cmd/connector/main.go

test:
	go test $(GOTEST_FLAGS) -race ./...

