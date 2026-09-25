BINARY ?= golieipp
CMD ?= ./cmd/golieipp
OUT_DIR ?= dist
GOOS ?= linux
GOARCH ?= amd64
CGO_ENABLED ?= 0
TAGS ?=

# Keep this recursive so target-specific TAGS values are visible to prerequisites.
GO_TAG_ARGS = $(if $(strip $(TAGS)),-tags $(TAGS),)

.PHONY: build linux-x86 clean test

build:
	mkdir -p $(OUT_DIR)
	CGO_ENABLED=$(CGO_ENABLED) GOOS=$(GOOS) GOARCH=$(GOARCH) go build $(GO_TAG_ARGS) -o $(OUT_DIR)/$(BINARY)-$(GOOS)-$(GOARCH) $(CMD)

linux-x86: GOOS := linux
linux-x86: GOARCH := amd64
linux-x86: TAGS := avahi
linux-x86: build

test:
	go test $(GO_TAG_ARGS) ./...

clean:
	rm -rf $(OUT_DIR)
