#!/bin/bash
set -e # 如果任何命令失败，立即退出

echo "更新 Buf 依赖..."
buf dep update

echo "生成 Go 代码..."
# 指定要生成的模块路径是 'proto'
buf generate proto
echo "生成 SQLC 代码..."
sqlc generate # 添加这一行

echo "整理 Go 模块依赖..."
go mod tidy

echo "完成。"