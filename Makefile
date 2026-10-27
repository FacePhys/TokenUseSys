CONFIG ?= config.yaml

.PHONY: build test lint server selfcheck

build:
	go build ./...

test:
	go test ./...

lint:
	go vet ./...
	@test -z "$$(gofmt -l .)" || (gofmt -l . && exit 1)
	@# 运行镜像（scratch）里没有系统时区数据库，所有程序都必须带上 time/tzdata。
	@for cmd in $$(ls cmd); do \
		go list -deps ./cmd/$$cmd | grep -qx time/tzdata || { echo "cmd/$$cmd 没有引入 time/tzdata"; exit 1; }; \
	done

# 本地开发：config.yaml 里把 storage 的路径改到 data/，log.dir 留空写标准输出。
server:
	go run ./cmd/server -config $(CONFIG)

# M1 自检：只读，约 3～5 分钟；脱敏样本写进 testdata/fixtures/，提交前再看一眼。
selfcheck:
	go run ./cmd/relayctl -config $(CONFIG) selfcheck -fixtures testdata/fixtures
