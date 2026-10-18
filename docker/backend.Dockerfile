# FlowEdge Backend Dockerfile
FROM golang:1.25-alpine AS builder

WORKDIR /app
COPY backend/go.mod backend/go.sum ./
RUN go mod download
COPY backend/ .
RUN CGO_ENABLED=0 GOOS=linux go build -o flowedge-backend .

# 运行阶段
FROM alpine:latest
RUN apk --no-cache add ca-certificates ffmpeg

WORKDIR /app
COPY --from=builder /app/flowedge-backend .

EXPOSE 8080
CMD ["./flowedge-backend"]
