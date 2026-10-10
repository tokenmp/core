GO ?= go
GOLANGCI_LINT ?= golangci-lint

.PHONY: build test lint fmt fmt-check check tools

# 编译检查。本模块是纯库，没有 main 包，`go build ./...` 只做类型与链接检查，
# 不产出任何文件。
build:
	$(GO) build ./...

test:
	$(GO) test -race ./...

lint:
	$(GOLANGCI_LINT) run

fmt:
	$(GOLANGCI_LINT) fmt

fmt-check:
	$(GOLANGCI_LINT) fmt --diff

# 门禁：只依赖 Go 与 golangci-lint，不需要网络、数据库或 node。
check: build test lint fmt-check

# 按 .mise.toml 装齐本机工具链。
tools:
	mise install
