SHELL := /bin/sh

APP_NAME ?= buy-sms
BIN_DIR ?= bin
BIN_EXT ?=
DOCKER ?= docker
COMPOSE ?= docker compose
COMPOSE_CMD := $(COMPOSE) --env-file .env
POSTGRES_VOLUME_NAME ?= $(shell sed -n 's/^POSTGRES_VOLUME_NAME=//p' .env 2>/dev/null | tail -n 1 | tr -d '\r')
POSTGRES_VOLUME_NAME := $(or $(strip $(POSTGRES_VOLUME_NAME)),buy-sms_postgres-data)

.DEFAULT_GOAL := help

.PHONY: help setup dev-api dev-web frontend-build sync-web build test fmt vet \
	docker-volume-create docker-volume-check docker-build docker-up docker-down docker-logs docker-config \
	docker-build-cn docker-up-cn docker-config-cn

help: ## 显示可用命令
	@awk 'BEGIN {FS = ":.*## "; printf "\n用法：make <目标>\n\n"} /^[a-zA-Z0-9_-]+:.*## / {printf "  %-20s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

setup: ## 下载 Go 与前端依赖
	go mod download
	npm --prefix frontend install --no-audit --no-fund

dev-api: ## 启动后端开发服务
	go run ./cmd/server

dev-web: ## 启动 Vue 开发服务
	npm --prefix frontend run dev

frontend-build: ## 构建前端
	npm --prefix frontend run build

sync-web: ## 将前端产物同步到 Go 嵌入目录
	node -e "const fs=require('node:fs');fs.rmSync('web/dist',{recursive:true,force:true});fs.mkdirSync('web/dist',{recursive:true});fs.cpSync('frontend/dist','web/dist',{recursive:true});fs.writeFileSync('web/dist/placeholder.txt','此文件仅用于保证全新检出的 Go 嵌入目录非空；生产构建会同时嵌入 Vue 构建产物。\n');"

build: frontend-build sync-web ## 构建包含前端资源的单一可执行文件
	go build -buildvcs=false -trimpath -o $(BIN_DIR)/$(APP_NAME)$(BIN_EXT) ./cmd/server

test: ## 执行后端测试和前端生产构建检查
	go test ./...
	npm --prefix frontend run build

fmt: ## 格式化 Go 代码
	go fmt ./...

vet: ## 执行 Go 静态检查
	go vet ./...

docker-volume-create: ## 首次部署时显式创建 PostgreSQL 外部卷
	$(DOCKER) volume create "$(POSTGRES_VOLUME_NAME)"

docker-volume-check: ## 只读检查 PostgreSQL 外部卷是否存在
	@$(DOCKER) volume inspect "$(POSTGRES_VOLUME_NAME)" >/dev/null 2>&1 || { \
		echo "错误：PostgreSQL 外部卷 $(POSTGRES_VOLUME_NAME) 不存在。已部署环境请停止部署并恢复原卷；仅确认首次部署时执行 make docker-volume-create。" >&2; \
		exit 1; \
	}

docker-config: ## 校验国际网络 Compose 配置
	$(COMPOSE_CMD) config --quiet

docker-build: ## 使用国际网络源构建镜像
	$(COMPOSE_CMD) build

docker-up: docker-volume-check ## 使用国际网络源启动容器
	$(COMPOSE_CMD) up -d --build

docker-down: ## 停止并移除容器（保留数据库卷）
	$(COMPOSE_CMD) down

docker-logs: ## 查看应用容器日志
	$(COMPOSE_CMD) logs -f app

docker-config-cn: ## 校验中国大陆镜像源 Compose 配置
	$(COMPOSE_CMD) -f docker-compose.yml -f docker-compose.cn.yml config --quiet

docker-build-cn: ## 使用中国大陆镜像源构建镜像
	$(COMPOSE_CMD) -f docker-compose.yml -f docker-compose.cn.yml build

docker-up-cn: docker-volume-check ## 使用中国大陆镜像源启动容器
	$(COMPOSE_CMD) -f docker-compose.yml -f docker-compose.cn.yml up -d --build
