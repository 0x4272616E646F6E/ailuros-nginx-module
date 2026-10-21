# Build the Ailuros NGINX module per NGF release.
#   make build  NGF=2.8.0 ARCH=amd64
#   make verify NGF=2.8.0
VERSION ?= dev
ARCH    ?= amd64
NGF     ?=

ROW = $(shell jq -c --arg ngf '$(NGF)' '.[] | select(.ngf == $$ngf)' versions.json)
NGINX_VERSION = $(shell printf '%s' '$(ROW)' | jq -r .nginx)
NGINX_SHA256  = $(shell printf '%s' '$(ROW)' | jq -r .nginx_sha256)

OUT_SO = out/ngx_http_ailuros_module-$(VERSION)-ngf$(NGF)-linux-$(ARCH).so
BUILD_ARGS = --build-arg NGF_VERSION=$(NGF) \
	--build-arg NGINX_VERSION=$(NGINX_VERSION) \
	--build-arg NGINX_SHA256=$(NGINX_SHA256)

BUILD_ARGS_ASAN = --build-arg NGINX_VERSION=$(NGINX_VERSION) --build-arg NGINX_SHA256=$(NGINX_SHA256)

# Host-compiler build for CodeQL tracing only; see scripts/build-host.sh.
build-host: check-ngf
	scripts/build-host.sh $(NGF)

.PHONY: release-assets release-clean build-host build verify test test-asan test-contract test-kind cppcheck check-ngf clean

check-ngf:
	@command -v jq >/dev/null || { echo "jq is required but not installed" >&2; exit 1; }
	@test -r versions.json || { echo "versions.json is missing or unreadable" >&2; exit 1; }
	@test -n "$(NGF)" || { echo "NGF is required, e.g. make build NGF=2.8.0" >&2; exit 1; }
	@test -n "$(ROW)" || { echo "NGF $(NGF) not found in versions.json" >&2; exit 1; }

# NGINX_SHA256 can be overridden on the command line to test the check.
build: check-ngf
	docker buildx build --platform linux/$(ARCH) $(BUILD_ARGS) -f build/Dockerfile \
		--target export --output type=local,dest=out/.stage-$(ARCH) .
	mv out/.stage-$(ARCH)/ngx_http_ailuros_module.so $(OUT_SO)
	rmdir out/.stage-$(ARCH)
	@echo "wrote $(OUT_SO)"

verify: check-ngf
	docker buildx build --platform linux/$(ARCH) $(BUILD_ARGS) -f build/Dockerfile \
		--target verify --progress=plain --output type=cacheonly .

# NGF selects the versions.json row (and so the stock image and the built
# module) under test; unset, the first row is used. An unknown NGF fails.
test:
	@if [ -n "$(NGF)" ]; then $(MAKE) --no-print-directory check-ngf NGF=$(NGF); fi
	cd test && AILUROS_NGF=$(NGF) go test ./... -count=1 -timeout 30m $(TESTFLAGS)

# The behaviour suite against NGINX and the module built with AddressSanitizer
# and UBSan (build/Dockerfile.asan), for the host's architecture. Local test
# image only; the .so mounted into it comes from the same ASan build.
ASAN_IMAGE = ailuros-nginx-asan:local
test-asan: check-ngf
	docker buildx build $(BUILD_ARGS_ASAN) -f build/Dockerfile.asan --target verify --progress=plain --output type=cacheonly .
	docker buildx build $(BUILD_ARGS_ASAN) -f build/Dockerfile.asan --target runtime -t $(ASAN_IMAGE) --load .
	rm -rf out/.asan && mkdir -p out/.asan
	docker buildx build $(BUILD_ARGS_ASAN) -f build/Dockerfile.asan --target export --output type=local,dest=out/.asan .
	cd test && AILUROS_NGF=$(NGF) AILUROS_MODULE_IMAGE=$(ASAN_IMAGE) \
		AILUROS_MODULE_SO=$(CURDIR)/out/.asan/ngx_http_ailuros_module.so \
		go test ./behaviour/... -count=1 -timeout 30m $(TESTFLAGS)

# The module against the published Ailuros (pinned by digest in
# test/contract/contract_test.go) and a stub model. Needs the module built:
# make build NGF=<ngf> ARCH=<host arch> VERSION=dev, or AILUROS_MODULE_SO.
test-contract: check-ngf
	cd test && AILUROS_NGF=$(NGF) go test ./contract/... -count=1 -timeout 15m $(TESTFLAGS)

# NGF-in-kind end to end: installs deploy/ngf/ into a real NGF in a throwaway
# kind cluster (ailuros-module-it, temporary kubeconfig; yours is never used).
# Needs docker, kind, kubectl and helm, network access for the NGF chart and
# images, and the module built: make build NGF=<ngf> ARCH=<host arch>
# VERSION=dev, or AILUROS_MODULE_SO.
test-kind: check-ngf
	cd test && AILUROS_NGF=$(NGF) go test -tags kind ./kind/... -count=1 -timeout 30m -v $(TESTFLAGS)

# Uses a local cppcheck if there is one, otherwise Alpine's package in a
# throwaway container of the stock NGF image (NGF, or the first versions.json row).
CPPCHECK_FLAGS = --enable=warning,style,performance --error-exitcode=1
CPPCHECK_NGF   = $(or $(NGF),$(shell jq -r '.[0].ngf' versions.json))
cppcheck:
	@if command -v cppcheck >/dev/null; then \
		cppcheck $(CPPCHECK_FLAGS) module/src; \
	else \
		docker run --rm --user root --entrypoint sh \
			-v "$(CURDIR)/module/src:/src:ro" \
			ghcr.io/nginx/nginx-gateway-fabric/nginx:$(CPPCHECK_NGF) \
			-c 'apk add --no-cache cppcheck >/dev/null && cppcheck --version && cppcheck $(CPPCHECK_FLAGS) /src'; \
	fi

# Every versions.json row x {amd64, arm64}: the .so, its ConfigMap YAML and
# dist/SHA256SUMS, with the release's names. VERSION must be set, e.g.
#   make release-assets VERSION=v0.1.0
# NGF=<ngf> and RELEASE_ARCHES="<arch>..." narrow the build to one row or
# architecture (the release workflow builds each natively, then runs
# release-checksums once over the merged dist/). A foreign architecture builds
# under QEMU emulation.
RELEASE_ARCHES = amd64 arm64
.PHONY: release-build release-checksums
release-assets: release-clean release-build release-checksums

release-clean:
	rm -rf dist

release-build:
	@test "$(VERSION)" != dev || { echo "VERSION is required, e.g. make release-assets VERSION=v0.1.0" >&2; exit 1; }
	@command -v jq >/dev/null || { echo "jq is required but not installed" >&2; exit 1; }
	mkdir -p dist out
	set -e; for ngf in $$(jq -r --arg ngf '$(NGF)' '.[] | select($$ngf == "" or .ngf == $$ngf) | .ngf' versions.json); do \
		for arch in $(RELEASE_ARCHES); do \
			$(MAKE) --no-print-directory build NGF=$$ngf ARCH=$$arch VERSION=$(VERSION); \
			so=ngx_http_ailuros_module-$(VERSION)-ngf$$ngf-linux-$$arch.so; \
			cp out/$$so dist/$$so; \
			AILUROS_VERSION=$(VERSION) scripts/configmap.sh dist/$$so $$ngf $$arch > dist/ailuros-nginx-module-ngf$$ngf-$$arch.yaml; \
		done; \
	done

release-checksums:
	cd dist && if command -v sha256sum >/dev/null; then sha256sum *.so *.yaml; else shasum -a 256 *.so *.yaml; fi > SHA256SUMS
	@echo "wrote dist/:" && ls dist

clean:
	rm -rf out dist
