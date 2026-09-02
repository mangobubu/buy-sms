# syntax=docker/dockerfile:1.7

# 三个基础镜像均可替换为境内镜像代理，默认使用国际 Docker Hub。
ARG NODE_IMAGE=node:22-alpine
ARG GO_IMAGE=golang:1.24-alpine
ARG RUNTIME_IMAGE=alpine:3.21

FROM ${NODE_IMAGE} AS frontend-builder
WORKDIR /src/frontend

ARG NPM_REGISTRY=https://registry.npmjs.org
COPY frontend/package*.json ./
RUN --mount=type=cache,target=/root/.npm \
    npm config set registry "${NPM_REGISTRY}" \
    && if [ -f package-lock.json ]; then \
         npm ci --no-audit --no-fund; \
       else \
         npm install --no-audit --no-fund --no-package-lock; \
       fi

COPY frontend/ ./
RUN npm run build

FROM ${GO_IMAGE} AS backend-builder
WORKDIR /src

ARG GOPROXY=https://proxy.golang.org,direct
ARG GOSUMDB=sum.golang.org
ARG TARGETOS=linux
ARG TARGETARCH=amd64

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    GOPROXY="${GOPROXY}" GOSUMDB="${GOSUMDB}" go mod download

COPY . ./
# web 包负责通过 go:embed 嵌入 dist；前端构建产物必须在 Go 编译前就位。
COPY --from=frontend-builder /src/frontend/dist/ ./web/dist/

RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS="${TARGETOS}" GOARCH="${TARGETARCH}" \
    go build -buildvcs=false -trimpath -ldflags="-s -w -buildid=" \
    -o /out/buy-sms ./cmd/server

FROM ${RUNTIME_IMAGE} AS runtime

ARG ALPINE_MIRROR=
ARG DEBIAN_MIRROR=
ARG DEBIAN_SECURITY_MIRROR=

# 同时兼容 Alpine 与 Debian slim 运行镜像；境内构建可替换系统软件源。
RUN set -eux; \
    if [ -f /etc/alpine-release ]; then \
      if [ -n "${ALPINE_MIRROR}" ]; then \
        sed -i "s#https\?://[^/]*/alpine#${ALPINE_MIRROR%/}#g" /etc/apk/repositories; \
      fi; \
      apk add --no-cache ca-certificates tzdata wget; \
      addgroup -S -g 10001 app; \
      adduser -S -D -H -u 10001 -G app app; \
    else \
      if [ -n "${DEBIAN_MIRROR}" ]; then \
        if [ -f /etc/apt/sources.list ]; then \
          sed -i "s#https\?://deb.debian.org/debian#${DEBIAN_MIRROR%/}#g" /etc/apt/sources.list; \
        fi; \
        if [ -f /etc/apt/sources.list.d/debian.sources ]; then \
          sed -i "s#https\?://deb.debian.org/debian#${DEBIAN_MIRROR%/}#g" /etc/apt/sources.list.d/debian.sources; \
        fi; \
      fi; \
      if [ -n "${DEBIAN_SECURITY_MIRROR}" ]; then \
        if [ -f /etc/apt/sources.list ]; then \
          sed -i "s#https\?://security.debian.org/debian-security#${DEBIAN_SECURITY_MIRROR%/}#g" /etc/apt/sources.list; \
        fi; \
        if [ -f /etc/apt/sources.list.d/debian.sources ]; then \
          sed -i "s#https\?://security.debian.org/debian-security#${DEBIAN_SECURITY_MIRROR%/}#g; s#https\?://deb.debian.org/debian-security#${DEBIAN_SECURITY_MIRROR%/}#g" /etc/apt/sources.list.d/debian.sources; \
        fi; \
      fi; \
      apt-get update; \
      DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends ca-certificates tzdata wget; \
      rm -rf /var/lib/apt/lists/*; \
      groupadd --gid 10001 app; \
      useradd --uid 10001 --gid app --no-create-home --shell /usr/sbin/nologin app; \
    fi

WORKDIR /app
COPY --from=backend-builder --chown=10001:10001 /out/buy-sms /app/buy-sms

ENV APP_ENV=production \
    APP_ADDR=:8080 \
    TZ=Asia/Shanghai

USER 10001:10001
EXPOSE 8080
ENTRYPOINT ["/app/buy-sms"]

