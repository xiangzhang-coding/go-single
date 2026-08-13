#!/usr/bin/env bash
# 生成本地演示用自签证书（T28 SSL 终止）。
# 仅用于本地 HTTPS 演示（curl -k 或浏览器信任提示）；生产走 Let's Encrypt 等受信 CA，
# 详见 docs/DEPLOYMENT.md。证书目录 certs/ 不入库（.gitignore）。
set -euo pipefail
cd "$(dirname "$0")"

mkdir -p certs
openssl req -x509 -nodes -newkey rsa:2048 -days 365 \
    -keyout certs/go-single.key \
    -out certs/go-single.crt \
    -subj "/CN=127.0.0.1" \
    -addext "subjectAltName=IP:127.0.0.1,DNS:localhost"

echo "生成完成：deploy/nginx/certs/go-single.{crt,key}（有效 365 天）"
