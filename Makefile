.PHONY: test vet build build-nas docker run-extract-local clean

test:                ## unit tests — run anywhere (Mac included), pure stdlib
	go test ./...

vet:
	go vet ./...
	gofmt -l . && test -z "$$(gofmt -l .)"

build:               ## native binary (your Mac)
	go build -o dist/merger ./cmd/merger

build-nas:           ## static linux/amd64 binary for the DS220+
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 \
	  go build -trimpath -ldflags="-s -w" -o dist/merger-linux-amd64 ./cmd/merger

docker:              ## container image for DSM Container Manager
	docker buildx build --platform linux/amd64 -t takeout-merger:latest .

# Local end-to-end smoke test: put a small real takeout-*.tgz in ./testdata/archives
run-extract-local: build
	./dist/merger extract \
	  --archives ./testdata/archives \
	  --staging  /tmp/merger-staging \
	  --dry-run

clean:
	rm -rf dist
