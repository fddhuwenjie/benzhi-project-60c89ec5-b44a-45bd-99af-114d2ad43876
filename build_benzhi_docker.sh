#!/usr/bin/env bash
set -euo pipefail

image_name=${1:?必须提供镜像名称}
platform=${2:?必须提供目标架构}

case "$platform" in
  linux/amd64|linux/arm64) ;;
  *) echo "目标架构必须是 linux/amd64 或 linux/arm64" >&2; exit 2 ;;
esac

docker buildx build \
  --platform "$platform" \
  --file benzhi.Dockerfile \
  --tag "${image_name}:latest" \
  --load \
  .
