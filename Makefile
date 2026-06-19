BINARY ?= golieipp
CMD ?= ./cmd/golieipp
OUT_DIR ?= dist
GOOS ?= linux
GOARCH ?= amd64
CGO_ENABLED ?= 0

.PHONY: build linux-x86 clean test

build:
	mkdir -p $(OUT_DIR)
	CGO_ENABLED=$(CGO_ENABLED) GOOS=$(GOOS) GOARCH=$(GOARCH) go build -o $(OUT_DIR)/$(BINARY)-$(GOOS)-$(GOARCH) $(CMD)

linux-x86: GOOS := linux
linux-x86: GOARCH := amd64
linux-x86: build

test:
	go test ./...

clean:
	rm -rf $(OUT_DIR)
