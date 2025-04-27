@echo off
TITLE Start Realtime Service
echo Starting Realtime Service...
cd /d d:\go-xlive
go run ./cmd/realtime/main.go -config ./configs
pause