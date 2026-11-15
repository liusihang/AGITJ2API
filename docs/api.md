# API 参考

[返回 README](../README.md) · [部署配置](deployment.md) · [登录与续期](authentication.md)

除 `/healthz` 外，所有请求需要 `Authorization: Bearer <代理 API Key>`。兼容接口返回 OpenAI 风格对象和 `error`；原生接口保留平台的 `code/message/data` 封装。本文的模型代码仅作示例，可用模型与参数请读取实时目录。

## OpenAI 风格接口

基础地址为 `http://127.0.0.1:8789/v1`，使用同一个代理 API Key。模型 ID 直接使用 `/v1/models` 返回的同济工作流代码，不需要另建模型映射。模型额外的 `type` 字段区分 `t2i`、`i2i`、`t2v`、`i2v` 等能力。

| 方法 | 路径 | 用途 |
|---|---|---|
| GET | `/v1/models`、`/v1/models/{id}` | OpenAI 风格的模型列表和单模型信息 |
| POST | `/v1/images/generations` | 文生图，等待任务完成后返回图片 |
| POST | `/v1/images/edits` | 上传或提供参考图，等待编辑完成后返回图片 |
| POST | `/v1/audio/transcriptions` | 上传音频并返回识别文字 |
| POST | `/v1/videos` | 创建视频任务，立即返回 `video_<任务ID>` |
| GET | `/v1/videos/{id}` | 查询视频状态和进度 |
| GET | `/v1/videos/{id}/content` | 下载视频，支持 Range；仅支持 `variant=video` |

### 图片生成与编辑

图片接口默认返回 `data[].b64_json`；传 `response_format=url` 可取得平台的临时下载地址，调查时票据通常约 15 分钟有效。图片生成默认使用 `t2i-krea2-turbo`，编辑默认使用 `qwen-image-2-1-e`。具体费用以使用前的 `/v1/estimate` 和任务最终结算为准。

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

参考图支持 multipart 上传、JSON 中的公网 HTTPS `image_url`，或 `data:image/...;base64,...`。单张限 20 MiB，最多四张，实际数量由所选工作流决定；为保护服务所在的网络，URL 不访问私有、回环或链路本地地址。平台自身的 `/remote-home/` 签名输出可用作参考图，下载时不携带代理凭据。支持 PNG、JPEG 和 WebP 参考图。平台的工作流可能另有限制。

### 视频生成

视频接口采用 Videos 异步任务结构，供支持该协议的客户端使用。模型和实际任务由同济平台提供；兼容范围以本项目实现和客户端版本为准。

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

### 语音识别

文件语音识别支持同济的 `Whisper` 和 `Paraformer`，也可使用常见模型名 `whisper-1` 作为 `Whisper` 的别名。Whisper 支持不指定语言自动识别；Paraformer 当前配置为中文，传入 `language="zh"` 会转换为平台要求的中文语言参数。模型以 `/v1/models` 的实时结果为准。

```python
# 每个新请求先保存唯一键；重试同一请求时使用原键。
speech_key = str(uuid.uuid4())
with open("speech.wav", "rb") as audio:
    transcript = client.audio.transcriptions.create(
        model="Whisper", file=audio, language="zh",
        extra_headers={"Idempotency-Key": speech_key},
    )
print(transcript.text)
```

识别接口接受 WAV、MP3，单文件最多 30 MiB；支持 `response_format=json` 和 `text`。Paraformer 返回分段信息时，JSON 结果额外包含 `segments`；Whisper 当前不返回分段。提示词、流式输出、字幕和 `verbose_json` 暂不支持。同步识别等待上游响应最多约 5 分钟，整个请求最多 6 分钟；异步任务最多等待 10 分钟。

语音识别也支持上述 `Idempotency-Key`：同一键可恢复已提交的任务，结果可在平台识别历史保留期内取回；无法确认是否提交时返回错误，不自动重发。代理只保存请求摘要和平台任务或历史 ID，不保存音频和转写文本；平台自身按其历史保留规则保存结果。

音乐/音效和 3D 继续使用下述原生异步任务接口。语音合成使用下述原生语音任务接口，尚不提供 OpenAI `/v1/audio/speech` 适配。

### 语音合成（原生接口）

使用相同代理 Key 调用以下白名单路由；模型、服务、音色和格式以实时目录为准：

| 方法 | 路径 | 用途 |
|---|---|---|
| GET | `/api/v1/chat/models?model_type=speech` | 语音模型及 `advanced_config.text_to_speech` |
| GET | `/api/v1/chat/services?model_asset_id={id}` | 模型当前可用的服务 |
| POST | `/api/v1/speech/tasks` | 提交语音任务 |
| POST | `/api/v1/speech/tasks/query` | 查询已提交任务及结果 |

CosyVoice 提交示例（`service_id` 从实时服务列表取得）：

```json
{"kind":"tts","service_id":123,"text":"你好，这是语音测试。","voice_type":"可爱女生","language":"中文","encoding":"mp3","speed":1.0}
```

提交返回 `task_id` 和 `history_id`。保存这两个值，以 `kind: "tts"`、相同 `service_id`、`task_id`、`history_id` 查询任务；成功时 `audio_data` 是 Base64 音频，`encoding` 标明实际格式。原生路由直接转发平台响应，不提供本地幂等记录。提交超时或断开时不要自动重发，应先核对平台语音历史，避免创建重复任务。

## 原生工作流调用

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

## 原生接口清单

原生反代保留 `/api/v1/aigc/...` 路径、JSON 结构、HTTP 状态和错误字段，不改写生成结果内容。常用路径另有 `/v1/...` 别名：

| 方法 | 代理路径 | 行为 |
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

## 重试与错误处理

| 状态 / 现象 | 建议处理 |
|---|---|
| `401 invalid_api_key` / `INVALID_API_KEY` | 使用代理 `data/api-key`，不要传上游网页登录 token |
| `503 upstream_token_unavailable` / `UPSTREAM_TOKEN_UNAVAILABLE` | 查看 `/v1/status`，按[认证说明](authentication.md)更新凭据 |
| 上游 `401` | 当前请求不自动重发；下一次请求触发续登检查 |
| `400` / `413` | 按返回的字段错误调整参数、格式或文件大小 |
| `409` | 可能是请求仍在提交、提交结果不明、同键不同参数，或视频未完成；按错误信息核对任务 |
| `429` | 尊重上游 `Retry-After`，降低请求频率 |
| `502`、超时、连接中断 | 先查平台任务；有幂等记录时使用原键和原请求恢复，避免换键重复扣费 |

兼容接口中，`data/compat-submissions/` 的记录用于恢复图片、视频和语音请求。不要把该目录当作可随意清理的缓存。原生 `/v1/generations` 会将幂等键传给平台，不提供同样的本地恢复记录；原生提交的去重有效期由上游决定。

模型发现、状态、估价和配额检查不会提交生成任务；生成、编辑、转写和其他推理请求可能消耗账号资源。具体用量、费用和可用性以平台为准。
