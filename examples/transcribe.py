#!/usr/bin/env python3
"""Transcribe a WAV or MP3 file through AGITJ2API."""
import argparse
from pathlib import Path

from openai import OpenAI


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("audio", type=Path)
    parser.add_argument("--model", default="Whisper", help="Whisper, whisper-1 or Paraformer")
    parser.add_argument("--language", help="language code, such as zh; omit for Whisper auto-detection")
    parser.add_argument("--format", choices=("json", "text"), default="json")
    parser.add_argument("--request-id", required=True, help="save this key; reuse it only for the same request")
    args = parser.parse_args()
    client = OpenAI(timeout=1000, max_retries=0)
    options = {
        "model": args.model,
        "response_format": args.format,
        "extra_headers": {"Idempotency-Key": args.request_id},
    }
    if args.language:
        options["language"] = args.language
    with args.audio.open("rb") as audio:
        result = client.audio.transcriptions.create(file=audio, **options)
    print(result if isinstance(result, str) else result.text)


if __name__ == "__main__":
    main()
