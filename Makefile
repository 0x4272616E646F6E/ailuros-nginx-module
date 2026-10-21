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

.PHONY: build verify test check-ngf clean

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

test:
	cd test && go test ./...

clean:
	rm -rf out dist
