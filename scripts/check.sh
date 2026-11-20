#!/usr/bin/env bash
# 本地质量门禁。与 .github/workflows/ci.yml 保持一致。
#
# 存在的理由：本机没有安装 make，Makefile 里的目标跑不起来；
# 而门禁命令一旦靠手敲，就会漏掉 gofumpt 或前端 lint。
#
# 用法：bash scripts/check.sh          # 全量
#       SKIP_WEB=1 bash scripts/check.sh  # 只查 Go
set -euo pipefail

cd "$(dirname "$0")/.."

echo "==> go build"
go build ./...

echo "==> go vet"
go vet ./...

echo "==> go test"
go test ./...

echo "==> gofumpt"
gofumpt_bin="${GOFUMPT:-$(go env GOPATH)/bin/gofumpt}"
if [[ ! -x "$gofumpt_bin" ]]; then
  echo "gofumpt 未安装。安装：go install mvdan.cc/gofumpt@v0.12.0" >&2
  exit 1
fi
unformatted="$("$gofumpt_bin" -l .)"
if [[ -n "$unformatted" ]]; then
  echo "以下文件需要 gofumpt 重排：" >&2
  echo "$unformatted" >&2
  echo "修复：$gofumpt_bin -w ." >&2
  exit 1
fi

if [[ "${SKIP_WEB:-0}" == "1" ]]; then
  echo "==> 跳过前端（SKIP_WEB=1）"
elif [[ -d web/node_modules ]]; then
  echo "==> web lint + build"
  (cd web && npm run lint && npm run build)
else
  echo "==> 跳过前端：web/node_modules 不存在（cd web && npm install）" >&2
fi

echo "全部通过"
