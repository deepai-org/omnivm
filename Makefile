.PHONY: build test test-local test-docker run clean

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

# Run full test suite
test: test-local test-docker

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
