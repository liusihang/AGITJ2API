# AGITJ2API

[![CI](https://github.com/liusihang/AGITJ2API/actions/workflows/ci.yml/badge.svg)](https://github.com/liusihang/AGITJ2API/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)
[![Go 1.23+](https://img.shields.io/badge/Go-1.23%2B-00ADD8.svg)](go.mod)

**将同济大学 AGI 平台的图片、视频、语音识别、音乐与 3D 能力接入自己的应用。**

AGITJ2API 是一个轻量 API 网关：用 OpenAI 风格接口调用图片、视频和语音识别，用统一的异步任务接口调用音乐、音效和 3D 工作流。服务使用 Go 标准库，无数据库、无额外服务依赖，支持学校 SSO 自动续登。

A lightweight Go gateway for Tongji University's AGI platform, with OpenAI-style image, video and transcription APIs, native music/3D workflows, and SSO session renewal.

本项目为社区项目，与同济大学及 OpenAI 无隶属关系。使用者需要自己的有效平台账号和相应权限；本项目提供接口适配，模型、任务、积分和结果由[同济 AGI 平台](https://agi.tongji.edu.cn)管理。

## 支持哪些能力

| 能力 | 接口 | 当前范围 |
|---|---|---|
| 模型发现 | `GET /v1/models` | 实时工作流与语音识别模型 |
| 文生图 | `POST /v1/images/generations` | JSON 请求，返回 Base64 或临时 URL |
| 图像编辑 | `POST /v1/images/edits` | 上传参考图或提供图片 URL |
| 视频生成 | `POST /v1/videos` | 异步创建、查询、下载，支持 Range |
| 语音识别 | `POST /v1/audio/transcriptions` | Whisper / Paraformer，WAV / MP3 |
| 音乐、音效、3D | `POST /v1/generations` | 平台原生异步工作流 |
| 登录维护 | `GET /v1/status` | SSO 续登状态、token 到期时间 |

图片、视频的 `model` 使用实时目录中的工作流代码。语音识别支持 `Whisper`、`Paraformer`，`whisper-1` 是同济 `Whisper` 的别名。模型可用性由账号权限和平台目录决定。

当前兼容范围详见 [API 参考](docs/api.md)。独立语音合成 TTS、聊天补全、Responses、实时语音、流式图片、mask 和 OpenAI Files API 尚未接入。音乐生成通过原生工作流调用。

## 五分钟启动

可选 Docker Compose 或源码运行。首次配置需要 Python 3.9+，用于交互导入凭据；服务运行时不依赖 Python。

### 方式一：Docker Compose

需要 Docker Engine / Docker Desktop 及 Compose 插件。下面的命令适用于 macOS / Linux shell，无需在宿主机安装 Go。

```bash
git clone https://github.com/liusihang/AGITJ2API.git
cd AGITJ2API

# data 必须由当前用户创建，确保容器可读写 600 权限的凭据。
mkdir -p data
chmod 700 data
export LOCAL_UID="$(id -u)" LOCAL_GID="$(id -g)"

docker compose build
docker compose run --rm --no-deps agitj2api -init
python3 import_token.py --sso-cookie
docker compose up -d
```

导入工具会提示粘贴学校 `_idp_session` Cookie，输入不回显。获取方法见下一节。默认只向本机发布 `127.0.0.1:8789`，所有运行数据写入 `./data/`。

### 方式二：源码运行

需要 Go 1.23+ 和 Python 3.9+。

```bash
git clone https://github.com/liusihang/AGITJ2API.git
cd AGITJ2API

go build -trimpath -o bin/agitj2api .
./bin/agitj2api -init
python3 import_token.py --sso-cookie
./bin/agitj2api
```

`-init` 只生成代理 API Key，不会覆盖已有密钥。服务启动后用学校会话获取并验证平台 token，后续按需及定时续登。

### 获取登录凭据

1. 在浏览器正常登录[同济 AGI 平台](https://agi.tongji.edu.cn)。
2. 打开[学校认证帮助页](https://iam.tongji.edu.cn/help/)，在开发者工具的 **Application / Storage → Cookies → `iam.tongji.edu.cn`** 中找到 `_idp_session`。
3. 复制它的 **Value**，粘贴到 `python3 import_token.py --sso-cookie` 的交互提示中。

Cookie 保存为 `data/sso-cookie`，平台 token 保存为 `data/access-token`，文件权限为 `600`。不要把学校 Cookie 发给 API 调用方，也不要写进仓库、Issue 或截图。

如果暂时不配置自动续登，也可运行 `python3 import_token.py` 导入 AGI 网页的 access token。两种方式、失效处理和续期边界见[登录与自动续期](docs/authentication.md)。

### 检查服务与登录

在另一个终端进入仓库目录：

```bash
export AGITJ2API_URL=http://127.0.0.1:8789
export OPENAI_BASE_URL="$AGITJ2API_URL/v1"
export OPENAI_API_KEY="$(cat data/api-key)"

curl -fsS "$AGITJ2API_URL/healthz"
curl -fsS "$AGITJ2API_URL/v1/status" \
  -H "Authorization: Bearer $OPENAI_API_KEY"
curl -fsS "$AGITJ2API_URL/v1/models" \
  -H "Authorization: Bearer $OPENAI_API_KEY"
```

`/healthz` 只检查进程存活；`/v1/status` 显示本地凭据与续登状态；`/v1/models` 成功返回模型目录，才说明本次请求已通过上游认证。首次 SSO 可能需要稍候。

## 用 OpenAI Python SDK 调用

SDK 示例需要 Python 3.10+，安装项目联调使用的版本：

```bash
python3 -m venv .venv
source .venv/bin/activate
python3 -m pip install -r examples/requirements.txt
```

以下示例使用上一节的 `OPENAI_BASE_URL` 和 `OPENAI_API_KEY`。生成与识别可能消耗账号资源，模型与尺寸请先查询 `/v1/models`、`/v1/workflows`；估价及最终计费见 [API 参考](docs/api.md)。

```python
import base64
from pathlib import Path
from openai import OpenAI

client = OpenAI(timeout=1000, max_retries=0)

# 每个新业务分配唯一键并保存；重试同一次请求时复用原键和原参数。
request_key = "my-image-request-001"
result = client.images.generate(
    model="t2i-krea2-turbo",
    prompt="一只白色陶瓷杯放在木桌上，柔和晨光",
    size="1024x1024",
    extra_headers={"Idempotency-Key": request_key},
)
Path("image.png").write_bytes(base64.b64decode(result.data[0].b64_json))
```

语音识别：

```python
with open("speech.wav", "rb") as audio:
    transcript = client.audio.transcriptions.create(
        model="Whisper",
        file=audio,
        language="zh",
        extra_headers={"Idempotency-Key": "my-speech-request-001"},
    )
print(transcript.text)
```

更多可运行示例位于 [examples/](examples/README.md)：

```bash
python3 examples/images.py --prompt "一只橘猫坐在窗边" --request-id image-demo-001
python3 examples/transcribe.py speech.wav --request-id speech-demo-001
python3 examples/video.py --prompt "清晨湖面，薄雾缓缓散去" --request-id video-demo-001
```

每次新的生成使用新的 `--request-id`。调用失败或中断后，保留原键和原参数恢复；不要直接换键重发。兼容接口会在 `data/compat-submissions/` 保存请求摘要及平台任务标识。原生任务接口的幂等处理及恢复方式见 [API 参考](docs/api.md#重试与错误处理)。

## 部署与使用边界

服务按**单账号、单实例、共享代理 API Key**设计，适合个人及受信任的小团队。所有持有该 Key 的调用方共用同一平台账号的额度、任务和历史记录。服务没有用户隔离、独立计费或速率限制。

默认仅监听本机，跨机器访问请配置 HTTPS 入口或受信任的内网连接。服务自身不提供 TLS；代理 Key、学校 Cookie、平台 token 均为敏感凭据。`data/`、`.env` 和本地构建产物已排除出 Git，Docker 构建上下文只允许源码、模块声明和许可证。

自动续登依赖学校 SSO 会话仍有效。学校要求重新认证时，需要正常登录并重新导入 Cookie；目前不能保证一次登录后永久使用。上游模型和网页协议可能变化，遇到异常请先检查 `/v1/status` 与实时目录。

| 文档 | 内容 |
|---|---|
| [API 参考](docs/api.md) | 全部端点、参数、限制、幂等与错误处理 |
| [部署配置](docs/deployment.md) | Docker、源码、配置项、更新与数据维护 |
| [登录与自动续期](docs/authentication.md) | 两种凭据模式、续登机制、失效恢复 |
| [调用示例](examples/README.md) | 图片、编辑、视频、语音识别 |
| [贡献指南](CONTRIBUTING.md) | 开发、测试和提交范围 |
| [安全说明](SECURITY.md) | 凭据保护与漏洞报告 |

## 开发

```bash
make build
make check
```

`make check` 运行 Go 竞态测试、`go vet` 和 Python 语法检查，无需真实平台账号，不提交付费任务。GitHub Actions 另会验证 Docker Compose 的构建、密钥初始化、启动与鉴权。

核心代码保持在仓库根目录：`main.go` 负责原生网关，`compat.go` 负责图片与视频适配，`speech.go` 负责语音识别，`sso.go` 负责学校认证。运行数据统一放在 `data/`。

## 许可证

[MIT License](LICENSE)。该许可证适用于本仓库代码；平台服务、模型和生成内容的使用遵循其各自规则。
