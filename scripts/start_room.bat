@echo off
TITLE Start Room Service
echo Starting Room Service...
cd /d d:\go-xlive
go run ./cmd/room/main.go -config ./configs
pause