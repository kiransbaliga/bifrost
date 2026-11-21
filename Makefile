.PHONY: all build test clean run

BINARY_NAME=bifrost
CMD_DIR=./cmd/bifrost

all: test build

build:
	CGO_ENABLED=0 go build -ldflags="-s -w" -o $(BINARY_NAME) $(CMD_DIR)

build-linux-amd64:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o $(BINARY_NAME)-linux-amd64 $(CMD_DIR)

build-linux-arm64:
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -ldflags="-s -w" -o $(BINARY_NAME)-linux-arm64 $(CMD_DIR)

test:
	go test -v -race ./...

lint:
	go vet ./...

clean:
	rm -f $(BINARY_NAME) $(BINARY_NAME)-linux-amd64 $(BINARY_NAME)-linux-arm64

run: build
	./$(BINARY_NAME) -port 8080
