# FlowEdge Backend Dockerfile
FROM golang:1.25-alpine AS builder

WORKDIR /app
COPY backend/go.mod backend/go.sum ./
RUN go mod download
COPY backend/ .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -o flowedge-backend .

# 运行阶段
FROM alpine:latest
RUN apk --no-cache add ca-certificates ffmpeg wget

WORKDIR /app
COPY --from=builder /app/flowedge-backend .
# 流水线定义是产品形态本身，不是可选数据：缺 data/pipelines.json 引擎会 log.Fatalf 直接退出。
# 干净 clone 里它由 git 跟踪，构建上下文里一定存在。
COPY data/pipelines.json ./data/pipelines.json

RUN adduser -D -u 10001 flowedge && mkdir -p /app/data && chown -R flowedge /app
USER flowedge

EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=3s --start-period=10s --retries=3 \
  CMD wget -qO- http://127.0.0.1:8080/api/health >/dev/null || exit 1

CMD ["./flowedge-backend"]
