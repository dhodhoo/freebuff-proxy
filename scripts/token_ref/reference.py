#!/usr/bin/env python3
"""Golden reference for internal/tokenestimate (o200k_base, FreeBuff fudge).

Cross-validates the Go estimator against the independent Python tiktoken
implementation. Regenerate goldens after changing estimator constants:

    python scripts/token_ref/reference.py

Every fixture below mirrors a case in internal/tokenestimate/estimator_test.go.
Mismatch = the Go estimator diverged from this reference; update the Go test
values to the printed numbers (or fix the Go code) and re-run the Go suite.

Requires: pip install tiktoken (tested with tiktoken 0.13.0, Python 3.14).
"""
import json
import math
from collections import OrderedDict

import tiktoken

ENCODING = tiktoken.get_encoding("o200k_base")
FUDGE = 1.35
PER_MESSAGE_OVERHEAD = 8
IMAGE_TOKEN_ESTIMATE = 1600


class Ordered(dict):
    """dict that preserves insertion order when Go-marshaled (struct fields)."""


def count_text(text: str) -> int:
    """Mirror Estimator.CountText: floor(raw * 1.35); '' -> 0."""
    if text == "":
        return 0
    return math.floor(len(ENCODING.encode(text, allowed_special="all")) * FUDGE)


def go_marshal(v):
    """Mirror Go encoding/json: struct field order preserved, map keys sorted,
    compact separators, HTML-safe escapes."""
    if isinstance(v, Ordered):
        return "{" + ",".join(
            f'"{k}":{go_marshal(val)}' for k, val in v.items()
        ) + "}"
    if isinstance(v, dict):
        return "{" + ",".join(
            f'"{k}":{go_marshal(v[k])}' for k in sorted(v)
        ) + "}"
    if isinstance(v, list):
        return "[" + ",".join(go_marshal(x) for x in v) + "]"
    if isinstance(v, str):
        return json.dumps(v, ensure_ascii=False)
    if v is None or isinstance(v, (bool, int, float)):
        return json.dumps(v)
    raise TypeError(type(v))


def count_json(value) -> int:
    """Mirror Estimator.CountJSON: deterministic Go-style marshal then count."""
    if value is None:
        return 0
    return count_text(go_marshal(value))


def tools_for_count(tools):
    """Mirror Estimator.toolsForCount: struct {name, description, input_schema}."""
    out = []
    for tool in tools:
        tc = Ordered()
        tc["name"] = tool.get("name", "")
        if tool.get("description"):
            tc["description"] = tool["description"]
        if tool.get("input_schema") is not None:
            tc["input_schema"] = tool["input_schema"]
        out.append(tc)
    return out


def count_anthropic_request(req) -> int:
    """Mirror Estimator.CountAnthropicRequest (subset used by the fixtures)."""
    total = 0
    system = req.get("system")
    if isinstance(system, str):
        total += count_text(system)
    elif isinstance(system, list):
        for part in system:
            if not isinstance(part, dict):
                continue
            typ = part.get("type")
            if typ == "text":
                total += count_text(part.get("text", ""))
            elif typ == "image":
                total += IMAGE_TOKEN_ESTIMATE
            else:
                total += count_json(part)
    for msg in req.get("messages", []):
        total += PER_MESSAGE_OVERHEAD
        content = msg.get("content")
        if content is None:
            continue
        if isinstance(content, str):
            total += count_text(content)
            continue
        for part in content:
            typ = part.get("type")
            if typ == "text":
                total += count_text(part.get("text", ""))
            elif typ == "thinking":
                total += count_text(part.get("thinking") or part.get("text") or "")
            elif typ in ("tool_use", "server_tool_use"):
                total += count_text(part.get("name", "")) + count_json(part.get("input"))
            elif typ == "tool_result":
                tr = part.get("content")
                if isinstance(tr, str):
                    total += count_text(tr)
                elif isinstance(tr, list):
                    for item in tr:
                        if isinstance(item, str):
                            total += count_text(item)
                        elif isinstance(item, dict) and item.get("type") == "text":
                            total += count_text(item.get("text", ""))
                        elif isinstance(item, dict) and item.get("type") == "image":
                            total += IMAGE_TOKEN_ESTIMATE
                        else:
                            total += count_json(item)
                else:
                    total += count_json(tr)
            elif typ == "image":
                total += IMAGE_TOKEN_ESTIMATE
            else:
                total += count_json(part)
    if isinstance(req.get("tools"), list):
        total += count_json(tools_for_count(req["tools"]))
    return total


# --- fixtures (mirror estimator_test.go) ---
CODE_FIXTURE = """func (e *Estimator) CountText(text string) int {
	if text == "" {
		return 0
	}
	n, err := e.codec.Count(text)
	if err != nil {
		units := len(utf16.Encode([]rune(text)))
		return (units + fallbackDivisor - 1) / fallbackDivisor
	}
	return int(math.Floor(float64(n) * fudgeFactor))
}"""

TEXT_CASES = [
    ("empty", "", 0),
    ("hello", "hello", 1),
    ("hello world", "hello world", 2),
    ("Hello, world!", "Hello, world!", 5),
    ("indonesian", "Saya sedang menguji penghitung token untuk aplikasi ini.", 14),
    ("cjk", "你好世界，这是一个测试。", 8),
    ("emoji combining", "👨\u200d💻🚀 café naïve e\u0301", 16),
    ("go code", CODE_FIXTURE, 116),
    ("long", "The quick brown fox jumps over the lazy dog. " * 60, 811),
]

JSON_CASES = [
    ("tool_input_small", {"city": "Jakarta"}, 8),
    ("tool_input_big", {"city": "Jakarta", "extra": "a" * 64, "unit": "celsius"}, 29),
    ("unknown_block", {"foo": "bar", "type": "weird"}, 13),
    ("json_ok", "ok", 4),
]

EMPTY_MSG = [{"role": "user"}]
TOOLS_MINIMAL = [{"name": "get_weather", "input_schema": {
    "type": "object", "properties": {"city": {"type": "string"}}}}]
TOOLS_RICH = [{"name": "get_weather", "description": "Get current weather",
               "input_schema": {"type": "object", "required": ["city"],
                                "properties": {"city": {"description": "City name", "type": "string"},
                                               "unit": {"enum": ["celsius", "fahrenheit"], "type": "string"}}}}]
TOOLS_TWO = [{"name": "get_weather", "description": "Get current weather",
              "input_schema": {"type": "object", "properties": {"city": {"type": "string"}}}},
             {"name": "search_docs", "input_schema": {"type": "object", "properties": {"query": {"type": "string"}}}}]

REQ_CASES = [
    ("complex", {"system": "You are a helpful assistant.", "messages": [
        {"role": "user", "content": "hello"},
        {"role": "assistant", "content": [{"type": "thinking", "thinking": "Let me think about this."}]},
        {"role": "assistant", "content": [{"type": "tool_use", "id": "t1", "name": "get_weather", "input": {"city": "Jakarta"}}]},
        {"role": "user", "content": [{"type": "tool_result", "tool_use_id": "t1", "content": [{"type": "text", "text": "3 files"}]}]},
    ], "tools": TOOLS_MINIMAL}, 93),
    ("no tools", {"messages": EMPTY_MSG}, 8),
    ("tools_minimal", {"messages": EMPTY_MSG, "tools": TOOLS_MINIMAL}, 40),
    ("tools_rich", {"messages": EMPTY_MSG, "tools": TOOLS_RICH}, 83),
    ("tools_two", {"messages": EMPTY_MSG, "tools": TOOLS_TWO}, 78),
    ("system_image_flat", {"system": [{"type": "image", "source": {"type": "base64", "data": "QUJD" * 40000}}],
                           "messages": [{"role": "user", "content": "hi"}]}, 1600 + 8 + 1),
]


def main() -> int:
    failures = 0
    for name, text, want in TEXT_CASES:
        got = count_text(text)
        mark = "ok" if got == want else "** MISMATCH **"
        if got != want:
            failures += 1
        print(f"{'count_text':16} {name:18} {got:6}  want {want:6}  {mark}")
    for name, value, want in JSON_CASES:
        got = count_json(value)
        mark = "ok" if got == want else "** MISMATCH **"
        if got != want:
            failures += 1
        print(f"{'count_json':16} {name:18} {got:6}  want {want:6}  {mark}")
    for name, req, want in REQ_CASES:
        got = count_anthropic_request(req)
        mark = "ok" if got == want else "** MISMATCH **"
        if got != want:
            failures += 1
        print(f"{'count_request':14} {name:18} {got:6}  want {want:6}  {mark}")
    print(f"\n{len(TEXT_CASES) + len(JSON_CASES) + len(REQ_CASES) - failures}/{len(TEXT_CASES) + len(JSON_CASES) + len(REQ_CASES)} matched")
    return 1 if failures else 0


if __name__ == "__main__":
    raise SystemExit(main())
