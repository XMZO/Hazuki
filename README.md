# Hazuki Proxy Suite

<p align="center">
  <a href="https://github.com/XMZO/Hazuki">
    <img src="./avatar.png" alt="Hazuki" width="160" />
  </a>
</p>

Hazuki 是一个 **单进程多端口** 的代理套件：内置 SQLite 配置中心 + Web 管理面板，把常用的反代能力做成可配置模块并组合使用。

- Web 面板：配置管理、版本回滚、导入/导出备份、服务状态/健康检查、流量统计
- 热更新：大部分配置保存后立即生效；**端口变更需要重启进程/容器**
- 可选落盘加密：通过 `HAZUKI_MASTER_KEY` 加密保存敏感字段（支持在面板轮换）
- 模块：Torcherino / cdnjs / Git / Sakuya（Oplist）

## 快速部署（Docker Compose）

部署文件在 `go/` 目录：

```bash
cd go
cp .env.example .env
docker compose pull
docker compose up -d
```

默认会拉取 `ghcr.io/xmzo/hazuki-go:latest`。该镜像由 GitHub Actions 在 `main` 分支推送后自动编译并发布，当前同时提供 `linux/amd64` 和 `linux/arm64`。
如果你的 GHCR 包还是私有状态，需要先执行 `docker login ghcr.io`，或把该包改成 public 再让服务器匿名拉取。
运行时镜像使用更小的 distroless 底座，因此容器里默认没有 `bash` / `sh`。

启动后打开：

- 面板：`http://你的服务器:3100`
- 首次初始化：`http://你的服务器:3100/setup`
- 快速向导：`http://你的服务器:3100/wizard`
- 系统状态：`http://你的服务器:3100/system`
- 流量统计：`http://你的服务器:3100/traffic`

> 提示：敏感字段在面板里通常会被“留空=保持不变”的方式保护；如需清除请使用对应的 “clear/清除” 选项。

## 默认端口

- `3100`：Admin 面板
- `3000`：Torcherino（通用反代）
- `3001`：cdnjs（jsDelivr 缓存代理，Redis 可选）
- `3002`：Git（GitHub Raw 代理，可新增多实例）
- `3200`：Sakuya（OpenList 直链加速）

## 健康检查

- admin：`http://HOST:3100/_hazuki/health`
- torcherino：`http://HOST:3000/_hazuki/health`
- cdnjs：`http://HOST:3001/_hazuki/health`
- git：`http://HOST:3002/_hazuki/health`
- sakuya：`http://HOST:3200/_hazuki/health`

## 模块说明（简要）

- **Torcherino**：通用反代，支持 `HOST_MAPPING` 域名映射；可选 Worker Secret Header 注入
- **cdnjs**：面向 `jsDelivr` 的缓存代理；Redis 可选但推荐；缓存 TTL 以文件后缀为主，可在面板配置默认值与覆盖表
- **Git**：GitHub Raw 代理；支持 Token、CORS、Cache-Control 策略、替换规则；可在面板新增多实例（需要额外暴露端口）
- **Sakuya（Oplist）**：对接 OpenList / Oplist 的直链加速
  > **注意**：如果 OpenList 挂载了 115，且 Hazuki 部署在中国大陆以外，115 缺少海外节点，直链在海外可能非常不稳定（如握手超时/502），不推荐用于 115 网盘。

## 配置与账号

- 首次启动会读取 `go/.env` 并写入 SQLite（仅 DB 为空时生效）；之后以 SQLite 为准，建议用 Web 面板管理。
- 第一次访问会引导到 `http://你的服务器:3100/setup` 创建管理员；也可在 `go/.env` 里提前设置 `HAZUKI_ADMIN_USERNAME` / `HAZUKI_ADMIN_PASSWORD`（仅首次生效）。
- `HAZUKI_MASTER_KEY` 用于敏感字段加密存储。已存在 `enc:v1:...` 数据时，启动必须提供同一个 key；轮换请在 `/system` 执行，以确保数据库内容重加密并同步更新环境变量。
- Redis：Docker Compose 默认包含 Redis；如果你不用 Docker，请自行部署 Redis 并设置 `REDIS_HOST`/`REDIS_PORT`（也可在面板里改）。
- 如果你部署的是自己的 fork，可在 `go/.env` 里设置 `HAZUKI_IMAGE=ghcr.io/<your-owner>/hazuki-go:latest`，让 compose 拉取你自己的 GHCR 镜像。

## 备份（导出/导入）

面板支持把数据库导出为 `.hzdb` 文件，并在导入时覆盖当前数据库内容：

- 导出：`/config/export`
- 导入：`/config/import`

## 本地运行（不使用 Docker）

要求：Go 1.24+

```bash
cd go
cp .env.example .env
go run ./cmd/hazuki
```

或编译：

```bash
cd go
go build -trimpath -ldflags "-s -w" -o hazuki-go ./cmd/hazuki
./hazuki-go
```

## Nginx 反代示例（以 cdnjs 为例）

```nginx
location / {
    proxy_pass http://127.0.0.1:3001;
    proxy_set_header Host $host;
    proxy_set_header X-Forwarded-Proto $scheme;
}
```

## 仓库结构

```
├── go/            # 主程序与部署文件
├── src/           # 旧实现（维护模式，仅保留兼容/参考）
├── cdnjs/         # 旧：独立版本（参考）
├── git/           # 旧：独立版本（参考）
└── torcherino/    # 旧：独立版本（参考）
```

## License

MIT
