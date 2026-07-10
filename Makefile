# loop-eng Makefile
# 纯 Go（CGO_ENABLED=0，sqlite 用 modernc 纯 Go 驱动），不碰 SDK/CGO。
BINARY  := loop-eng
PKG     := ./cmd/loop-eng
GO      := go

.PHONY: build install uninstall test clean fmt vet

# build: 编译当前平台的二进制到仓库根 ./loop-eng
build:
	CGO_ENABLED=0 $(GO) build -o $(BINARY) $(PKG)

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
