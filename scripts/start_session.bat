@echo off
TITLE Start Session Service
echo Starting Session Service...
cd /d d:\go-xlive
go run ./cmd/session/main.go -config ./configs
pause