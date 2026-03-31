.PHONY: build run openapi clean test test-integration test-all

BINARY   = voiceblender
ENV_FILE = voiceblender.env

build:
	go build -o $(BINARY) ./cmd/voiceblender

run: build
	env $$(cat $(ENV_FILE) | grep -v '^\s*\#' | xargs) ./$(BINARY)

openapi:
	go generate ./internal/api/

test:
	go test ./internal/... -count=1 -timeout=60s

test-integration:
	go test -tags integration -v -timeout 60s -skip TestConcurrentRoomsScale ./tests/integration/

test-benchmark:
	go test -tags integration -v -timeout 120s -run TestConcurrentRoomsScale ./tests/integration/

test-all: test test-integration test-benchmark

clean:
	rm -f $(BINARY) openapi-gen
