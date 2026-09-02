# m-ui 构建入口。sing-box 的 Reality/uTLS 需要 with_utls 构建标签,所有目标统一带上。
TAGS   := with_utls
LDFLAGS := -w -s
BIN    := m-ui

.PHONY: build linux test vet run clean

build:            ## 本机平台二进制
	go build -tags $(TAGS) -trimpath -ldflags "$(LDFLAGS)" -o $(BIN) .

linux:            ## Linux amd64 静态二进制(无 CGO)
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -tags $(TAGS) -trimpath -ldflags "$(LDFLAGS)" -o dist/$(BIN) .

test:             ## 全部测试(含全协议 sing-box 干跑)
	go test -tags $(TAGS) ./...

vet:
	go vet -tags $(TAGS) ./...

run: build        ## 本机运行(数据库 ./m-ui.db)
	./$(BIN) run -db m-ui.db

clean:
	rm -rf $(BIN) $(BIN).exe dist
