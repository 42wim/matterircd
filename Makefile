GITHASH := $(shell git describe --always --exclude '*')
GOOS ?= $(shell go env GOOS)
GOARCH ?= $(shell go env GOARCH)

ifeq ($(GOOS),linux)
ifneq (,$(filter $(GOARCH),amd64 arm64))
CGO_ENABLED ?= 1
endif
endif

.PHONY: build lint

build:
	CGO_ENABLED=$(or $(CGO_ENABLED),0) go build -tags goolm -ldflags="-X main.githash=$(GITHASH)"
	@echo $(GITHASH)

lint:
	golangci-lint run -v --new-from-rev HEAD~5
