#!/bin/bash
set -e

# ============================================
#  Git Worktree 分支创建脚本
#  用法：在任意项目目录下运行，输入数量即可创建对应分支
# ============================================

# 检查是否在 Git 仓库中
if ! git rev-parse --git-dir > /dev/null 2>&1; then
    echo "[ERROR] 当前目录不是 Git 仓库，请先初始化 Git"
    exit 1
fi

# 获取当前目录名作为分支名前缀
CURRENT_DIR_NAME="$(basename "$(pwd)")"
# 获取当前分支名
CURRENT_BRANCH="$(git rev-parse --abbrev-ref HEAD)"

echo "当前目录: $(pwd)"
echo "当前分支: $CURRENT_BRANCH"
echo "分支前缀: $CURRENT_DIR_NAME"
echo

# 提示输入创建数量
read -p "请输入要创建的分支数量: " COUNT

# 验证输入
if ! [[ "$COUNT" =~ ^[0-9]+$ ]] || [ "$COUNT" -le 0 ]; then
    echo "[ERROR] 请输入大于 0 的正整数"
    exit 1
fi

# 获取父目录路径
PARENT_DIR="$(dirname "$(pwd)")"

echo
echo "将基于分支 '$CURRENT_BRANCH' 创建 $COUNT 个 worktree 分支："
echo "============================================================"

# 创建分支
for ((i=1; i<=COUNT; i++)); do
    NEW_BRANCH="${CURRENT_DIR_NAME}-${i}"
    NEW_DIR="${PARENT_DIR}/${NEW_BRANCH}"

    # 检查分支是否已存在
    if git show-ref --verify --quiet "refs/heads/$NEW_BRANCH" 2>/dev/null; then
        echo "[SKIP] 分支 '$NEW_BRANCH' 已存在，跳过"
        continue
    fi

    # 检查目录是否已存在
    if [ -d "$NEW_DIR" ]; then
        echo "[SKIP] 目录 '$NEW_DIR' 已存在，跳过"
        continue
    fi

    echo -n "[CREATE] 创建分支 '$NEW_BRANCH' -> "
    if git worktree add -b "$NEW_BRANCH" "$NEW_DIR" "$CURRENT_BRANCH"; then
        echo "✓ 成功"
    else
        echo "✗ 失败"
        echo "[ERROR] 创建分支 '$NEW_BRANCH' 失败"
        exit 1
    fi
done

echo "============================================================"
echo
echo "完成！已创建 $COUNT 个 worktree 分支。"
echo
echo "📂 目录结构："
echo "   ${PARENT_DIR}/"
for ((i=1; i<=COUNT; i++)); do
    NEW_BRANCH="${CURRENT_DIR_NAME}-${i}"
    if git show-ref --verify --quiet "refs/heads/$NEW_BRANCH" 2>/dev/null; then
        echo "   ├── ${NEW_BRANCH}/  (分支: $NEW_BRANCH)"
    fi
done

echo
echo "💡 使用提示："
echo "   1. cd 到任意子目录即可开始开发"
echo "   2. 在子目录中运行 ./commit.sh 提交代码"
echo "   3. 运行 'git worktree list' 查看所有分支"
echo "   4. 运行 'git worktree remove <路径>' 删除分支"
