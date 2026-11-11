GITHASH := $(shell git describe --always --exclude '*')
GOOS ?= $(shell go env GOOS)
GOARCH ?= $(shell go env GOARCH)

ifeq ($(GOOS),linux)
ifneq (,$(filter $(GOARCH),amd64 arm64))
CGO_ENABLED ?= 1
endif
endif

# Direct bypassing caches to ensure we don't get stale or out-of-date versions
export GOPROXY=direct

.PHONY: build docker docker release updatedeps

build:
	CGO_ENABLED=$(or $(CGO_ENABLED),0) go build -tags goolm -ldflags="-X main.githash=$(GITHASH)"
	@echo $(GITHASH)

docker:
	docker build -t 42wim/matterircd .

lint:
	golangci-lint run
	golangci-lint run vendor/github.com/matterbridge/matterclient/
	golangci-lint run vendor/github.com/matterbridge/matrixclient/

release:
	goreleaser release --skip publish
	cd dist && set -e; \
	for i in $$(awk '{ print $$2 }' checksums.txt); do \
		dist_arch=$$(echo $$i | awk -F- '{ print $$(NF-1) "-" $$NF }' | sed -e 's/-/_/g' -e 's/.exe//g' -e 's/armv6/arm_6/g'); \
		mv matterircd_$${dist_arch}*/matterircd* $$i; \
	done; \
	rm -f artifacts.json config.yaml

updatedeps:
	go get github.com/matterbridge/matterclient@master
	go get github.com/matterbridge/matrixclient@master
	go mod tidy && go mod vendor
	# https://github.com/alecthomas/chroma/pull/1344
	git checkout -q vendor/github.com/alecthomas/chroma/v2/formatters/mirc.go
	# github.com/sorcix deprecated, restore our own local changes
	@rm -f vendor/github.com/sorcix/irc/message.go
	git checkout -q vendor/github.com/sorcix/irc/message.go
