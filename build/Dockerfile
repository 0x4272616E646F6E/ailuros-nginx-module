# syntax=docker/dockerfile:1
# Builds the Ailuros dynamic module inside the stock NGINX Gateway Fabric data
# plane image, so the .so is compiled against the toolchain and NGINX version
# that will load it.

ARG NGF_VERSION

FROM ghcr.io/nginx/nginx-gateway-fabric/nginx:${NGF_VERSION} AS source
ARG NGINX_VERSION
ARG NGINX_SHA256
USER root
RUN apk add --no-cache alpine-sdk linux-headers pcre2-dev openssl-dev zlib-dev wget
# The stock image must run the NGINX version we are about to compile against.
RUN set -eu; \
    nginx -V > /tmp/nginx-v.txt 2>&1; \
    got="$(sed -n 's#^nginx version: nginx/##p' /tmp/nginx-v.txt)"; \
    if [ "$got" != "${NGINX_VERSION}" ]; then \
        echo "ERROR: NGINX version mismatch: image reports '$got', versions.json says '${NGINX_VERSION}'" >&2; exit 1; \
    fi; \
    if ! grep -q -- '--with-compat' /tmp/nginx-v.txt; then \
        echo "ERROR: the image's nginx was not built with --with-compat" >&2; exit 1; \
    fi
RUN set -eu; \
    wget -qO /tmp/nginx.tar.gz "https://nginx.org/download/nginx-${NGINX_VERSION}.tar.gz"; \
    got="$(sha256sum /tmp/nginx.tar.gz | cut -d' ' -f1)"; \
    if [ "$got" != "${NGINX_SHA256}" ]; then \
        echo "ERROR: NGINX_SHA256 mismatch for nginx-${NGINX_VERSION}.tar.gz: expected ${NGINX_SHA256}, got $got" >&2; exit 1; \
    fi; \
    mkdir /src && tar -xzf /tmp/nginx.tar.gz -C /src --strip-components=1 && rm /tmp/nginx.tar.gz

FROM source AS module
COPY module /src/module
WORKDIR /src
RUN ./configure --with-compat --add-dynamic-module=/src/module \
        --with-cc-opt='-Wall -Wextra -Werror' \
    && make modules

FROM ghcr.io/nginx/nginx-gateway-fabric/nginx:${NGF_VERSION} AS verify
USER root
COPY --from=module /src/objs/ngx_http_ailuros_module.so /usr/lib/nginx/modules/ngx_http_ailuros_module.so
RUN printf '%s\n' \
        'load_module /usr/lib/nginx/modules/ngx_http_ailuros_module.so;' \
        'pid /tmp/nginx.pid;' \
        'events {}' \
        'http { server { listen 8080; ailuros on; } }' \
        > /tmp/verify.conf \
    && nginx -t -c /tmp/verify.conf

FROM scratch AS export
COPY --from=module /src/objs/ngx_http_ailuros_module.so /ngx_http_ailuros_module.so
