.PHONY: build test test-local test-docker test-manifests test-all run clean

IMAGE_NAME := omnivm
IMAGE_TAG := latest

# Build the Docker image
build:
	docker build -t $(IMAGE_NAME):$(IMAGE_TAG) .

# Run local tests (dispatcher, signals, arrow — no cgo runtimes needed)
test-local:
	go test -race -v ./pkg/dispatcher/ ./pkg/signals/ ./pkg/arrow/

# Run full test suite inside Docker
test-docker: build
	docker run --rm $(IMAGE_NAME):$(IMAGE_TAG) -python "print('Python OK')"
	docker run --rm $(IMAGE_NAME):$(IMAGE_TAG) -js "console.log('JS OK')"
	docker run --rm $(IMAGE_NAME):$(IMAGE_TAG) -ruby "puts 'Ruby OK'"
	@echo "All runtime smoke tests passed!"

# Run manifest test suite (11 manifests across 6 categories)
test-manifests: build
	@OMNIVM_IMAGE=$(IMAGE_NAME):$(IMAGE_TAG) ./scripts/test-manifests.sh

# Run manifest tests in quick mode (skip Express/pastebin)
test-manifests-quick: build
	@OMNIVM_IMAGE=$(IMAGE_NAME):$(IMAGE_TAG) ./scripts/test-manifests.sh --quick

# Run stress tests (52 cross-runtime tests)
test-stress: build
	docker run --rm --entrypoint stresstest $(IMAGE_NAME):$(IMAGE_TAG)

# Run full test suite
test: test-local test-docker

# Run everything: unit tests, smoke tests, stress tests, manifest tests
test-all: test test-stress test-manifests

# Start the REPL
run: build
	docker run -it --rm $(IMAGE_NAME):$(IMAGE_TAG)

# Execute a script
run-file: build
	docker run -it --rm -v $(PWD)/examples:/scripts $(IMAGE_NAME):$(IMAGE_TAG) -file /scripts/$(FILE)

# Clean up
clean:
	docker rmi $(IMAGE_NAME):$(IMAGE_TAG) 2>/dev/null || true
	go clean -testcache
