# 登录与自动续期

[返回 README](../README.md) · [部署配置](deployment.md) · [API 参考](api.md)

## 三种凭据各自做什么

| 凭据 | 保存位置 | 用途 |
|---|---|---|
| 代理 API Key | `data/api-key` | 调用方访问 AGITJ2API；不会传给同济平台 |
| 平台 access token | `data/access-token` | AGITJ2API 访问 AGI 平台 |
| 学校 `_idp_session` Cookie | `data/sso-cookie` | 在学校会话有效时，通过正常 SSO 链路取得新 access token |

学校 Cookie 属于统一认证会话，只保存在服务端，仅在预期 IAM 站点的 `/idp` 路径使用，不发送给 API 调用方或 AGI 业务接口。服务不保存学校账号密码，不需要常驻浏览器。

## 推荐：自动 SSO 续登

1. 在浏览器正常登录 AGI 平台。
2. 打开 [学校认证帮助页](https://iam.tongji.edu.cn/help/)，从开发者工具的 Cookies 中复制 `iam.tongji.edu.cn` 下 `_idp_session` 的 Value。
3. 在仓库目录运行：

```bash
python3 import_token.py --sso-cookie
```

输入不会回显，导入工具以 `600` 权限原子替换文件。也可通过 `--file /absolute/path/sso-cookie` 指定服务端实际使用的位置。不要把值直接作为命令行参数传递。

服务可以从没有 `access-token` 文件的状态启动。它使用 Cookie 完成 SSO 跳转，先通过 AIGC 配额接口验证新 token，再写入文件；学校轮换了 Cookie 时也会保存新值。

续登在以下情形触发：

- 服务启动时，以及之后默认每 `30m` 一次。
- 请求发现 token 缺失、过期、不足一小时，或已被上游 `401` 拒绝时。
- 手动运行 `./bin/agitj2api -refresh-sso`；Docker 方式见[部署说明](deployment.md)。

并发触发会合并为一次续登。同一 Cookie 续登失败后有一分钟冷却；导入不同 Cookie 可立即重试。续登失败时保留原 token；若它仍有效且未被上游拒绝，服务继续使用它。

**自动续登不能保证永久免登录。** 调查时平台 JWT 声明有效期为 24 小时；学校会话的空闲期限和最长寿命未得到确认。定期续登是运行策略，未证明它能延长学校服务端会话。

## 备选：手动导入平台 token

在已登录的 AGI 网页开发者工具 Console 中读取：

```javascript
sessionStorage.getItem('access_token')
```

在本机导入，随后粘贴 token：

```bash
python3 import_token.py
```

工具会检查 JWT 格式及到期时间。没有 Cookie 文件时，服务按手动 token 模式工作；平台 token 到期后需要重新正常登录并导入。已有 Cookie 文件但希望明确禁用自动续登时，启动参数使用 `-sso-cookie-file ""`。

上游 token 每次请求重新读取，因此重新导入无需重启。模型服务的其他 API Key 不应直接当作 AIGC 网页 token 使用。

## 如何判断登录是否有效

```bash
export OPENAI_API_KEY="$(cat data/api-key)"
curl -fsS http://127.0.0.1:8789/v1/status \
  -H "Authorization: Bearer $OPENAI_API_KEY"
curl -fsS http://127.0.0.1:8789/v1/resource-quota \
  -H "Authorization: Bearer $OPENAI_API_KEY"
```

状态接口只读取本地状态，不触发网络请求，也不返回真实凭据：

| 字段 | 含义 |
|---|---|
| `token_ready` | 本地存在可读、未到期且未被本进程标记拒绝的 token |
| `token_expires_at` | 从 JWT 解析的到期时间，若可解析 |
| `automatic_sso_refresh` / `sso.enabled` | 当前存在 Cookie 文件；不表示一定能成功登录 |
| `sso.last_success_at` | 本次进程最近一次成功续登的时间，UTC |
| `sso.last_error` | 最近一次续登错误 |
| `sso.login_required` | 学校要求重新正常登录 |

`token_ready=true` 不代替服务端吊销检查。配额或模型目录成功返回，才验证了本次上游认证；`/healthz` 仅检查进程。

## 学校会话失效后

正常重新登录，再执行 `python3 import_token.py --sso-cookie`。服务下次定时检查或业务请求会尝试续登；也可手动执行 `-refresh-sso` 立即更新。代理 API Key、已有任务和幂等记录保持不变。

上游 `401` 不会导致当前 POST 自动重发。收到错误或超时后，先核对任务状态；恢复原业务时使用原幂等键，防止重复调用。
