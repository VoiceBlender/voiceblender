.PHONY: build run openapi clean

BINARY   = voiceblender
ENV_FILE = voiceblender.env

build:
	go build -o $(BINARY) ./cmd/voiceblender

run: build
	env $$(cat $(ENV_FILE) | grep -v '^\s*\#' | xargs) ./$(BINARY)

openapi:
	go generate ./internal/api/

clean:
	rm -f $(BINARY) openapi-gen
