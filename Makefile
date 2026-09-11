GOOS ?= linux
GOARCH ?= amd64
VERSION ?= 0.1.0
BUILD_DIR ?= dist/$(GOOS)/$(GOARCH)

# Artifact filename: kimi-tools.so so `plugins.configs.kimi-tools` matches.
SO_NAME := kimi-tools.so

ifeq ($(GOOS),windows)
	SO_NAME := kimi-tools.dll
endif
ifeq ($(GOOS),darwin)
	SO_NAME := kimi-tools.dylib
endif

.PHONY: all build test clean

all: build

build:
	@mkdir -p $(BUILD_DIR)
	CGO_ENABLED=1 GOOS=$(GOOS) GOARCH=$(GOARCH) go build \
		-buildmode=c-shared \
		-ldflags "-X main.buildVersion=$(VERSION)" \
		-o $(BUILD_DIR)/$(SO_NAME) .
	@echo "built $(BUILD_DIR)/$(SO_NAME)"

test:
	go test -v ./...

clean:
	rm -rf dist/
