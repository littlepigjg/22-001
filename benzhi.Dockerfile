# benzhi.Dockerfile - SHURL 短链接服务评测镜像
# 注意：容器内必须保留完整 Go 工具链，不能使用多阶段编译。
# 基于 golang:1.22 官方镜像。

FROM golang:1.22

WORKDIR /app

# 全局禁用 CGO：项目为纯标准库实现，禁用 CGO 后可确保跨架构（amd64/arm64）
# 构建和容器内 go build / go test / go run 均不依赖平台特定的 cgo 工具链
# （arm64 模拟环境下 cgo 经常出现 exit status 2 等工具链问题）。
ENV CGO_ENABLED=0

# 复制整个项目源码到 /app
COPY . .

# 预先下载依赖（虽然纯标准库，但保留以便以后扩展）并验证可编译
RUN go mod download && go build ./...

# 确保 /app/data 存在（JSON 存储路径）
RUN mkdir -p /app/data

# 默认启动服务，监听 8080
CMD ["go", "run", "./cmd/server"]
