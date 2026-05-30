.PHONY: build test test-local test-unit test-docker test-cli test-manifests test-libomnivm-manifests test-libomnivm-stress test-all run clean

IMAGE_NAME := omnivm
IMAGE_TAG := latest

# Build the Docker image
build:
	docker build --target builder -t $(IMAGE_NAME):$(IMAGE_TAG) .

# Run local tests (dispatcher, signals, arrow, cli, errmsg, golang — no cgo runtimes needed)
test-local:
	go test -race -v ./pkg/dispatcher/ ./pkg/signals/ ./pkg/arrow/ ./pkg/cli/ ./pkg/errmsg/
	go test -v -count=1 ./pkg/golang/

# Run Go, cgo runtime, integration, and Python package tests inside Docker.
test-unit:
	docker build --target tester -t $(IMAGE_NAME):tester .

# Run full test suite inside Docker
test-docker: build
	docker run --rm $(IMAGE_NAME):$(IMAGE_TAG) -python "print('Python OK')"
	docker run --rm $(IMAGE_NAME):$(IMAGE_TAG) -js "console.log('JS OK')"
	docker run --rm $(IMAGE_NAME):$(IMAGE_TAG) -ruby "puts 'Ruby OK'"
	@echo "All runtime smoke tests passed!"

# Run CLI integration tests (new run subcommand, argv, stdin, shebang, etc.)
test-cli: build
	docker run --rm --entrypoint /bin/bash $(IMAGE_NAME):$(IMAGE_TAG) /omnivm/scripts/test-cli.sh

# Run manifest test suite (26 manifests across 7 categories)
test-manifests: build
	@OMNIVM_IMAGE=$(IMAGE_NAME):$(IMAGE_TAG) ./scripts/test-manifests.sh

test-libomnivm-manifests: build
	@OMNIVM_IMAGE=$(IMAGE_NAME):$(IMAGE_TAG) ./scripts/test-libomnivm-manifests.sh

test-libomnivm-stress: build
	@OMNIVM_IMAGE=$(IMAGE_NAME):$(IMAGE_TAG) ./scripts/test-libomnivm-stress.sh

# Run manifest tests in quick mode (skip Express/pastebin)
test-manifests-quick: build
	@OMNIVM_IMAGE=$(IMAGE_NAME):$(IMAGE_TAG) ./scripts/test-manifests.sh --quick

# Run stress tests (52 cross-runtime tests)
test-stress: build
	docker run --rm --entrypoint stresstest $(IMAGE_NAME):$(IMAGE_TAG)

# Run the canonical full test suite.
test: test-all

# Run everything: unit/integration tests, smoke tests, CLI tests, stress tests, manifest tests
test-all: test-unit test-docker test-cli test-stress test-manifests test-libomnivm-manifests test-libomnivm-stress

# Start the REPL
run: build
	docker run -it --rm $(IMAGE_NAME):$(IMAGE_TAG)

# Execute a script (new syntax)
run-file: build
	docker run -it --rm -v $(PWD)/examples:/scripts $(IMAGE_NAME):$(IMAGE_TAG) run /scripts/$(FILE)

# Clean up
clean:
	docker rmi $(IMAGE_NAME):$(IMAGE_TAG) 2>/dev/null || true
	go clean -testcache
