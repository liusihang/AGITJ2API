#!/usr/bin/env python3
"""Import a web JWT or school SSO cookie without echoing it."""
import argparse
import base64
from datetime import datetime, timezone
import getpass
import json
import os
from pathlib import Path
import sys
import tempfile


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--file")
    parser.add_argument("--sso-cookie", action="store_true", help="导入学校 _idp_session，启用自动续登")
    parser.add_argument("--stdin", action="store_true", help="从标准输入读取，供受信任脚本使用")
    args = parser.parse_args()
    prompt = "粘贴学校 _idp_session Cookie（不回显）：" if args.sso_cookie else "粘贴网页登录 token（不回显）："
    value = sys.stdin.read().strip() if args.stdin else getpass.getpass(prompt).strip()
    if args.sso_cookie and value.startswith("["):
        value = next((c["value"] for c in json.loads(value) if c.get("name") == "_idp_session"), "")
    elif value.startswith("{"):
        obj = json.loads(value)
        value = obj.get("_idp_session", "") if args.sso_cookie else obj.get("authorization") or obj.get("access_token") or ""
    if not isinstance(value, str):
        raise ValueError("凭据字段必须是字符串")
    value = value.removeprefix("_idp_session=" if args.sso_cookie else "Bearer ").strip()
    if not value or any(ord(c) < 33 or ord(c) > 126 for c in value):
        raise ValueError("凭据为空或包含空白")
    if args.sso_cookie:
        if any(c in value for c in '\\;,"') or not value.isascii():
            raise ValueError("Cookie 值格式不合法，请只复制 _idp_session 的值")
        detail = "学校会话已导入；服务将自动取得 AIGC token"
        default = os.environ.get("AIGC_SSO_COOKIE_FILE", "data/sso-cookie")
    else:
        pieces = value.split(".")
        if len(pieces) != 3:
            raise ValueError("需要网页登录 JWT")
        claims = json.loads(base64.urlsafe_b64decode(pieces[1] + "=" * (-len(pieces[1]) % 4)))
        expires = datetime.fromtimestamp(claims["exp"], timezone.utc)
        if expires <= datetime.now(timezone.utc):
            raise ValueError("token 已过期，请重新进行学校 SSO 登录")
        detail = "有效期至 " + expires.astimezone().isoformat()
        default = os.environ.get("AIGC_TOKEN_FILE", "data/access-token")
    target = Path(args.file or default).resolve()
    target.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
    name = None
    try:
        fd, name = tempfile.mkstemp(prefix=".token-", dir=target.parent)
        with os.fdopen(fd, "w") as out:
            out.write(value + "\n")
        os.replace(name, target)
        name = None
    finally:
        if name:
            os.unlink(name)
    print(f"已更新 {target}；{detail}；无需重启，等待下一次续登检查或执行 -refresh-sso。")


if __name__ == "__main__":
    try:
        main()
    except (ValueError, KeyError, OSError) as error:
        raise SystemExit(f"导入失败：{error}") from None
