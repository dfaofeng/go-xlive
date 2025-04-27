@echo off
TITLE Start Bilibili Adapter Service
echo Starting Bilibili Adapter Service...
cd /d d:\go-xlive
go run ./cmd/adapter-bilibili/main.go -config ./configs
pause