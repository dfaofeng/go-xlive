@echo off
TITLE Start Gateway Service
echo Starting Gateway Service...
cd /d d:\go-xlive
go run ./cmd/gateway/main.go -config ./configs
pause