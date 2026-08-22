# benzhi.Dockerfile - SHURL 短链接服务评测镜像
# 注意：容器内必须保留完整 Go 工具链，不能使用多阶段编译。
# 基于 golang:1.22 官方镜像。

FROM golang:1.22

WORKDIR /app

# 复制整个项目源码到 /app
COPY . .

# 预先下载依赖（虽然纯标准库，但保留以便以后扩展）并验证可编译
RUN go mod download && go build ./...

# 确保 /app/data 存在（JSON 存储路径）
RUN mkdir -p /app/data

# 默认启动服务，监听 8080
CMD ["go", "run", "./cmd/server"]
