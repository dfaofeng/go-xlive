@echo off
TITLE Start User Service
echo Starting User Service...
cd /d d:\go-xlive
go run ./cmd/user/main.go -config ./configs
pause