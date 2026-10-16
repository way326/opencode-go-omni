"""Offline Windows C ABI smoke test for the Chat -> Responses stream lifecycle fix.

Usage:
    python tools/stream_lifecycle_abi_smoke.py path/to/candidate.dll
    python tools/stream_lifecycle_abi_smoke.py --plan     (prints the case plan, loads nothing)

The script loads only the DLL named on the command line and drives it through the
real cliproxy_plugin_init / plugin.call / plugin.shutdown exports with an
in-memory fake host. It opens no sockets, starts no listeners, reads no real
credentials, and never calls a live API: every upstream answer comes from the
fake host callbacks below.

Fixture: catalog id "DeepSeek-v4.1-flash" (chat-completions route) plus a
non-selected sibling, a fake selected key in AuthAttributes and an unrelated
fake key in the config YAML, request-timeout 500ms.

Cases:
  1. legal-silence: content + finish_reason + usage trailer, upstream never sends
     DONE. The pump must stop reading and close the downstream cleanly. A DLL that
     reads again parks that read until host.http.stream_close lands, so a
     regression still terminates and fails the assertions instead of hanging.
  2. truncated-eof: half a JSON line then EOF. The downstream must close with an
     explicit error and must not fabricate response.completed.
  3. legal-eof: control - the same legal frames plus a real EOF (done).
  4. watchdog-timeout: upstream sends nothing. request-timeout must release the
     parked read and close the downstream with an error exactly once.

Every blocked wait is bounded and released in a finally block; the plugin only
sees real host callbacks through init/invoke/free, and shutdown is issued only
after all host callbacks have returned.

Exit status: 0 when the summary says PASS, 1 otherwise (see "failures").
"""

import base64
import ctypes as C
import json
import sys
import threading
import time
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))

from abi_smoke import Buffer, Free, HostAPI, HostCall, PluginAPI, b64

MODEL = "DeepSeek-v4.1-flash"
OTHER_MODEL = "glm-5.3"
PUBLIC_MODEL = "opencode-go/" + MODEL
SELECTED_KEY = "sk-stream-lifecycle-selected"
CONFIGURED_KEY = "sk-stream-lifecycle-config-unused"
CATALOG_URL = "https://opencode.ai/inference/go/v1/models"
CHAT_URL = "https://opencode.ai/inference/go/v1/chat/completions"
REQUEST_TIMEOUT_MS = 500
USAGE_PROMPT = 11
USAGE_COMPLETION = 7

READ_GATE_TIMEOUT = 10.0
CASE_TIMEOUT = 20.0
IDLE_TIMEOUT = 10.0

ALLOWED_HOST_METHODS = (
    "host.auth.list",
    "host.auth.save",
    "host.log",
    "host.http.do",
    "host.http.do_stream",
    "host.http.stream_read",
    "host.http.stream_close",
    "host.stream.emit",
    "host.stream.close",
)


def chat_chunk(chunk_id, delta=None, finish=None, usage=None):
    payload = {"id": chunk_id, "object": "chat.completion.chunk", "model": MODEL, "choices": []}
    if delta is not None:
        choice = {"index": 0, "delta": delta}
        if finish is not None:
            choice["finish_reason"] = finish
        payload["choices"] = [choice]
    if usage is not None:
        payload["usage"] = usage
    return ("data: " + json.dumps(payload, separators=(",", ":")) + "\n\n").encode()


CHAT_CONTENT = chat_chunk("c1", delta={"role": "assistant", "content": "done"})
CHAT_FINISH = chat_chunk("c1", delta={}, finish="stop")
CHAT_USAGE = chat_chunk(
    "c1",
    usage={
        "prompt_tokens": USAGE_PROMPT,
        "completion_tokens": USAGE_COMPLETION,
        "total_tokens": USAGE_PROMPT + USAGE_COMPLETION,
    },
)
PARTIAL_LINE = b'data: {"id":"c2","choices":[{"delta":{"content":"lo'
CC_BODY = b64(json.dumps({"model": MODEL, "messages": [{"role": "user", "content": "hi"}], "stream": True}))
CONFIG_YAML = "api-keys:\n  - value: " + CONFIGURED_KEY + "\nrequest-timeout: %dms\n" % REQUEST_TIMEOUT_MS


def case_specs():
    return [
        {
            "name": "legal-silence",
            "frames": [CHAT_CONTENT, CHAT_FINISH, CHAT_USAGE],
            "park_reads": True,
            "clean": True,
            "terminal": 1,
            "usage": (USAGE_PROMPT, USAGE_COMPLETION),
            "parked_reads": (0, 0),
            "max_reads": 3,
        },
        {
            "name": "truncated-eof",
            "frames": [CHAT_CONTENT, PARTIAL_LINE],
            "park_reads": False,
            "clean": False,
            "terminal": 0,
            "usage": None,
            "parked_reads": (0, 0),
            "max_reads": 3,
        },
        {
            "name": "legal-eof",
            "frames": [CHAT_CONTENT, CHAT_FINISH, CHAT_USAGE],
            "park_reads": False,
            "clean": True,
            "terminal": 1,
            "usage": (USAGE_PROMPT, USAGE_COMPLETION),
            "parked_reads": (0, 0),
            "max_reads": 4,
        },
        {
            "name": "watchdog-timeout",
            "frames": [],
            "park_reads": True,
            "clean": False,
            "terminal": 0,
            "usage": None,
            "parked_reads": (1, 1),
            "max_reads": 1,
            "min_elapsed": REQUEST_TIMEOUT_MS / 1000.0 * 0.5,
        },
    ]


def responses_terminal(events):
    """Returns (completed_count, input_tokens, output_tokens) from emitted SSE payloads."""
    terminal = 0
    input_tokens = None
    output_tokens = None
    blob = b"\n\n".join(events).replace(b"\r\n", b"\n")
    for block in blob.split(b"\n\n"):
        if b"response.completed" not in block:
            continue
        for line in block.split(b"\n"):
            line = line.strip()
            if not line.startswith(b"data:"):
                continue
            try:
                event = json.loads(line[5:].strip())
            except ValueError:
                continue
            if not isinstance(event, dict) or event.get("type") != "response.completed":
                continue
            terminal += 1
            usage = (event.get("response") or {}).get("usage") or {}
            input_tokens = usage.get("input_tokens", input_tokens)
            output_tokens = usage.get("output_tokens", output_tokens)
    return terminal, input_tokens, output_tokens


class StreamCase:
    def __init__(self, name, frames, park_reads):
        self.name = name
        self.down_id = "down-" + name
        self.up_id = "up-" + name
        self.callback_id = "callback-" + name
        self.frames = list(frames)
        self.park_reads = park_reads
        self.reads = 0
        self.parked_reads = 0
        self.up_closes = 0
        self.started = 0
        self.emitted = []
        self.down_closes = []
        self.close_order = []
        self.release = threading.Event()
        self.downstream_closed = threading.Event()
        self.upstream_closed = threading.Event()


class FakeHost:
    """In-memory host ABI: no sockets, no listeners, no files, no live API."""

    def __init__(self):
        self.lock = threading.RLock()
        self.failures = []
        self.calls = []
        self.logs = []
        self.catalog_requests = []
        self.held = {}
        self.active = 0
        self.idle = threading.Event()
        self.idle.set()
        self.current = None
        self.cases = []
        self.free = Free(self._free)
        self.host_call = HostCall(self._host_call)

    def require(self, condition, message):
        if condition:
            return
        with self.lock:
            self.failures.append(message)

    def begin(self, case):
        with self.lock:
            self.current = case
            self.cases.append(case)

    def release_all(self):
        with self.lock:
            cases = list(self.cases)
        for case in cases:
            case.release.set()

    def wait_idle(self, timeout):
        return self.idle.wait(timeout)

    def _free(self, ptr, _size):
        with self.lock:
            self.held.pop(ptr, None)

    def _host_call(self, _ctx, method_raw, ptr, size, output):
        with self.lock:
            if self.active == 0:
                self.idle.clear()
            self.active += 1
        try:
            method = method_raw.decode("utf-8")
            raw = C.string_at(ptr, size) if ptr else b""
            request = json.loads(raw) if raw else {}
            with self.lock:
                self.calls.append(method)
            if method not in ALLOWED_HOST_METHODS:
                raise RuntimeError("unexpected host method %r" % (method,))
            result = self._dispatch(method, request)
            encoded = json.dumps({"ok": True, "result": result}).encode()
            allocated = C.create_string_buffer(encoded)
            addr = C.addressof(allocated)
            with self.lock:
                self.held[addr] = allocated
            output.contents.ptr = addr
            output.contents.len = len(encoded)
            return 0
        except Exception as exc:
            with self.lock:
                self.failures.append("host call %r failed: %s" % (method_raw, exc))
            return 1
        finally:
            with self.lock:
                self.active -= 1
                if self.active == 0:
                    self.idle.set()

    def _dispatch(self, method, request):
        if method == "host.auth.list":
            return {"files": []}
        if method == "host.auth.save":
            return {}
        if method == "host.log":
            with self.lock:
                self.logs.append((request.get("level", ""), request.get("message", "")))
            return {}
        if method == "host.http.do":
            return self._http_do(request)
        if method == "host.http.do_stream":
            return self._do_stream(request)
        if method == "host.http.stream_read":
            return self._stream_read(request)
        if method == "host.http.stream_close":
            return self._stream_close(request)
        if method == "host.stream.emit":
            return self._stream_emit(request)
        if method == "host.stream.close":
            return self._stream_close_downstream(request)
        raise RuntimeError("unhandled host method %r" % (method,))

    def _http_do(self, request):
        url = request.get("url")
        if url != CATALOG_URL:
            self.require(False, "unexpected host.http.do url %r (only the in-memory catalog is served)" % (url,))
            return {"StatusCode": 404, "Headers": {}, "Body": b64("{}")}
        headers = {name.lower(): values for name, values in (request.get("headers") or {}).items()}
        self.require(headers.get("authorization") == ["Bearer " + CONFIGURED_KEY],
                     "catalog refresh must use the configured fake key, headers=%r" % (headers,))
        with self.lock:
            self.catalog_requests.append(request)
        body = {"data": [{"id": MODEL}, {"id": OTHER_MODEL}]}
        return {"StatusCode": 200, "Headers": {}, "Body": b64(json.dumps(body))}

    def _do_stream(self, request):
        with self.lock:
            case = self.current
            if case is not None:
                case.started += 1
        if case is None:
            self.require(False, "host.http.do_stream without an active stream case")
            return {"status_code": 500, "headers": {}, "stream_id": ""}
        headers = {name.lower(): values for name, values in (request.get("headers") or {}).items()}
        self.require(request.get("method") == "POST", "%s: do_stream method = %r" % (case.name, request.get("method")))
        self.require(request.get("url") == CHAT_URL,
                     "%s: do_stream url = %r, want %r" % (case.name, request.get("url"), CHAT_URL))
        self.require(headers.get("authorization") == ["Bearer " + SELECTED_KEY],
                     "%s: do_stream must use the selected fake key, headers=%r" % (case.name, headers))
        self.require(headers.get("content-type") == ["application/json"],
                     "%s: do_stream content-type = %r" % (case.name, headers.get("content-type")))
        self.require(request.get("host_callback_id") == case.callback_id,
                     "%s: do_stream callback id = %r, want %r" % (case.name, request.get("host_callback_id"), case.callback_id))
        try:
            upstream_body = json.loads(base64.b64decode(request.get("body") or ""))
        except Exception as exc:
            self.require(False, "%s: do_stream body is not base64 JSON: %s" % (case.name, exc))
            upstream_body = {}
        self.require(upstream_body.get("model") == MODEL,
                     "%s: upstream model = %r, want %r" % (case.name, upstream_body.get("model"), MODEL))
        return {"status_code": 200, "headers": {"Content-Type": ["text/event-stream"]}, "stream_id": case.up_id}

    def _stream_read(self, request):
        with self.lock:
            case = self.current
        if case is None:
            self.require(False, "host.http.stream_read without an active stream case")
            return {"done": True}
        self.require(request.get("stream_id") == case.up_id,
                     "%s: stream_read id = %r, want %r" % (case.name, request.get("stream_id"), case.up_id))
        with self.lock:
            case.reads += 1
            payload = case.frames.pop(0) if case.frames else None
            park = payload is None and case.park_reads
            if park:
                case.parked_reads += 1
        if payload is not None:
            return {"payload": b64(payload)}
        if park:
            if not case.release.wait(READ_GATE_TIMEOUT):
                self.require(False, "%s: parked stream_read was never released by host.http.stream_close" % case.name)
            return {"done": True}
        return {"done": True}

    def _stream_close(self, request):
        with self.lock:
            # Downstream completion can precede upstream reclamation. Route
            # cleanup by its own ID instead of the next case's active stream.
            case = next((item for item in self.cases
                         if item.up_id == request.get("stream_id")), None)
            if case is not None:
                case.up_closes += 1
                case.close_order.append("up")
                case.release.set()
                case.upstream_closed.set()
                matched = True
            else:
                matched = False
        self.require(matched, "host.http.stream_close for unexpected stream id %r" % (request.get("stream_id"),))
        return {}

    def _stream_emit(self, request):
        with self.lock:
            case = self.current
        if case is None:
            self.require(False, "host.stream.emit without an active stream case")
            return {}
        self.require(request.get("stream_id") == case.down_id,
                     "%s: emit id = %r, want %r" % (case.name, request.get("stream_id"), case.down_id))
        with self.lock:
            case.emitted.append(base64.b64decode(request.get("payload") or ""))
        return {}

    def _stream_close_downstream(self, request):
        with self.lock:
            case = self.current
            if case is not None:
                case.down_closes.append({"stream_id": request.get("stream_id"), "error": request.get("error", "")})
                case.close_order.append("down")
                if request.get("stream_id") == case.down_id:
                    case.downstream_closed.set()
                    matched = True
                else:
                    matched = False
            else:
                matched = False
        self.require(matched, "host.stream.close for unexpected stream id %r" % (request.get("stream_id"),))
        return {}


def stream_request(case):
    return {
        "AuthID": "opencode-go-lifecycle-selected.json",
        "AuthProvider": "opencode-go",
        "AuthAttributes": {"api_key": SELECTED_KEY},
        "Model": PUBLIC_MODEL,
        "SourceFormat": "openai",
        "Format": "openai-response",
        "Stream": True,
        "OriginalRequest": CC_BODY,
        "Payload": CC_BODY,
        "Headers": {"X-Opencode-Session": ["lifecycle-session-1"], "Content-Type": ["application/json"]},
        "stream_id": case.down_id,
        "host_callback_id": case.callback_id,
    }


def check_case(host, case, spec, elapsed):
    name = case.name
    if spec["clean"]:
        host.require(len(case.down_closes) == 1 and not case.down_closes[0]["error"],
                     "%s: downstream must close cleanly, closes=%r" % (name, case.down_closes))
    else:
        host.require(len(case.down_closes) == 1 and bool(case.down_closes[0]["error"]),
                     "%s: downstream must close with an explicit error, closes=%r" % (name, case.down_closes))
    terminal, input_tokens, output_tokens = responses_terminal(case.emitted)
    host.require(terminal == spec["terminal"],
                 "%s: response.completed events = %d, want %d" % (name, terminal, spec["terminal"]))
    if spec["usage"] is not None:
        host.require((input_tokens, output_tokens) == spec["usage"],
                     "%s: terminal usage = (%r, %r), want %r" % (name, input_tokens, output_tokens, spec["usage"]))
    host.require(case.up_closes == 1,
                 "%s: upstream stream_close calls = %d, want exactly 1" % (name, case.up_closes))
    low, high = spec["parked_reads"]
    host.require(low <= case.parked_reads <= high,
                 "%s: parked reads = %d, want %d..%d" % (name, case.parked_reads, low, high))
    if "max_reads" in spec:
        host.require(case.reads <= spec["max_reads"],
                     "%s: stream reads = %d, want <= %d" % (name, case.reads, spec["max_reads"]))
    if spec.get("min_elapsed"):
        host.require(elapsed >= spec["min_elapsed"],
                     "%s: downstream closed after %.3fs, watchdog should have waited >= %.3fs" % (name, elapsed, spec["min_elapsed"]))


def case_summary(case):
    terminal, input_tokens, output_tokens = responses_terminal(case.emitted)
    return {
        "name": case.name,
        "reads": case.reads,
        "parked_reads": case.parked_reads,
        "upstream_closes": case.up_closes,
        "downstream_closes": len(case.down_closes),
        "downstream_error": case.down_closes[0]["error"] if case.down_closes else None,
        "response_completed": terminal,
        "usage": [input_tokens, output_tokens],
        "emitted_events": len(case.emitted),
        "close_order": list(case.close_order),
    }


def plan():
    return {
        "model": PUBLIC_MODEL,
        "upstream_url": CHAT_URL,
        "request_timeout_ms": REQUEST_TIMEOUT_MS,
        "cases": [
            {
                "name": spec["name"],
                "frames": len(spec["frames"]),
                "park_reads": spec["park_reads"],
                "expect_clean": spec["clean"],
                "expect_completed": spec["terminal"],
                "expect_usage": list(spec["usage"]) if spec["usage"] else None,
                "expect_parked_reads": list(spec["parked_reads"]),
            }
            for spec in case_specs()
        ],
        "network_requests": 0,
        "listeners_started": 0,
    }


def main(argv):
    if len(argv) != 2:
        print(__doc__)
        return 2
    if argv[1] == "--plan":
        print(json.dumps(plan(), indent=2, sort_keys=True))
        return 0

    host = FakeHost()
    try:
        dll = C.CDLL(argv[1])
    except OSError as exc:
        sys.stderr.write("failed to load %s: %s" % (argv[1], exc))
        return 1
    init = dll.cliproxy_plugin_init
    init.argtypes = [C.POINTER(HostAPI), C.POINTER(PluginAPI)]
    init.restype = C.c_int
    host_api = HostAPI(1, None, host.host_call, host.free)
    plugin_api = PluginAPI()
    if init(C.byref(host_api), C.byref(plugin_api)) != 0:
        raise SystemExit("cliproxy_plugin_init failed")
    if plugin_api.abi_version != 1:
        raise SystemExit("unexpected ABI version %r" % (plugin_api.abi_version,))

    def invoke(method, request, expect_error=False):
        encoded = json.dumps(request).encode()
        buf = C.create_string_buffer(encoded)
        output = Buffer()
        rc = plugin_api.call(method.encode(), C.cast(buf, C.c_void_p), len(encoded), C.byref(output))
        try:
            if rc != 0:
                raise RuntimeError("plugin call %s returned %d" % (method, rc))
            envelope = json.loads(C.string_at(output.ptr, output.len))
            if expect_error:
                if envelope.get("ok") or not envelope.get("error"):
                    raise RuntimeError("plugin call %s: expected an error envelope" % method)
                return envelope["error"]
            if not envelope.get("ok"):
                raise RuntimeError("plugin call %s failed: %r" % (method, envelope.get("error")))
            return envelope.get("result") or {}
        finally:
            if output.ptr:
                plugin_api.free_buffer(output.ptr, output.len)

    cases = []
    summary = {
        "abi": 1,
        "model": PUBLIC_MODEL,
        "request_timeout_ms": REQUEST_TIMEOUT_MS,
        "network_requests": 0,
        "listeners_started": 0,
    }
    try:
        register = invoke("plugin.register", {"schema_version": 6, "config_yaml": b64(CONFIG_YAML)})
        host.require(bool(register), "plugin.register returned an empty result")
        host.require(len(host.catalog_requests) >= 1, "plugin.register never fetched the in-memory catalog")
        for spec in case_specs():
            case = StreamCase(spec["name"], spec["frames"], spec["park_reads"])
            host.begin(case)
            started = time.monotonic()
            invoke("executor.execute_stream", stream_request(case))
            closed = case.downstream_closed.wait(CASE_TIMEOUT)
            elapsed = time.monotonic() - started if closed else None
            if not closed:
                host.require(False, "%s: downstream close never arrived within %.1fs" % (case.name, CASE_TIMEOUT))
            # An idle callback counter is not a lifecycle completion signal:
            # it can briefly reach zero between downstream and upstream calls.
            host.require(case.upstream_closed.wait(IDLE_TIMEOUT),
                         "%s: upstream reclamation never arrived" % case.name)
            host.require(host.wait_idle(IDLE_TIMEOUT),
                         "%s: host callbacks still in flight after downstream close" % case.name)
            check_case(host, case, spec, elapsed if elapsed is not None else CASE_TIMEOUT)
            case.release.set()
            cases.append(case)
    except Exception as exc:
        host.require(False, "harness error: %s" % exc)
    finally:
        host.release_all()
        if not host.wait_idle(IDLE_TIMEOUT):
            host.require(False, "host callbacks still in flight before shutdown")
        plugin_api.shutdown()
        host.require(not host.held, "host callback buffers leaked: %d" % len(host.held))

    summary["cases"] = [case_summary(case) for case in cases]
    summary["host_calls"] = len(host.calls)
    summary["host_logs"] = len(host.logs)
    summary["failures"] = list(host.failures)
    summary["result"] = "FAIL" if host.failures else "PASS"
    print(json.dumps(summary, indent=2, sort_keys=True))
    if host.failures:
        sys.stderr.write("stream lifecycle ABI smoke failed:\n")
        for failure in host.failures:
            sys.stderr.write("  - %s\n" % failure)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main(sys.argv))