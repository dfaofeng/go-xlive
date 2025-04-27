@echo off
TITLE Start Event Service
echo Starting Event Service...
cd /d d:\go-xlive
go run ./cmd/event/main.go -config ./configs
pause