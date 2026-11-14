# 调用示例

[返回 README](../README.md) · [完整 API 参考](../docs/api.md)

在仓库根目录运行示例，需要 Python 3.10+、已启动且已登录的 AGITJ2API，以及 OpenAI Python SDK：

```bash
python3 -m venv .venv
source .venv/bin/activate
python3 -m pip install -r examples/requirements.txt
export OPENAI_BASE_URL=http://127.0.0.1:8789/v1
export OPENAI_API_KEY="$(cat data/api-key)"
```

`OPENAI_API_KEY` 是代理 Key。远程调用者通过安全渠道获得该 Key，连接自己部署的 HTTPS 地址，不需要学校 Cookie。

这些脚本会实际调用平台并可能消耗资源。`--request-id` 用作幂等键，允许 8–128 位字母、数字、连字符和下划线。首次请求前保存它和参数；同一业务恢复时复用原值，每个新业务使用新值。

## 图片生成与编辑

```bash
python3 examples/images.py \
  --prompt "一只橘猫坐在窗边，柔和晨光" \
  --request-id image-demo-001 --output image.png

python3 examples/images.py \
  --image reference.png --prompt "保持主体，将背景改为浅蓝色" \
  --request-id image-edit-001 --output edited.png
```

默认生成模型 `t2i-krea2-turbo`，默认编辑模型 `qwen-image-2-1-e`；可用 `--model` 替换。`--size` 必须是模型实际支持的精确尺寸，省略时使用其默认值。示例显式请求 PNG 输出。

## 语音识别

```bash
# Whisper：省略语言，由模型自动识别。
python3 examples/transcribe.py speech.wav --request-id speech-demo-001

# Paraformer：当前为中文模型。
python3 examples/transcribe.py speech.mp3 \
  --model Paraformer --language zh --request-id speech-demo-002
```

支持 WAV / MP3，单文件不超过 30 MiB。`--format text` 可取得纯文本；默认 JSON 返回的 `text` 同样直接打印到终端。完整分段信息可使用 SDK 返回对象的 `model_dump()` 查看。

## 视频生成

```bash
python3 examples/video.py \
  --prompt "清晨湖面，薄雾中阳光缓缓升起" \
  --seconds 5 --request-id video-demo-001 --output video.mp4
```

脚本先打印任务 ID，然后轮询最多 20 分钟，完成后下载视频。中断脚本不会取消平台任务；用相同 `--request-id` 和原参数再次执行可恢复。也可以通过 `/v1/videos/{id}` 单独查询，再通过 `/v1/videos/{id}/content` 下载。

模型 ID 和时长取值以 `/v1/workflows` 为准。图生视频、原生音乐 / 3D 工作流及 cURL 示例见 [API 参考](../docs/api.md)。
