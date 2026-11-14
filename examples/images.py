#!/usr/bin/env python3
"""Generate or edit one image using AGITJ2API's OpenAI-style API."""
import argparse
import base64
from pathlib import Path

from openai import OpenAI


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--prompt", required=True)
    parser.add_argument("--request-id", required=True, help="save this key; reuse it only for the same request")
    parser.add_argument("--image", type=Path, help="reference image; enables image editing")
    parser.add_argument("--model", help="defaults to t2i-krea2-turbo or qwen-image-2-1-e")
    parser.add_argument("--size", help="exact workflow size, such as 1024x1024; omitted uses its default")
    parser.add_argument("--output", type=Path, default=Path("image.png"))
    args = parser.parse_args()
    client = OpenAI(timeout=1000, max_retries=0)
    options = {
        "model": args.model or ("qwen-image-2-1-e" if args.image else "t2i-krea2-turbo"),
        "prompt": args.prompt,
        "response_format": "b64_json",
        "extra_body": {"output_format": "png"},
        "extra_headers": {"Idempotency-Key": args.request_id},
    }
    if args.size:
        options["size"] = args.size
    if args.image:
        with args.image.open("rb") as image:
            result = client.images.edit(image=image, **options)
    else:
        result = client.images.generate(**options)
    args.output.write_bytes(base64.b64decode(result.data[0].b64_json))
    print(f"Saved {args.output}")


if __name__ == "__main__":
    main()
