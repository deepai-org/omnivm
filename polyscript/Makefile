.PHONY: build test test-js clean

build:
	npm run build

test: build
	npm test -- --runInBand

test-js:
	npm test -- --runInBand

clean:
	rm -rf dist
