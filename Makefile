GITHASH := $(shell git describe --always --exclude '*')
GOOS ?= $(shell go env GOOS)
GOARCH ?= $(shell go env GOARCH)

ifeq ($(GOOS),linux)
ifneq (,$(filter $(GOARCH),amd64 arm64))
CGO_ENABLED ?= 1
endif
endif

.PHONY: build lint docker release

build:
	CGO_ENABLED=$(or $(CGO_ENABLED),0) go build -tags goolm -ldflags="-X main.githash=$(GITHASH)"
	@echo $(GITHASH)

lint:
	golangci-lint run -v --new-from-rev HEAD~5

docker:
	docker build -t 42wim/matterircd .

release:
	goreleaser release --skip publish
	cd dist; for i in $$(awk '{ print $$2 }' checksums.txt); do \
		dist_arch=$$(echo $$i | cut -d'-' -f3,4 | sed -e 's/-/_/g' -e 's/.exe//g' -e 's/armv6/arm_6/g'); \
		mv matterircd_$${dist_arch}*/matterircd* $$i; \
	done; rm -f artifacts.json config.yaml
