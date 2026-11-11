# FlowEdge Frontend Dockerfile
FROM node:22-alpine AS builder

WORKDIR /app
# lockfile 是提交进仓库的：用 npm ci 做可复现安装。
# 以前写的是 `npm ci || npm install`，装失败会静默退回「装到哪算哪」，
# 镜像能起来但依赖版本已经和 lock 不一致。
COPY frontend/package.json frontend/package-lock.json ./
RUN npm ci
COPY frontend/ .
RUN npm run build

# 运行阶段
FROM nginx:alpine
COPY --from=builder /app/dist /usr/share/nginx/html
COPY docker/nginx.conf /etc/nginx/conf.d/default.conf

EXPOSE 80
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
  CMD wget -qO- http://127.0.0.1/ >/dev/null || exit 1
CMD ["nginx", "-g", "daemon off;"]
