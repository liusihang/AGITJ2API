# 贡献指南

欢迎提交可复现的问题和范围明确的改进。项目以简单、可部署的单账号网关为目标，Go 服务保持标准库实现，不要求数据库或外部队列。

## 本地开发

需要 Go 1.23+ 和 Python 3.9+。从仓库根目录运行：

```bash
make build
make check
```

没有 Make 时，可直接运行：

```bash
go build -trimpath -o bin/agitj2api .
go test -race -count=1 ./...
go vet ./...
python3 -m compileall -q import_token.py examples scripts
```

Go 测试使用本地模拟上游，不需要真实账号，不访问学校平台、不提交付费任务。Python SDK 仅用于调用示例，安装方式见 [examples/README.md](examples/README.md)。CI 另在 Docker 中验证首次初始化、启动和鉴权。

## 代码结构

| 文件 | 职责 |
|---|---|
| `main.go` | 路由白名单、代理鉴权、原生 AIGC 转发和任务便捷接口 |
| `compat.go` | OpenAI 风格模型、图片、视频适配和幂等记录 |
| `speech.go` | 语音识别模型发现、同步 / 异步转写和结果恢复 |
| `sso.go` | 受限 SSO 跳转、凭据更新和续登状态 |
| `*_test.go` | 对应模块的模拟上游与回归测试 |
| `import_token.py` | 交互导入凭据 |

## 提交改动

- Go 代码使用 `gofmt`。行为变化同时更新相关测试、API 说明和调用示例。
- 新增平台能力前核对实际请求、模型能力和错误结构；不要仅凭页面名称假定协议相同。
- 不自动重试生成 POST。不确定提交结果时保留标识，避免重复调用或扣费。
- 保持路由白名单，避免将服务扩展为任意带认证的上游代理。
- 不提交 `data/`、`.env`、日志、音视频结果、登录 token、学校 Cookie 或签名下载 URL。

PR 中说明问题、改动后的行为和验证方式。需要真实账号的联调应由使用者自愿在自己的环境执行；请明确是否创建了新任务，以及是否消耗资源。

报告问题时提供系统、Go / Docker 版本、接口、HTTP 状态、已脱敏的最小请求和错误信息。凭据泄露或其他安全问题请按 [SECURITY.md](SECURITY.md) 私下报告。

安装 Gitleaks 后，可运行 `gitleaks git --redact --log-opts=--all` 扫描历史。仓库配置只排除了测试文件中一个明确的虚构 Key，不会跳过整个测试目录。
