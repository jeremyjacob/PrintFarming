BINARY := bambuddy-plate-guard
VERSION ?= dev

.PHONY: build test vet fmt clean

build:
	mkdir -p bin
	CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o bin/$(BINARY) .

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -w .

clean:
	rm -rf bin coverage.out
