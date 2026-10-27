# 构建 server 和 relayctl 两个静态二进制；运行镜像不含 shell 和包管理器（开发文档 §12.2）。
FROM golang:1.27.2-bookworm AS build
WORKDIR /src
ARG GOPROXY=https://goproxy.cn,direct
ENV CGO_ENABLED=0 GOPROXY=${GOPROXY}
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go build -trimpath -ldflags="-s -w" -o /out/server ./cmd/server \
 && go build -trimpath -ldflags="-s -w" -o /out/relayctl ./cmd/relayctl \
 && mkdir -p /rootfs/var/lib/relayusage/backups /rootfs/var/log/relayusage /rootfs/tmp

FROM scratch
# 请求 https://4router.net 和机器人 webhook 要用 CA 证书。
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=build /out/ /app/
# 数据、日志目录归运行用户所有；挂载新的命名卷时会继承这个属主。
COPY --from=build --chown=65532:65532 /rootfs/var/lib/relayusage /var/lib/relayusage
COPY --from=build --chown=65532:65532 /rootfs/var/log/relayusage /var/log/relayusage
# SQLite 大排序、临时表放不下内存时要写临时文件，scratch 里没有可写目录会报 SQLITE_IOERR_GETTEMPPATH。
COPY --from=build --chown=65532:65532 /rootfs/tmp /tmp
USER 65532:65532
ENTRYPOINT ["/app/server"]
CMD ["-config", "/etc/relayusage/config.yaml"]
