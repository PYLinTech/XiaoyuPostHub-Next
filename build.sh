#!/usr/bin/env bash
#
# 一键构建：前端 → 同步进 Go 包 → 编译成单个自包含二进制。
#
# 产物落在 dist/：
#   dist/xphd                  默认（本机架构）
#   dist/xphd-linux-amd64 ...   交叉编译或 --all 时的矩阵产物
#   dist/SHA256SUMS            仅 --all 时生成
#
# 用法：
#   ./build.sh                  构建前端 + 本机架构二进制
#   ./build.sh --all            交叉编译常见平台矩阵
#   ./build.sh --skip-frontend  跳过前端，复用上一次 frontend/dist，只编二进制
#   ./build.sh --only-frontend  只构建前端，不编二进制
#   GOOS=linux GOARCH=arm64 ./build.sh    指定单个目标
#
# 两个"只做一半"的参数是互补的，不是同义词：--skip-frontend 砍掉前半段、
# --only-frontend 砍掉后半段。起名时不要再用 skip 打头说"跳过什么"，
# 那样两个参数看上去都在跳过前端，读的人得跑一遍才知道区别。
#
# 环境要求：Go 与 Node.js（npm）。缺任何一个都会明确报错并指出缺哪个。

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$ROOT"

FRONTEND_DIR="$ROOT/frontend"
EMBED_DIR="$ROOT/internal/webui/dist"
OUT_DIR="$ROOT/dist"

SKIP_FRONTEND=0
ONLY_FRONTEND=0
ALL=0

die() { printf '\033[31m错误\033[0m %s\n' "$1" >&2; exit 1; }
info() { printf '\033[36m==>\033[0m %s\n' "$1"; }
warn() { printf '\033[33m警告\033[0m %s\n' "$1" >&2; }

usage() {
  # 打印文件头部注释块（第 2 行到第一个空行为止），而不是写死行号——
  # 写死的话以后改一次文案，帮助信息就会悄悄错位。
  sed -n '2,/^$/p' "${BASH_SOURCE[0]}" | sed 's/^#\{0,1\} \{0,1\}//'
  exit 0
}

for arg in "$@"; do
  case "$arg" in
    --all) ALL=1 ;;
    --skip-frontend) SKIP_FRONTEND=1 ;;
    --only-frontend) ONLY_FRONTEND=1 ;;
    -h|--help) usage ;;
    *) die "未知参数：${arg}（--help 看用法）" ;;
  esac
done

command -v go >/dev/null 2>&1 || die "找不到 go，请先安装 Go 工具链"

# 宿主架构必须在读任何 GOOS/GOARCH 之前定下来，而且要把这两个变量从这次查询里
# 摘出去：`go env GOOS` 返回的是**当前生效**的值，用户用 `GOOS=linux ./build.sh`
# 调用时它跟着就是 linux。拿它当"本机架构"会把交叉产物错命名成 dist/xphd，
# 顺手覆盖掉真正的本机二进制——一个只在交叉编译时才出现、还很难查的坑。
HOST_GOOS="$(env -u GOOS -u GOARCH go env GOOS)"
HOST_GOARCH="$(env -u GOOS -u GOARCH go env GOARCH)"
if [ "$SKIP_FRONTEND" -eq 0 ] && [ "$ONLY_FRONTEND" -eq 0 ]; then
  command -v npm >/dev/null 2>&1 || die "找不到 npm。只想编译二进制请加 --skip-frontend"
fi

# ---------------------------------------------------------------- 前端

if [ "$SKIP_FRONTEND" -eq 0 ]; then
  [ -d "$FRONTEND_DIR" ] || die "找不到 frontend/ 目录"
  command -v npm >/dev/null 2>&1 || die "找不到 npm"

  cd "$FRONTEND_DIR"
  # 有 lockfile 就用 ci：它严格按锁文件装，可重复；没有才退回 install。
  if [ -f package-lock.json ]; then
    info "安装前端依赖（npm ci）"
    npm ci --no-audit --no-fund
  else
    info "安装前端依赖（npm install）"
    npm install --no-audit --no-fund
  fi
  # build 会先跑 vue-tsc 类型检查再打包；类型不过就不该产出二进制。
  info "构建前端（含类型检查）"
  npm run build
  cd "$ROOT"

  [ -f "$FRONTEND_DIR/dist/index.html" ] || die "前端构建结束但没有产出 dist/index.html"
elif [ ! -f "$FRONTEND_DIR/dist/index.html" ]; then
  warn "--skip-frontend 但 frontend/dist 不存在，内嵌的会是上一份产物或空目录"
fi

if [ "$ONLY_FRONTEND" -eq 1 ]; then
  info "已跳过二进制编译（--only-frontend）"
  info "下一步：./build.sh --skip-frontend 可复用刚构建的 frontend/dist 出二进制"
  exit 0
fi

# ---------------------------------------------------------------- 同步进 Go 包

# go:embed 只能内嵌"包目录之内"的文件，frontend/dist 在包外，因此必须先搬进
# internal/webui/dist 再编译。.gitkeep 是占位（保证没跑过前端构建时也能编译），
# 同步时保留它。
info "同步前端产物到 internal/webui/dist"
mkdir -p "$EMBED_DIR"
find "$EMBED_DIR" -mindepth 1 -not -name '.gitkeep' -delete 2>/dev/null || true
if [ -d "$FRONTEND_DIR/dist" ]; then
  # 逐项拷贝而不是 cp -R 整个目录：后者在不同系统上会把目录本身套进去一层。
  for item in "$FRONTEND_DIR"/dist/*; do
    [ -e "$item" ] || continue
    cp -R "$item" "$EMBED_DIR/"
  done
fi
[ -f "$EMBED_DIR/index.html" ] || die "内嵌目录里没有 index.html，内嵌会得到一个没有页面的二进制"

# ---------------------------------------------------------------- 编译

mkdir -p "$OUT_DIR"

# CGO_ENABLED=0：SQLite 走的是 modernc.org 的纯 Go 实现，不需要 cgo 关掉它
# 才能交叉编译；关掉之后产物不依赖 glibc/musl 差异，拷到目标机上直接能跑。
export CGO_ENABLED=0
# -trimpath 去掉构建机的绝对路径（可复现，且不泄露目录结构）
# -s -w 去符号表与调试信息，体积小一大截
LDFLAGS="-s -w"

build_one() {
  local goos="$1" goarch="$2"
  local suffix="" out
  if [ "$goos" = "$HOST_GOOS" ] && [ "$goarch" = "$HOST_GOARCH" ]; then
    out="$OUT_DIR/xphd"
  else
    suffix="-$goos-$goarch"
    out="$OUT_DIR/xphd$suffix"
  fi
  [ "$goos" = "windows" ] && out="$out.exe"

  info "编译 $goos/$goarch → ${out#$ROOT/}"
  GOOS="$goos" GOARCH="$goarch" \
    go build -trimpath -ldflags "$LDFLAGS" -o "$out" ./cmd/XiaoyuPostHub-Next
}

if [ "$ALL" -eq 1 ]; then
  # 常见部署组合。macOS 交叉编译出的二进制不能直接跑（要签名/公证），
  # 但可以交给 CI 打包，这里只保证"能产出"。
  for target in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64; do
    build_one "${target%/*}" "${target#*/}"
  done
  info "生成校验和"
  ( cd "$OUT_DIR" && shasum -a 256 xphd* > SHA256SUMS 2>/dev/null || sha256sum xphd* > SHA256SUMS )
elif [ -n "${GOOS:-}" ] || [ -n "${GOARCH:-}" ]; then
  build_one "${GOOS:-$HOST_GOOS}" "${GOARCH:-$HOST_GOARCH}"
else
  build_one "$HOST_GOOS" "$HOST_GOARCH"
fi

# ---------------------------------------------------------------- 收尾

info "完成"
printf '\n'
ls -lh "$OUT_DIR" | tail -n +2 | awk '{printf "  %-28s %s\n", $9, $5}'
printf '\n'
printf '  部署：把上面的二进制单独拷到目标机即可，前端已内嵌，无需再带 dist 目录。\n'
printf '  注意：数据库与 master.key 不在二进制里，分别由 XPH_DB_PATH / XPH_DATA_DIR 决定。\n'
