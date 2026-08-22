#!/bin/bash
# build_benzhi_docker.sh - SHURL 短链接服务评测镜像构建脚本
# 用法: ./build_benzhi_docker.sh [镜像名] [标签] [平台]
# 默认: 镜像名=shurl, 标签=latest, 平台=linux/amd64

set -e

# ============================================================
# 1. 参数与基础配置
# ============================================================
IMAGE_NAME="${1:-shurl}"
IMAGE_TAG="${2:-latest}"
PLATFORM="${3:-linux/amd64}"
CONTEXT_PATH="$(cd "$(dirname "$0")" && pwd)"

# ============================================================
# 2. 前置检查: Docker 是否可用
# ============================================================
if ! command -v docker >/dev/null 2>&1; then
    echo "错误: 找不到 docker 命令，请先安装 Docker"
    exit 1
fi
if ! docker info > /dev/null 2>&1; then
    echo "错误: Docker 服务未启动，请先启动 Docker（例如 systemctl start docker）"
    exit 1
fi
if [ ! -f "${CONTEXT_PATH}/benzhi.Dockerfile" ]; then
    echo "错误: 找不到 benzhi.Dockerfile（期望在 ${CONTEXT_PATH} 目录）"
    exit 1
fi

# ============================================================
# 3. 核心构建
# ============================================================
echo "=========================================="
echo "SHURL 镜像构建开始"
echo "  镜像名称 : ${IMAGE_NAME}:${IMAGE_TAG}"
echo "  目标平台 : ${PLATFORM}"
echo "  构建上下文: ${CONTEXT_PATH}"
echo "=========================================="

docker build \
    -f "${CONTEXT_PATH}/benzhi.Dockerfile" \
    -t "${IMAGE_NAME}:${IMAGE_TAG}" \
    --platform "${PLATFORM}" \
    "${CONTEXT_PATH}"

# ============================================================
# 4. 构建结果与运行示例
# ============================================================
echo ""
echo "✅ 镜像构建成功: ${IMAGE_NAME}:${IMAGE_TAG}"
echo ""
echo "镜像大小:"
docker images "${IMAGE_NAME}:${IMAGE_TAG}" --format "table {{.Repository}}\t{{.Tag}}\t{{.Size}}"
echo ""
echo "----------------------------------------------------------------"
echo "【本地运行示例】"
echo "  # 后台运行容器，映射 8080 -> 8080："
echo "  docker run -d --name shurl-server -p 8080:8080 ${IMAGE_NAME}:${IMAGE_TAG}"
echo ""
echo "  # 进入容器内执行 go build / go test -race："
echo "  docker run --rm -it ${IMAGE_NAME}:${IMAGE_TAG} /bin/bash"
echo "  root@xxx:/app# go build ./..."
echo "  root@xxx:/app# go test ./... -race -count=5"
echo ""
echo "  # 验证接口："
echo "  curl -s http://localhost:8080/health"
echo "  curl -s http://localhost:8080/ready"
echo "  curl -s -X POST http://localhost:8080/api/urls \\"
echo "       -H 'Content-Type: application/json' \\"
echo "       -d '{\"raw_url\":\"https://example.com\"}'"
echo "----------------------------------------------------------------"
echo ""
echo "【构建其他平台】"
echo "  ./build_benzhi_docker.sh ${IMAGE_NAME} ${IMAGE_TAG} linux/amd64"
echo "  ./build_benzhi_docker.sh ${IMAGE_NAME} ${IMAGE_TAG} linux/arm64"
