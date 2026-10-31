#!/bin/zsh
# 双击启动：开一个终端窗口跑本地服务，然后自动打开浏览器。
cd "$(dirname "$0")" || exit 1
exec ./weread-export
