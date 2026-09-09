# loop-eng Makefile
# 纯 Go（CGO_ENABLED=0，sqlite 用 modernc 纯 Go 驱动），不碰 SDK/CGO。
BINARY  := loop-eng
PKG     := ./cmd/loop-eng
GO      := go

# VERSION：release 版本号，注入进二进制（loop-eng --version / daemon 横幅可见）。
# 默认取 git describe（tag 优先，无 tag 回落短 hash，工作树脏带 -dirty 后缀）；
# 显式覆盖：make build VERSION=v1.2.3。拿不到 git（空）时二进制回退内嵌 VCS 信息。
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null)

.PHONY: build install uninstall test clean fmt vet

# build: 编译当前平台的二进制到仓库根 ./loop-eng
build:
	CGO_ENABLED=0 $(GO) build -ldflags "-X loop-eng/internal/cli.version=$(VERSION)" -o $(BINARY) $(PKG)

# install: 装进 $$GOPATH/bin（需该目录在 PATH 才能裸命令调用）
install:
	CGO_ENABLED=0 $(GO) install $(PKG)

# uninstall: 移除已安装的二进制
uninstall:
	rm -f "$$( $(GO) env GOPATH )/bin/$(BINARY)"

# test: 全套测试（loop-eng 自身的 verify.deterministic 就是这条）
test:
	CGO_ENABLED=0 $(GO) test ./...

fmt:
	$(GO) fmt ./...

vet:
	CGO_ENABLED=0 $(GO) vet ./...

clean:
	rm -f $(BINARY)
