@echo off
chcp 65001 > nul
setlocal
rem chcp 65001 将代码页切换到 UTF-8
rem > nul 禁止 chcp 命令本身的输出 ("Active code page: 65001")
@echo off
setlocal

echo 更新 Buf 依赖...
buf dep update
if %errorlevel% neq 0 (
    echo 更新 Buf 依赖失败。
    exit /b %errorlevel%
)

echo 生成 Go 代码...
rem 指定要生成的模块路径是 'proto'
buf generate proto
if %errorlevel% neq 0 (
    echo 生成 Go 代码失败。
    exit /b %errorlevel%
)
echo 生成 SQLC 代码...
sqlc generate # 添加这一行
if %errorlevel% neq 0 (
    echo 生成 SQLC 代码失败。
    exit /b %errorlevel%
)
echo 整理 Go 模块依赖...
go mod tidy
if %errorlevel% neq 0 (
    echo 执行 go mod tidy 失败。
    exit /b %errorlevel%
)

echo 完成。
endlocal