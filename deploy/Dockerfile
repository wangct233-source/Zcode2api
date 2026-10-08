# 多阶段构建：编译 → 精简运行镜像
# 阶段一：Go 编译（静态链接，禁 CGO）
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/zcode2api ./cmd/gateway

# 阶段二：运行镜像（含 chromium 供 claim 模块使用）
FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates curl chromium \
    && rm -rf /var/lib/apt/lists/*
# Rod 会自动发现系统 chromium
ENV ROD_BROWSER_BIN=/usr/bin/chromium
# 容器必须监听所有网卡，端口映射才能转发进来
# （env 优先级高于 config.yaml，裸跑二进制不受影响，默认仍为 127.0.0.1 安全默认）
ENV ZG_HOST=0.0.0.0
COPY --from=build /out/zcode2api /app/zcode2api
WORKDIR /app
# 数据目录挂载点：账号库 / 配置 / 更新缓存
VOLUME ["/app/data"]
EXPOSE 17800
HEALTHCHECK --interval=30s --timeout=5s --retries=3 \
  CMD curl -fsS http://127.0.0.1:17800/health || exit 1
ENTRYPOINT ["/app/zcode2api", "serve"]
