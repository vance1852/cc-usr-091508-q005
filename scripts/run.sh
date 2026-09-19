#!/usr/bin/env bash
# 便捷启动脚本：优先使用仓库内 .toolchain 的 Go，否则用系统 go。
set -euo pipefail
cd "$(dirname "$0")/.."

if [ -x .toolchain/usr/lib/go-1.19/bin/go ]; then
  export GOROOT="$PWD/.toolchain/usr/lib/go-1.19"
  export PATH="$GOROOT/bin:$PATH"
fi
export GOPATH="${GOPATH:-$PWD/.gopath}"
export GOPROXY="${GOPROXY:-https://proxy.golang.org}"

exec go run ./cmd/server "$@"
