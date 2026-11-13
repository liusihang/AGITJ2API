# 同济 AIGC 内部反代

一个 Go 标准库服务，单账号、固定内部 API Key、自动 SSO 续登、无数据库。支持平台当前的生图、图像编辑、视频、音乐/音效和 3D 工作流。任务由同济平台运行和保存，本服务转发请求及文件流。

接口来自同济平台网页使用的 AIGC 协议；工作流和参数以实时接口为准。音乐/音效工作流包含在内，独立 TTS/ASR 不在本服务范围。官方入口：[AIGC 专区](https://agi.tongji.edu.cn/aigc/image)、[AIGC 说明](https://agi.tongji.edu.cn/docs/aigc-overview)。

## 启动

需要 Go 1.23 或更新版本，导入凭据的小工具使用 Python 3.9 或更新版本。

```bash
cd tongji-aigc-proxy
go build -trimpath -o tongji-aigc-proxy .
./tongji-aigc-proxy -init
python3 import_token.py --sso-cookie
./tongji-aigc-proxy
```

`-init` 生成 `data/api-key`，重复执行保留原密钥。自动续登需要在学校正常登录后取得的 `_idp_session` Cookie：打开 [学校认证帮助页](https://iam.tongji.edu.cn/help/)，在浏览器开发者工具的 Application / Cookies 中选择 `iam.tongji.edu.cn`，复制 `_idp_session` 的值，粘贴给导入工具。输入不会回显，也不应写进命令行或聊天记录。

工具把学校会话保存到 `data/sso-cookie`；服务启动后自动走正常 SSO 链路，取得并验证 AIGC token，再写入 `data/access-token`。可以从没有 access-token 文件的状态直接启动。凭据文件权限均为 `600`，`data/` 已在 Git 和 Docker 构建上下文中排除。

也可只使用网页登录 JWT，手动更新：运行 `python3 import_token.py`，粘贴平台网页 `sessionStorage.getItem('access_token')` 的值。没有学校 Cookie 文件时，服务继续按手动 token 模式运行。

默认只监听本机 `127.0.0.1:8789`。内网其他机器需要访问时，按部署网络设置 `-listen 0.0.0.0:8789`。同一个内部 API Key 的持有者共用这个同济账号、积分和全部 AIGC 任务。服务自身不提供 TLS；跨机器使用时放在已有 HTTPS 入口或受信任的内网连接后面。

```bash
./tongji-aigc-proxy -listen 0.0.0.0:8789
```

可配置项：

| 命令行参数 | 环境变量 | 默认值 |
|---|---|---|
| `-listen` | `LISTEN_ADDR` | `127.0.0.1:8789` |
| `-key-file` | `AIGC_API_KEY_FILE` | `data/api-key` |
| `-token-file` | `AIGC_TOKEN_FILE` | `data/access-token` |
| `-sso-cookie-file` | `AIGC_SSO_COOKIE_FILE` | `data/sso-cookie`；传空字符串可禁用续登 |
| `-sso-refresh-interval` | 无 | `30m`，最短 `1m` |
| `-refresh-sso` | 无 | 执行一次续登后退出 |
| `-upstream` | 无 | `https://agi.tongji.edu.cn`，主要供本地测试 |

除 `/healthz` 外，所有接口都需要 `Authorization: Bearer <内部 API Key>`。内部 Key 不会传到同济；上游只收到平台的登录 token。内部 Key 在启动时读取，修改它需要重启；上游 token 每个请求重读，更新无需重启。

## OpenAI 风格兼容接口

基础地址为 `http://127.0.0.1:8789/v1`，使用同一个内部 API Key。模型 ID 直接使用 `/v1/models` 返回的同济工作流代码，不需要另建模型映射。模型额外的 `type` 字段区分 `t2i`、`i2i`、`t2v`、`i2v` 等能力。

| 方法 | 路径 | 用途 |
|---|---|---|
| GET | `/v1/models`、`/v1/models/{id}` | OpenAI 风格的模型列表和单模型信息 |
| POST | `/v1/images/generations` | 文生图，等待任务完成后返回图片 |
| POST | `/v1/images/edits` | 上传或提供参考图，等待编辑完成后返回图片 |
| POST | `/v1/videos` | 创建视频任务，立即返回 `video_<任务ID>` |
| GET | `/v1/videos/{id}` | 查询视频状态和进度 |
| GET | `/v1/videos/{id}/content` | 下载视频，支持 Range；仅支持 `variant=video` |

图片接口默认返回 `data[].b64_json`；传 `response_format=url` 可取得平台的临时下载地址，当前票据通常约 15 分钟有效。图片生成默认使用 `t2i-krea2-turbo`，编辑默认使用 `qwen-image-2-1-e`。具体费用以使用前的 `/v1/estimate` 和任务最终结算为准。

```python
from openai import OpenAI
from pathlib import Path
import base64
import uuid

client = OpenAI(
    base_url="http://127.0.0.1:8789/v1",
    api_key=Path("data/api-key").read_text().strip(),
    timeout=1000,
    max_retries=0,
)

# 为每次新业务保存一个键；恢复同一次请求时使用原键。
request_key = str(uuid.uuid4())
result = client.images.generate(
    model="t2i-krea2-turbo",
    prompt="一只白色陶瓷杯放在木桌上，柔和晨光",
    size="1024x1024",
    extra_headers={"Idempotency-Key": request_key},
)
Path("image.png").write_bytes(base64.b64decode(result.data[0].b64_json))

with open("reference.png", "rb") as image:
    edited = client.images.edit(
        model="qwen-image-2-1-e", image=image,
        prompt="保持主体，将背景改为浅蓝色",
        extra_headers={"Idempotency-Key": str(uuid.uuid4())},
    )
Path("edited.png").write_bytes(base64.b64decode(edited.data[0].b64_json))
```

参考图支持 multipart 上传、JSON 中的公网 HTTPS `image_url`，或 `data:image/...;base64,...`。单张限 20 MB，最多四张，实际数量由所选工作流决定；为保护服务所在的网络，URL 不访问私有、回环或链路本地地址。平台自身的 `/remote-home/` 签名输出可用作参考图，下载时不携带内部凭据。支持 PNG、JPEG 和 WebP 参考图。平台的工作流可能另有限制。

视频接口沿用历史 Sora 风格的异步结构，供支持该协议的客户端使用。OpenAI 官方 Videos API 已于 2026 年 9 月停止服务；这里的模型和实际任务由同济平台提供。[官方历史接口说明](https://developers.openai.com/api/reference/resources/videos)。

```python
video = client.videos.create(
    model="minimax-h3-t2v", prompt="清晨湖面，薄雾中阳光缓缓升起",
    seconds="5", extra_headers={"Idempotency-Key": str(uuid.uuid4())},
)
print(video.id, video.status)  # 后续查询 client.videos.retrieve(video.id)
# completed 后使用 client.videos.download_content(video.id).write_to_file("video.mp4")
```

图生视频选择 `i2v` 模型并提供 `input_reference.image_url` 或 multipart 参考图；文生视频选择 `t2v` 模型。历史 Sora 客户端的 `seconds` 值与同济模型的可选值不一定一致，请以 `/v1/workflows` 中该模型的 `duration` 为准。

兼容范围以实际工作流能力为准：显式 `size` 必须与模型支持的精确尺寸一致，未指定或 `auto` 使用工作流默认值；视频模型未提供分辨率控制时，显式 `size` 会返回错误，查询结果中的尺寸为空。当前不支持流式图片、mask、OpenAI `file_id`、透明背景或指定非默认质量。不支持的字段或参数返回错误，不会悄悄更改生成要求。

图片接口最多等待 15 分钟。任务状态、文件下载、上游错误和客户端断开不会触发生成 POST 的自动重发。建议客户端**在提交前保存 `Idempotency-Key` 并关闭无键自动重试**：兼容接口会在 `data/compat-submissions/` 保存请求摘要和任务 ID，同一键的相同请求可恢复已接受的任务；同一键对应不同请求、或此前提交结果不明时，会返回 409。未提供键时服务会生成一个并在响应头返回，但客户端连接在收到响应前中断时，不能仅靠这个自动生成的键识别重试。当前幂等记录按单实例设计。

音乐/音效和 3D 继续使用下述原生异步任务接口。语音合成 TTS、语音识别 ASR 尚未接入。

## 推荐调用方式

```bash
export AIGC_BASE=http://127.0.0.1:8789
export AIGC_KEY="$(cat data/api-key)"

# 当前工作流；param_schema 已解析为 JSON 数组。
curl -sS "$AIGC_BASE/v1/workflows" \
  -H "Authorization: Bearer $AIGC_KEY"

# token 状态，不返回实际凭据。
curl -sS "$AIGC_BASE/v1/status" \
  -H "Authorization: Bearer $AIGC_KEY"
```

1. 获取工作流，使用其中的 `code` 和参数 `key`。
2. 需要参考图时先上传，将返回的 `data.path` 放到 `params.image` 等对应字段。
3. 用完整参数调用 `/v1/estimate` 查询积分；服务会过滤出上游估价接受的参数。
4. 保存生成请求和本次幂等键，再调用 `/v1/generations`。
5. 保存返回的 `task_ids`，每隔数秒查询任务；完成后通过本服务下载对应文件。

完整参数可以包含 prompt、image、lyrics 和 seed。便捷接口从实时模板补齐已声明的默认值；没有声明默认值的字段需要调用方提供，尤其不要假定遗漏 seed 会自动随机化。参数值仍须遵守实际工作流的选项、数值和资源约束，上游负责最终校验。

估价示例：

```bash
curl -sS "$AIGC_BASE/v1/estimate" \
  -H "Authorization: Bearer $AIGC_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"template_code":"native-import-53","params":{"prompt":"一只橘猫坐在窗边，柔和晨光","seed":42},"count":1}'
```

提交图片：

```bash
# 先保存键；同一次业务提交保留同一个键。新的生成使用新的键。
python3 -c 'import uuid; print(uuid.uuid4())' > data/request-key

curl -sS "$AIGC_BASE/v1/generations" \
  -H "Authorization: Bearer $AIGC_KEY" \
  -H "Idempotency-Key: $(cat data/request-key)" \
  -H 'Content-Type: application/json' \
  -d '{"template_code":"native-import-53","params":{"prompt":"一只橘猫坐在窗边，柔和晨光","seed":42},"count":1}'
```

`/v1/generations` 依次读取模板、获取新 quote、提交任务，返回 `{"code":200,"message":"任务已提交","data":{"task_id":…, "task_ids":[…]}}`，并回显 `Idempotency-Key` 响应头。也可在请求体中传 `idempotency_key`；若请求头和正文同时传，二者必须相同。键允许 8–128 位字母、数字、连字符或下划线。`count` 缺省为 1，显式传 0 会拒绝。

任务查询与下载：

```bash
TASK_ID=替换成返回的数字ID
curl -sS "$AIGC_BASE/v1/tasks/$TASK_ID" \
  -H "Authorization: Bearer $AIGC_KEY"

curl -fS "$AIGC_BASE/v1/tasks/$TASK_ID/outputs/0" \
  -H "Authorization: Bearer $AIGC_KEY" -o result.bin

# 一个任务有多个文件时，也可下载 ZIP。
curl -fS "$AIGC_BASE/v1/tasks/$TASK_ID/archive" \
  -H "Authorization: Bearer $AIGC_KEY" -o outputs.zip
```

文件后缀参考任务 `outputs` 中的 `filename/mime`。下载使用实时认证，因此自己的 `/v1/tasks/{id}/outputs/{index}` 路径可以重复访问；上游 token 有效且文件仍保留时都可下载。任务 JSON 中上游原有的媒体 URL 保持原样，它们的 ticket 通常约 15 分钟到期。

参考图上传：

```bash
curl -sS "$AIGC_BASE/v1/inputs" \
  -H "Authorization: Bearer $AIGC_KEY" \
  -F image=@/绝对路径/reference.png
```

图片、视频、音频、3D 仅需换 `template_code` 和 `params`，使用相同提交方式。例如：

| 用途 | 工作流示例 | params 示例 |
|---|---|---|
| 文生图 | `native-import-53` | `{"prompt":"…","ratio":"1:1 (4096x4096)","seed":42}` |
| 文生视频 | `minimax-h3-t2v` | `{"prompt":"…","duration":"5秒","seed":42}` |
| 音乐/音效 | `stable-audio` | `{"prompt":"舒缓钢琴纯音乐","duration":30,"seed":42}` |
| 文生 3D | `native-import-25` | `{"prompt":"卡通橘猫，白色背景","seed":42}` |

表中其余参数由模板默认值补齐。使用前读取实时模板；不同视频工作流的 duration 类型并不一致。当前音频和 3D 的 `count` 必须为 1。图片/视频 quote 中的 0 不代表免费，费用显示使用 estimate，最终结算看任务 billing。

## 全部接口

原生反代保留 `/api/v1/aigc/...` 路径、JSON 结构、HTTP 状态和错误字段，不改写生成结果内容。常用路径另有 `/v1/...` 别名：

| 方法 | 内部路径 | 行为 |
|---|---|---|
| GET | `/healthz` | 进程存活检查；公开，不检测上游 |
| GET | `/v1/status` | 凭据到期时间、SSO 续登状态与错误；本地读取，不触发网络请求 |
| GET | `/v1/workflows` | 实时模板，解析 param_schema |
| POST | `/v1/estimate` | 接受完整 params，补默认值、过滤估价参数 |
| POST | `/v1/generations` | 自动 quote 和提交，必须提供幂等键 |
| GET | `/v1/templates`、`/v1/templates/timing` | 原始模板和耗时统计 |
| GET | `/v1/health`、`/v1/resource-quota` | 上游健康与账号资源配额 |
| POST | `/v1/inputs` | multipart 参考图上传 |
| POST | `/v1/quote` | 原生完整参数报价 |
| GET/POST | `/v1/tasks` | 原生列表/提交；原生提交自行提供 quote_token、idempotency_key |
| GET | `/v1/tasks/{id}`、`/v1/tasks/{id}/details` | 状态和参数 |
| GET | `/v1/tasks/{id}/inputs/{index}`、`/v1/tasks/{id}/outputs/{index}` | 流式下载，支持 Range/206 |
| GET | `/v1/tasks/{id}/archive` | 多文件归档 |
| POST | `/v1/tasks/{id}/cancel` | 取消，保留上游 409 等响应 |
| POST | `/v1/media-ticket` | 为平台媒体 URL 换临时票据 |
| POST | `/v1/prompt/optimize` | 原生提示词增强，具体可用性和收费取决于上游 |

列表的 query 参数原样透传。`/api/v1/aigc/estimate` 接受的是上游原生过滤参数，与便捷 `/v1/estimate` 的完整参数处理方式不同。其他列出的原生路径均可用 `/api/v1/aigc/...` 形式访问。

保留上游的 `error_code/retryable/request_id`、`Retry-After`、文件 MIME、Content-Disposition、Content-Range 和 Accept-Ranges。JSON 控制请求等待上游最多 90 秒；文件使用流式转发，不设置整个下载时限。客户端断开会取消相应 HTTP 转发，已接受的生成任务仍在平台执行。

## token 更新与超时处理

网页登录 JWT 的当前签发有效期为 24 小时。服务使用学校 `_idp_session` Cookie 走正常 SSO 跳转链取得新 JWT，不保存学校密码，也不需要常驻浏览器。

自动行为：

- 有 Cookie 文件时，启动立即续登，此后默认每 30 分钟续登一次。该间隔是可调整的运行策略，尚未证明它能延长学校服务端会话。
- 请求发现 token 缺失、过期、不足一小时，或已被上游 401 拒绝时，尝试续登。并发请求合并为一次续登。
- 每个新 token 都先调用 AIGC 配额接口验证，成功后以 `600` 权限原子替换文件；若学校轮换了会话 Cookie，也保存新的值。
- 续登失败时保留原文件。原 token 仍有效且没有被上游拒绝时，继续提供服务；否则返回 503 `UPSTREAM_TOKEN_UNAVAILABLE`。
- 上游 401 原样返回，当次请求不会重发；下一次请求会尝试续登。生成 POST 始终不会自动重试。
- 同一 Cookie 续登失败后有一分钟冷却，避免请求集中触发登录；导入不同 Cookie 后可立即重试。

学校会话失效后，正常重新登录，再运行 `python3 import_token.py --sso-cookie`。无需重启，等下一次定时检查或使用下面的命令立即续登：

```bash
./tongji-aigc-proxy -refresh-sso
```

内部 API Key 和已有任务 ID 保持不变。`_idp_session` 是学校统一认证凭据，只保存在服务端，不发送给反代调用方或 AIGC API；只在学校 IAM 的 `/idp` 路径使用。SSO 跳转限定为预期的 HTTPS 站点，并保持证书验证。

查看状态：

```json
{
  "code": 200,
  "data": {
    "token_ready": true,
    "token_expires_at": "2026-11-14T13:05:27Z",
    "automatic_sso_refresh": true,
    "sso": {
      "enabled": true,
      "refresh_interval_seconds": 1800,
      "last_attempt_at": "2026-11-13T13:05:26Z",
      "last_success_at": "2026-11-13T13:05:27Z",
      "last_error": "",
      "login_required": false
    }
  }
}
```

`last_success_at` 表明本次进程已经成功续登；`login_required=true` 表明学校要求重新进行正常登录。`token_ready` 只说明本地 token 文件与已知有效期状态，不代替服务端吊销检查。`/healthz` 仅用于进程存活检查。

学校会话的空闲期限和绝对最长寿命仍未知，不能保证一次登录后永久免人工。无到期日期的模型 API Key 也不能替代当前 AIGC JWT：通过校园网络验证时，模型列表成功，AIGC 路由仍拒绝它。

只读 GET 若在收到响应头前遇到 EOF，会重新连接重试一次；已经开始传输的文件流不会重新发送。服务不会自动重试生成提交，也不维护额外任务队列或去重数据库。幂等键会原样传到上游，但调查尚未验证服务端去重时长与全部失败分支。因此遇到 502、超时或连接断开，先查询 `/v1/tasks` 核对该次请求是否已产生任务；不要直接换一个键再次提交。调用方应在发送前保存请求体和幂等键。

## 容器运行

```bash
./tongji-aigc-proxy -init
python3 import_token.py --sso-cookie
LOCAL_UID="$(id -u)" LOCAL_GID="$(id -g)" docker compose up -d --build
```

Compose 默认仍只向本机发布 8789 端口。运行 UID/GID 与本机保持一致以读取 `600` 权限文件；data 目录读写挂载，以便容器保存更新的 token 与轮换后的 Cookie；宿主机重新导入 Cookie 后，容器也可以直接读取。修改内网监听入口时同步调整 Compose 的端口绑定。

## 验证

```bash
go test -race ./...
go vet ./...
```

回归测试覆盖鉴权和接口范围、凭据热更新和过期、估价参数过滤与大整数保留、报价后提交、报价失败停止提交、只读 GET 的 EOF 恢复、提交连接断开不自动重试、上传内容、Range 文件下载及上游错误透传。

SSO 回归还覆盖无 token 引导、并发续登合并、会话失效与旧 token 保留、Cookie 更新和轮换、禁止不可信站点与 HTTP 降级跳转、HTTPS 默认 443 端口等价、后台定期执行和退出。

2026-11-13 本机联调：31/31 工作流估价成功；四类代表报价、参考图上传、已有音视频下载及分段读取通过；新图像成功生成并下载，完整解码通过，实际费用 0.1 积分。SSO 通过普通 HTTP、仅凭学校会话取得新 JWT，并通过 AIGC 配额验证。

本项目不上传运行数据、Cookie、token、内部 Key、生成结果或测试账号记录。本机日志和进程信息可自行保存到 `data/`。容器配置已提供；当前验证环境没有 Docker CLI，尚未进行容器构建和运行验证。
