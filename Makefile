.PHONY: build run openapi asyncapi specs clean vet test test-integration test-benchmark test-all download-greetings gen-human-greetings gen-noisy-greetings docker docker-push

BINARY   = voiceblender
ENV_FILE = voiceblender.env
IMAGE    = voiceblender
TAG      = $(shell git describe --tags --always)

build:
	go build -o $(BINARY) ./cmd/voiceblender

run: build
	env $$(cat $(ENV_FILE) | grep -v '^\s*\#' | xargs) ./$(BINARY)

# Regenerates both openapi.yaml and asyncapi.yaml via the go:generate
# directives in internal/api/server.go. Run after any change to the API
# routes, request/response types, VSI command registry, or event types.
openapi asyncapi specs:
	go generate ./internal/api/

vet:
	go vet ./...

test: vet
	go test ./internal/... -count=1 -timeout=60s

test-integration:
	go test -tags integration -v -timeout 10m -skip TestConcurrentRoomsScale ./tests/integration/

test-benchmark:
	go test -tags integration -v -timeout 300s -run TestConcurrentRoomsScale ./tests/integration/

test-all: test test-integration test-benchmark

GREETINGS_DIR = tests/data/greetings

download-greetings:
	@mkdir -p $(GREETINGS_DIR)/frankj-dob $(GREETINGS_DIR)/gavvllaw $(GREETINGS_DIR)/chetaniitbhilai
	$(eval TMP := $(shell mktemp -d))
	git clone --depth 1 https://github.com/frankj-dob/voicemail-greetings.git $(TMP)/frankj-dob
	find $(TMP)/frankj-dob -type f \( -name '*.mp3' -o -name '*.wav' \) -exec cp {} $(GREETINGS_DIR)/frankj-dob/ \;
	git clone --depth 1 https://github.com/GavvlLaw/voicemail-greetings.git $(TMP)/gavvllaw
	find $(TMP)/gavvllaw -type f \( -name '*.mp3' -o -name '*.wav' \) -exec cp {} $(GREETINGS_DIR)/gavvllaw/ \;
	git clone --depth 1 https://github.com/chetaniitbhilai/Streaming-Voicemail-Greeting-to-Message-Detection.git $(TMP)/chetaniitbhilai
	find $(TMP)/chetaniitbhilai/data -type f \( -name '*.mp3' -o -name '*.wav' \) -exec cp {} $(GREETINGS_DIR)/chetaniitbhilai/ \;
	rm -rf $(TMP)
	@echo "Downloaded greetings to $(GREETINGS_DIR):"
	@echo "  frankj-dob:     $$(ls $(GREETINGS_DIR)/frankj-dob | wc -l) files"
	@echo "  gavvllaw:       $$(ls $(GREETINGS_DIR)/gavvllaw | wc -l) files"
	@echo "  chetaniitbhilai: $$(ls $(GREETINGS_DIR)/chetaniitbhilai | wc -l) files"

# gen-noisy-greetings mixes the clean human greetings with real background
# noise, drawing each segment from the middle of a recording so there is
# definitely noise present. NOISE_DIR holds the source recordings (.wav or
# .mp3); anything not already 16 kHz mono is converted on load.
NOISE_DIR ?= tests/data/noise
gen-noisy-greetings:
	@test -d $(NOISE_DIR) || (echo "Error: $(NOISE_DIR) not found; set NOISE_DIR" && exit 1)
	go run ./cmd/gen-noisy-greetings -human $(GREETINGS_DIR)/human -noise $(NOISE_DIR) -out $(GREETINGS_DIR)

gen-human-greetings:
	@test -n "$$ELEVENLABS_API_KEY" || (echo "Error: ELEVENLABS_API_KEY is required" && exit 1)
	go run ./cmd/gen-greetings -out $(GREETINGS_DIR)/human

docker:
	docker build -t vpbx/$(IMAGE):$(TAG) -t vpbx/$(IMAGE):latest .

docker-push: docker
	docker push vpbx/$(IMAGE):$(TAG)
	docker push vpbx/$(IMAGE):latest

docker-master:
	docker build -t vpbx/$(IMAGE):master -t vpbx/$(IMAGE):master .

docker-push-master: docker
	docker push vpbx/$(IMAGE):master


clean:
	rm -f $(BINARY) openapi-gen
