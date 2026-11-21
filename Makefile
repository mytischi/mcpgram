.PHONY: build test test-race lint clean

BINARY := bin/mcpgram

build:
	go build -o $(BINARY) ./cmd/mcpgram

test:
	go test -v ./...

test-race:
	go test -race -v ./...

clean:
	rm -rf bin/
