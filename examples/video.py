#!/usr/bin/env python3
"""Submit a video job, poll its status and download the result."""
import argparse
from pathlib import Path
import time

from openai import OpenAI


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--prompt", required=True)
    parser.add_argument("--model", default="minimax-h3-t2v")
    parser.add_argument("--seconds", default="5", help="must match the workflow's supported duration")
    parser.add_argument("--request-id", required=True, help="save this key; reuse it only for the same request")
    parser.add_argument("--output", type=Path, default=Path("video.mp4"))
    args = parser.parse_args()
    client = OpenAI(timeout=1000, max_retries=0)
    video = client.videos.create(
        model=args.model,
        prompt=args.prompt,
        seconds=args.seconds,
        extra_headers={"Idempotency-Key": args.request_id},
    )
    print(f"Task: {video.id}", flush=True)
    deadline = time.monotonic() + 20 * 60
    while video.status not in ("completed", "failed"):
        print(f"{video.status}: {video.progress}%", flush=True)
        if time.monotonic() >= deadline:
            raise SystemExit(f"Still running: {video.id}. Resume using the same request ID and parameters.")
        time.sleep(5)
        video = client.videos.retrieve(video.id)
    if video.status == "failed":
        raise SystemExit(f"Task failed: {video.error}")
    client.videos.download_content(video.id).write_to_file(args.output)
    print(f"Saved {args.output}")


if __name__ == "__main__":
    main()
