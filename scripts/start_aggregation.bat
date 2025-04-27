@echo off
TITLE Start Aggregation Service
echo Starting Aggregation Service...
cd /d d:\go-xlive
go run ./cmd/aggregation/main.go -config ./configs
pause