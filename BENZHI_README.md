# BENZHI_README

## 项目说明
- 项目：benzhi-project-60c89ec5-b44a-45bd-99af-114d2ad43876
- 项目用途：提供文物修复项目从登记、方案基线、工序证据、质检整改到专家放行和不可变归档的闭环 HTTP JSON 服务。
- Go 工具链：`golang:1.22`
- 前端工具链：无

## 项目描述
- 项目名称：文物修复质量放行服务
- 项目介绍：为文物修复团队提供从项目登记、工序记录、质检整改到专家放行和档案归档的一条闭环流程，通过可追溯的证据链控制修复质量。
- 项目概述：为文物修复团队提供从项目登记、工序记录、质检整改到专家放行和档案归档的一条闭环流程，通过可追溯的证据链控制修复质量。
- 核心工作流：修复项目登记→方案与风险确认→工序执行→证据质检→问题整改→复核通过→专家放行→档案归档
- 对外接口：HTTP JSON API；服务支持 -addr=127.0.0.1:<port> 或 PORT 环境变量，默认监听 127.0.0.1:19081，提供项目、工序、质检、整改、放行和归档接口。

## 标准构建、运行和测试命令
进入容器后执行：
cd '/app' && GOTOOLCHAIN=local go build ./...

cd /app && GOTOOLCHAIN=local go run ./cmd/server -self-check -addr=127.0.0.1:19081

cd '/app' && GOTOOLCHAIN=local go test ./...

## Docker 构建和进入容器
chmod +x build_benzhi_docker.sh

./build_benzhi_docker.sh benzhi-project-60c89ec5-b44a-45bd-99af-114d2ad43876-amd64 linux/amd64

./build_benzhi_docker.sh benzhi-project-60c89ec5-b44a-45bd-99af-114d2ad43876-arm64 linux/arm64

docker run -it benzhi-project-60c89ec5-b44a-45bd-99af-114d2ad43876-amd64:latest

## 题目验证命令
1. 预期退出码 0：`go test ./...`
2. 预期退出码 0：`go run ./cmd/server -self-check -addr=127.0.0.1:19081`
