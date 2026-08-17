<div align="center">

# CodexDownloadProxy

**Codex / CC Switch 桌面客户端下载代理服务** —— 零依赖 Go 单文件，下载页内嵌二进制，服务器端代拉官方安装包，带 HMAC 签名防盗链。

[![Docker Pulls](https://img.shields.io/docker/pulls/minghongcai/codex-download-proxy)](https://hub.docker.com/r/minghongcai/codex-download-proxy)
[![Docker Image Version](https://img.shields.io/docker/v/minghongcai/codex-download-proxy?sort=semver)](https://hub.docker.com/r/minghongcai/codex-download-proxy)
[![Go Version](https://img.shields.io/badge/Go-1.26-blue)](https://go.dev/)

---

## 💖 赞助支持

<a href="https://apac.milorouter.com">
  <img src="assets/milo.png" alt="MiloRouter 中转站" width="120">
</a>

### [MiloRouter 中转站](https://apac.milorouter.com)

**AI API 中转服务 · 稳定 · 高速 · 国内友好**

如果本项目帮到了你，欢迎前往 [apac.milorouter.com](https://apac.milorouter.com) 了解支持 🙏

---

</div>

## 功能

- ✅ Codex 桌面版下载：macOS（Apple Silicon / Intel）、Windows（x64 / ARM64）
- ✅ CC Switch 客户端下载：macOS、Windows、Linux
- ✅ 服务器端代拉，用户不直连官方源 / GitHub / 微软商店
- ✅ 每次下载自动跟随最新版（Codex 走官方 `latest` 地址，CC Switch 走 GitHub 最新 Release）
- ✅ HMAC 签名短时下载票据（15 分钟），裸链接一律 `403`，防盗链
- ✅ 无磁盘缓存，纯流式转发；多架构 Docker 镜像（amd64 / arm64）

## 快速开始（Docker）

```bash
docker pull minghongcai/codex-download-proxy:latest

docker run -d --restart unless-stopped -p 8080:8080 \
  -e DOWNLOAD_SIGN_KEY="$(openssl rand -hex 32)" \
  minghongcai/codex-download-proxy:latest
```

打开 `http://你的服务器IP:8080/` 即可使用。

推荐用 docker compose 管理：

```yaml
services:
  codex-download-proxy:
    image: minghongcai/codex-download-proxy:latest
    restart: unless-stopped
    ports:
      - "8080:8080"
    environment:
      - DOWNLOAD_SIGN_KEY=<替换成随机字符串，例如 openssl rand -hex 32 的输出>
```

生产环境建议放在 nginx / Caddy / Cloudflare 后面，并让服务只监听本机：

```bash
docker run -d --restart unless-stopped -p 127.0.0.1:8080:8080 \
  -e DOWNLOAD_SIGN_KEY="$(openssl rand -hex 32)" \
  minghongcai/codex-download-proxy:latest
```

## 环境变量

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| `ADDR` | `:8080` | 监听地址，反代部署时设为 `127.0.0.1:8080` |
| `DOWNLOAD_SIGN_KEY` | 每次启动随机 | 签名密钥，**务必设置固定值**，否则重启后旧链接全部失效 |
| `CODEX_MAC_ARM64_URL` | OpenAI 官方 `Codex.dmg` | macOS Apple Silicon 上游 |
| `CODEX_MAC_X64_URL` | OpenAI 官方 `Codex-latest-x64.dmg` | macOS Intel 上游 |
| `CODEX_WIN_X64_URL` | codex-app-mirror CDN | Windows x64 上游（微软商店无稳定直链） |
| `CODEX_WIN_ARM64_URL` | codex-app-mirror CDN | Windows ARM64 上游 |
| `CCSWITCH_API_URL` | GitHub cc-switch 最新 Release API | CC Switch 版本解析地址 |

## 主要路由

| 路由 | 说明 |
| --- | --- |
| `/` | 内嵌下载页（Codex + CC Switch） |
| `/api/ticket?platform=<id>` | 获取 15 分钟有效的签名下载链接 |
| `/api/ccswitch` | CC Switch 最新版本与资产信息 |
| `/download/{platform}?expires=&token=` | 签名后的下载代理端点 |
| `/healthz` | 健康检查 |

## 从源码构建

```bash
go build -o codex-proxy .
./codex-proxy
```

## 构建并推送多架构镜像

```bash
docker buildx build \
  --platform linux/amd64,linux/arm64 \
  --push \
  -t minghongcai/codex-download-proxy:latest \
  .
```

## 安全说明

- 下载链接为 HMAC 签名票据，禁止直接分享裸链接（15 分钟过期、防篡改）
- 无磁盘缓存，所有流量经服务器转发，带宽消耗与下载量成正比，建议在反代层做按 IP 限流
- 签名密钥 `DOWNLOAD_SIGN_KEY` 请妥善保管，泄露后他人可自行签发下载链接
