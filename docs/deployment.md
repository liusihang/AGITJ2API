# 部署与配置

[返回 README](../README.md) · [API 参考](api.md) · [登录与续期](authentication.md)

## Docker Compose

从仓库根目录执行，首次启动流程见 [README](../README.md#五分钟启动)。镜像为多阶段构建，运行容器使用非 root 用户，凭据与幂等记录通过 `./data:/app/data` 读写挂载。

Linux / macOS 上，先导出当前用户 ID：

```bash
export LOCAL_UID="$(id -u)" LOCAL_GID="$(id -g)"
```

也可复制 `.env.example` 为 `.env`，将 `LOCAL_UID` / `LOCAL_GID` 改为实际值。不要在 `.env` 中存放 token 或学校 Cookie。Compose 的可调设置如下：

| 变量 | 默认值 | 含义 |
|---|---|---|
| `BIND_ADDR` | `127.0.0.1` | 宿主机发布地址 |
| `PORT` | `8789` | 宿主机发布端口 |
| `LOCAL_UID` | `1000` | 容器运行用户 ID |
| `LOCAL_GID` | `1000` | 容器运行组 ID |

```bash
docker compose logs --tail=50 agitj2api
docker compose restart agitj2api
docker compose down
```

`down` 不删除宿主机 `data/`。服务名由旧版的 `tongji-aigc` 改为 `agitj2api`；旧版 Compose 用户更新后可用 `docker compose up -d --build --remove-orphans` 清理旧服务容器，数据目录保持原位。

临时手动续登：

```bash
docker compose exec agitj2api /app/agitj2api -refresh-sso
```

容器镜像不包含 Python。重新导入 Cookie 在宿主机运行 `python3 import_token.py --sso-cookie`，容器读取同一个挂载目录即可。

## 源码与二进制

```bash
go build -trimpath -o bin/agitj2api .
./bin/agitj2api -init
python3 import_token.py --sso-cookie
./bin/agitj2api
```

也可使用 `make init`、`make run`。默认数据路径相对**进程工作目录**，由 systemd 或其他进程管理器运行时，请指定仓库为工作目录，或使用绝对文件路径：

```bash
./bin/agitj2api \
  -listen 127.0.0.1:8789 \
  -key-file /srv/agitj2api/data/api-key \
  -token-file /srv/agitj2api/data/access-token \
  -sso-cookie-file /srv/agitj2api/data/sso-cookie
```

服务处理 SIGINT / SIGTERM，最多等待 15 秒结束现有 HTTP 请求；长时间等待中的客户端可能断开，已被平台接受的任务仍在执行。更新后使用原幂等键或任务 ID 恢复查询。

## 服务配置项

命令行参数优先于环境变量。环境变量为空时使用默认值；需要明确禁用 SSO 时传 `-sso-cookie-file ""`。

| 参数 | 环境变量 | 默认值 / 行为 |
|---|---|---|
| `-listen` | `LISTEN_ADDR` | 源码 `127.0.0.1:8789`；镜像内 `0.0.0.0:8789` |
| `-key-file` | `AIGC_API_KEY_FILE` | `data/api-key` |
| `-token-file` | `AIGC_TOKEN_FILE` | `data/access-token` |
| `-sso-cookie-file` | `AIGC_SSO_COOKIE_FILE` | `data/sso-cookie` |
| `-sso-refresh-interval` | 无 | `30m`；不得少于 `1m` |
| `-init` | 无 | 初始化代理 Key 后退出，不需要上游凭据 |
| `-refresh-sso` | 无 | 执行一次 SSO 续登后退出 |
| `-upstream` | 无 | `https://agi.tongji.edu.cn`；供本地模拟测试使用 |

为了兼容已有部署，环境变量继续使用 `AIGC_*` 名称。代理 Key 在启动时读取，轮换后需要重启；上游 token 与 Cookie 会重新读取，更新不需要重启。

## 内网和 HTTPS 入口

源码运行时通过 `-listen 0.0.0.0:8789` 允许其他机器连接；Compose 使用 `.env` 中的 `BIND_ADDR` 控制宿主机发布地址。请根据实际需要绑定具体内网地址，或保留回环地址并使用现有 HTTPS 反向代理。

HTTPS 入口需要转发 `Authorization`、`Idempotency-Key` 和 `Range`，允许足够大的上传请求，并为图片生成 / 语音识别设置足够长的响应超时。建议请求体上限至少 `120 MiB`，响应等待至少 `1000s`；这些是入口配置建议，具体音频和参考图限制仍由 API 校验。

服务没有浏览器跨域访问支持。跨域网页调用请通过自己的后端转发，避免把共享代理 Key 暴露给网页访问者。

## 运行数据与更新

| 路径 | 内容 | 是否可重新生成 |
|---|---|---|
| `data/api-key` | 调用方使用的代理 Key | 可以轮换，但调用方需同步修改 |
| `data/sso-cookie` | 学校统一认证会话 | 失效后需重新登录取得 |
| `data/access-token` | AGI 平台网页登录 token | 有效 SSO 会话可重新获取 |
| `data/compat-submissions/` | 兼容接口请求摘要、任务 / 历史标识 | 应保留，用于避免重复提交和恢复任务 |

`data/` 应由运行服务的用户拥有；目录建议 `700`，凭据与幂等记录文件为 `600`。备份时将整个目录作为敏感数据处理，不要提交到 Git 或上传到公开存储。

更新 Docker 部署：

```bash
git pull --ff-only
export LOCAL_UID="$(id -u)" LOCAL_GID="$(id -g)"
docker compose up -d --build --remove-orphans
```

源码部署更新后重新编译并重启进程。保留 `data/`，不要重复创建账号凭据或清空幂等记录。更新完成后检查 `/healthz`、`/v1/status` 和带认证的 `/v1/models`。

## 故障排查

| 现象 | 检查与处理 |
|---|---|
| 无法读取 API Key | 先执行 `-init`，检查进程工作目录和 `-key-file` |
| Docker `permission denied` | 检查 `data/` 所有者与 `LOCAL_UID` / `LOCAL_GID` 是否一致 |
| `token_ready=false` | 查看 `token_error` 和 `sso.last_error`，按[认证说明](authentication.md)处理 |
| `login_required=true` | 正常登录学校后重新导入 `_idp_session` |
| 端口已占用 | 检查旧实例，或调整 `-listen` / Compose `PORT` |
| 图片或语音超时 | 同时检查调用方和 HTTPS 入口的等待时间；先核对任务再决定恢复方式 |
| 502 / 网络请求失败 | 检查服务运行环境到 AGI 与学校 IAM 的网络可达性 |

`/healthz` 成功只说明进程在线，不代表平台登录有效。错误和重试细节见 [API 参考](api.md#重试与错误处理)。
