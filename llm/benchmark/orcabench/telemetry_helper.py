#!/usr/bin/env python3
"""Small Grafana proxy client staged into the ORCA-Bench environment."""

import argparse
import base64
import datetime as dt
import json
import os
import sys
import urllib.error
import urllib.parse
import urllib.request


def _timestamp(value: str, unit: str) -> str:
    try:
        numeric = float(value)
        magnitude = abs(numeric)
        if magnitude >= 100_000_000_000_000_000:
            numeric /= 1_000_000_000
        elif magnitude >= 100_000_000_000_000:
            numeric /= 1_000_000
        elif magnitude >= 100_000_000_000:
            numeric /= 1_000
    except ValueError:
        parsed = dt.datetime.fromisoformat(value.replace("Z", "+00:00"))
        if parsed.tzinfo is None:
            parsed = parsed.replace(tzinfo=dt.timezone.utc)
        numeric = parsed.timestamp()
    multiplier = 1_000_000 if unit == "microseconds" else 1
    return str(int(numeric * multiplier))


def _request(path: str, *, params=None, body=None):
    base_url = os.environ.get("GRAFANA_URL")
    if not base_url:
        raise RuntimeError("GRAFANA_URL is unavailable")
    url = f"{base_url.rstrip('/')}{path}"
    if params:
        url += "?" + urllib.parse.urlencode(params)
    payload = json.dumps(body).encode() if body is not None else None
    request = urllib.request.Request(url, data=payload)
    request.add_header(
        "Authorization",
        "Basic " + base64.b64encode(b"admin:admin").decode("ascii"),
    )
    if payload is not None:
        request.add_header("Content-Type", "application/json")
    try:
        with urllib.request.urlopen(request, timeout=30) as response:
            return json.load(response)
    except urllib.error.HTTPError as error:
        detail = error.read().decode("utf-8", errors="replace")
        raise RuntimeError(f"Grafana proxy returned HTTP {error.code}: {detail}")


def _parser():
    parser = argparse.ArgumentParser(
        description="Query ORCA telemetry through its Grafana datasource proxies."
    )
    parser.add_argument("--uid", required=True, help="Datasource UID from the capability map")
    parser.add_argument("--max-chars", type=int, default=12000)
    subparsers = parser.add_subparsers(dest="operation", required=True)

    metrics = subparsers.add_parser("metrics")
    metrics.add_argument("--query", required=True)
    metrics.add_argument("--start", help="ISO 8601 or epoch timestamp")
    metrics.add_argument("--end", help="ISO 8601 or epoch timestamp")
    metrics.add_argument("--step", default="15s")

    services = subparsers.add_parser("services")

    traces = subparsers.add_parser("traces")
    traces.add_argument("--service", required=True)
    traces.add_argument("--start", required=True, help="ISO 8601 or epoch timestamp")
    traces.add_argument("--end", required=True, help="ISO 8601 or epoch timestamp")
    traces.add_argument("--limit", type=int, default=100)
    traces.add_argument("--tags")

    logs = subparsers.add_parser("logs")
    logs.add_argument("--index", required=True)
    body = logs.add_mutually_exclusive_group(required=True)
    body.add_argument("--body", help="OpenSearch JSON request body")
    body.add_argument("--body-file", help="File containing an OpenSearch JSON request body")
    return parser


def _execute(args):
    proxy = f"/api/datasources/proxy/uid/{urllib.parse.quote(args.uid, safe='')}"
    if args.operation == "metrics":
        params = {"query": args.query}
        path = "/api/v1/query"
        if args.start and args.end:
            path = "/api/v1/query_range"
            params.update(
                start=_timestamp(args.start, "seconds"),
                end=_timestamp(args.end, "seconds"),
                step=args.step,
            )
        elif args.start or args.end:
            raise RuntimeError("metrics requires both --start and --end, or neither")
        return _request(proxy + path, params=params)
    if args.operation == "services":
        return _request(proxy + "/api/services")
    if args.operation == "traces":
        params = {
            "service": args.service,
            "start": _timestamp(args.start, "microseconds"),
            "end": _timestamp(args.end, "microseconds"),
            "limit": args.limit,
        }
        if args.tags:
            params["tags"] = args.tags
        return _request(proxy + "/api/traces", params=params)
    if args.body_file:
        with open(args.body_file, encoding="utf-8") as source:
            body = json.load(source)
    else:
        body = json.loads(args.body)
    index = urllib.parse.quote(args.index, safe="*,-_")
    return _request(f"{proxy}/{index}/_search", body=body)


def main():
    args = _parser().parse_args()
    try:
        output = json.dumps(_execute(args), separators=(",", ":"))
        if len(output) > args.max_chars:
            output = output[: args.max_chars] + "\n[output truncated]"
        print(output)
    except (OSError, ValueError, RuntimeError) as error:
        print(f"error: {error}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
