"""Offline Windows C ABI smoke test. No network, files of CPA, or listening ports.

Usage: python tools/abi_smoke.py path/to/opencode-go-omni.dll
The fake host keeps auth records in memory, returns deterministic inference,
and serves a deterministic OpenCode Go /usage fixture for the quota provider.
"""
import base64
import ctypes as C
import json
import sys
import threading


def b64(value):
    if isinstance(value, str):
        value = value.encode()
    return base64.b64encode(value).decode()


class Buffer(C.Structure):
    _fields_ = [("ptr", C.c_void_p), ("len", C.c_size_t)]


HostCall = C.CFUNCTYPE(C.c_int, C.c_void_p, C.c_char_p, C.c_void_p, C.c_size_t, C.POINTER(Buffer))
Free = C.CFUNCTYPE(None, C.c_void_p, C.c_size_t)
PluginCall = C.CFUNCTYPE(C.c_int, C.c_char_p, C.c_void_p, C.c_size_t, C.POINTER(Buffer))
Shutdown = C.CFUNCTYPE(None)


class HostAPI(C.Structure):
    _fields_ = [("abi_version", C.c_uint32), ("host_ctx", C.c_void_p), ("call", HostCall), ("free_buffer", Free)]


class PluginAPI(C.Structure):
    _fields_ = [("abi_version", C.c_uint32), ("call", PluginCall), ("free_buffer", Free), ("shutdown", Shutdown)]


def main(path):
    held, calls, failures = {}, [], []
    lock = threading.Lock()
    closed = threading.Event()
    emitted = []
    key = "sk-offline-selected-fixture-1"
    configured_key = "sk-offline-config-not-selected"
    model = "gemini-4-argon-contributor"
    quota_callback = "callback-quota-offline"
    quota_reset_at = "2026-10-15T12:00:00Z"
    quota_seen = []
    v2_seen = []
    response = {"candidates": [{"index": 0, "content": {"role": "model", "parts": [{"text": "offline OK", "thoughtSignature": "AQID"}]}, "finishReason": "STOP"}], "usageMetadata": {"promptTokenCount": 2, "candidatesTokenCount": 1, "totalTokenCount": 3}}
    wire = ("data: " + json.dumps(response, separators=(",", ":")) + "\r\n\r\n").encode()
    chunks = []
    quota_fixture = {"usage": {
        "rolling": {"status": "ok", "percent": 4, "resetsAt": quota_reset_at},
        "weekly": {"status": "ok", "percent": 7, "resetsAt": quota_reset_at},
        "monthly": {"status": "ok", "percent": 41, "resetsAt": quota_reset_at},
    }}

    @Free
    def free(ptr, _size):
        with lock:
            held.pop(ptr, None)

    @HostCall
    def host_call(_ctx, method_raw, ptr, size, output):
        try:
            method = method_raw.decode()
            req = json.loads(C.string_at(ptr, size))
            with lock:
                calls.append(method)
            result = {}
            if method == "host.auth.list":
                result = {"files": []}
            elif method == "host.http.do":
                if req.get("host_callback_id") == quota_callback:
                    assert req["method"] == "GET", "quota upstream must use GET"
                    assert req["url"] == "https://opencode.ai/inference/go/v1/usage", req["url"]
                    headers = {name.lower(): values for name, values in req["headers"].items()}
                    assert headers["authorization"] == ["Bearer " + key], headers
                    assert headers["accept"] == ["application/json"], headers
                    quota_seen.append(req)
                    result = {"StatusCode": 200, "Headers": {}, "Body": b64(json.dumps(quota_fixture))}
                else:
                    assert req["url"].endswith("/models"), "unexpected nonstream URL"
                    result = {"StatusCode": 200, "Headers": {}, "Body": b64(json.dumps({"data": [{"id": model}, {"id": "glm-5.3"}, {"id": "gpt-5.6-luna"}, {"id": "minimax-m3"}]}))}
            elif method == "host.http.do_stream":
                assert req["host_callback_id"] == "callback-offline"
                assert req["url"] == f"https://opencode.ai/inference/go/v1/models/{model}:streamGenerateContent?alt=sse"
                headers = {name.lower(): values for name, values in req["headers"].items()}
                assert headers["authorization"] == ["Bearer " + key]
                assert "x-goog-api-key" not in headers
                assert headers["x-opencode-session"] == ["offline-session-A"]
                assert "contents" in json.loads(base64.b64decode(req["body"]))
                upstream_body = json.loads(base64.b64decode(req["body"]))
                if "V2-ABI-TASK-391" in json.dumps(upstream_body):
                    assert "agent_message" not in json.dumps(upstream_body)
                    assert "encrypted_content" not in json.dumps(upstream_body)
                    assert "/root/abi-worker" in json.dumps(upstream_body)
                    v2_seen.append(upstream_body)
                chunks[:] = [wire[:5], wire[5:17], wire[17:]]
                result = {"status_code": 200, "headers": {"Content-Type": ["text/event-stream"]}, "stream_id": "up-offline"}
            elif method == "host.http.stream_read":
                result = {"payload": b64(chunks.pop(0))} if chunks else {"done": True}
            elif method == "host.stream.emit":
                emitted.append(base64.b64decode(req["payload"]))
            elif method == "host.stream.close":
                assert not req.get("error"), "downstream stream failed"
                closed.set()
            encoded = json.dumps({"ok": True, "result": result}).encode()
            allocated = C.create_string_buffer(encoded)
            addr = C.addressof(allocated)
            with lock:
                held[addr] = allocated
            output.contents.ptr = addr
            output.contents.len = len(encoded)
            return 0
        except Exception as exc:
            failures.append(str(exc))
            return 1

    dll = C.CDLL(path)
    init = dll.cliproxy_plugin_init
    init.argtypes = [C.POINTER(HostAPI), C.POINTER(PluginAPI)]
    init.restype = C.c_int
    host = HostAPI(1, None, host_call, free)
    plug = PluginAPI()
    assert init(C.byref(host), C.byref(plug)) == 0
    assert plug.abi_version == 1

    def invoke(method, request, expect_error=False):
        encoded = json.dumps(request).encode()
        buf = C.create_string_buffer(encoded)
        output = Buffer()
        assert plug.call(method.encode(), C.cast(buf, C.c_void_p), len(encoded), C.byref(output)) == 0
        try:
            envelope = json.loads(C.string_at(output.ptr, output.len))
            if expect_error:
                assert not envelope["ok"] and envelope.get("error"), "expected plugin error"
                return envelope["error"]
            assert envelope["ok"], envelope.get("error")
            return envelope["result"]
        finally:
            plug.free_buffer(output.ptr, output.len)

    try:
        reg = invoke("plugin.register", {"schema_version": 6, "config_yaml": b64("api-keys:\n  - value: " + configured_key + "\n")})
        meta = reg["metadata"]
        assert meta.get("Name", meta.get("name")) == "opencode-go-omni"
        assert reg["schema_version"] == 6
        capabilities = reg["capabilities"]
        assert capabilities["auth_provider"] is False, "plugin must not advertise OAuth login"
        assert all(capabilities[name] for name in ("model_provider", "executor", "management_api"))
        assert capabilities["executor_model_scope"] == "oauth"
        for name in ("executor_input_formats", "executor_output_formats"):
            assert set(capabilities[name]) == {"openai", "claude", "openai-response", "gemini"}
        assert reg["capabilities"].get("quota_provider") is True, reg["capabilities"]
        identifier = invoke("quota.identifier", {})
        assert identifier.get("identifier") == "opencode-go", identifier
        described = invoke("quota.describe", {})
        assert described.get("supported_providers") == ["opencode-go"], described
        assert described.get("display_name") == "OpenCode Go", described
        # SDK QuotaDescribeResponse.SupportsReset carries omitempty; absence means false.
        assert described.get("supports_reset", False) is False, described
        fetched = invoke("quota.fetch", {
            "provider": "opencode-go",
            "auth_id": "auth-offline-quota",
            "auth_index": "0",
            "attributes": {"api_key": key},
            "host_callback_id": quota_callback,
        })
        groups = fetched.get("groups") or []
        assert len(groups) == 1, groups
        group = groups[0]
        assert group.get("displayName", group.get("display_name")) == "OpenCode Go", group
        buckets = group.get("buckets") or []
        assert len(buckets) == 3, buckets
        by_window = {bucket.get("window"): bucket for bucket in buckets}
        assert set(by_window) == {"rolling", "weekly", "monthly"}, by_window
        for window, percent in (("rolling", 4.0), ("weekly", 7.0), ("monthly", 41.0)):
            bucket = by_window[window]
            remaining = bucket.get("remainingFraction", bucket.get("remaining_fraction"))
            assert remaining is not None and abs(remaining - (1 - percent / 100)) <= 1e-9, bucket
            assert bucket.get("resetTime", bucket.get("reset_time")) == quota_reset_at, bucket
            assert bucket.get("description") == window + " (ok)", bucket
        assert len(quota_seen) == 1, quota_seen
        for credential in (
            {"metadata": {"type": "opencode-go", "api_key": key}},
            {"storage_json": b64(json.dumps({"type": "opencode-go", "api_key": key}))},
        ):
            quota_request = {"provider": "opencode-go", "auth_id": "auth-offline-quota", "auth_index": "0", "host_callback_id": quota_callback}
            quota_request.update(credential)
            assert invoke("quota.fetch", quota_request) == fetched, "quota credential sources differ"
        assert len(quota_seen) == 3, quota_seen
        reset = invoke("quota.reset", {"provider": "opencode-go", "auth_id": "auth-offline-quota", "auth_index": "0"}, expect_error=True)
        assert reset.get("code") == "unsupported", reset
        assert len(quota_seen) == 3, "quota.reset must not call upstream"
        listed = invoke("model.static", {})
        records = listed.get("Models", listed.get("models", []))
        assert len(records) == 4
        body = b64('{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}')
        credential_shapes = {
            "attributes": {"AuthAttributes": {"api_key": key}},
            "metadata": {"AuthMetadata": {"type": "opencode-go", "api_key": key}},
            "storage": {"StorageJSON": b64(json.dumps({"type": "opencode-go", "api_key": key}))},
        }
        stream_chunks = 0
        for label, credential in credential_shapes.items():
            closed.clear()
            emitted.clear()
            request = {"AuthID": "opencode-go-key-selected.json", "AuthProvider": "opencode-go", "Model": "opencode-go/" + model, "SourceFormat": "gemini", "Format": "gemini", "Stream": True, "OriginalRequest": body, "Payload": body, "Headers": {"X-Opencode-Session": ["offline-session-A"]}, "stream_id": "down-offline-" + label, "host_callback_id": "callback-offline"}
            request.update(credential)
            invoke("executor.execute_stream", request)
            assert closed.wait(15), "DLL stream timed out: " + label
            assert emitted and all(json.loads(chunk) for chunk in emitted)
            assert any(b"thoughtSignature" in chunk for chunk in emitted)
            assert any(b"usageMetadata" in chunk for chunk in emitted)
            stream_chunks += len(emitted)
        # Exercise the real compiled V2 normalizer through C ABI and a fake host.
        v2_body = b64(json.dumps({"input": [{"type": "agent_message", "author": "/root", "recipient": "/root/abi-worker", "content": [{"type": "input_text", "text": "Message Type: NEW_TASK\nPayload:\n"}, {"type": "encrypted_content", "encrypted_content": "V2-ABI-TASK-391"}]}]}))
        closed.clear()
        emitted.clear()
        request = {"AuthProvider": "opencode-go", "AuthAttributes": {"api_key": key}, "Model": "opencode-go/" + model, "SourceFormat": "openai-response", "Format": "gemini", "Stream": True, "OriginalRequest": v2_body, "Payload": v2_body, "Headers": {"X-Opencode-Session": ["offline-session-A"]}, "stream_id": "down-v2", "host_callback_id": "callback-offline"}
        invoke("executor.execute_stream", request)
        assert closed.wait(15), "V2 DLL stream timed out"
        assert len(v2_seen) == 1 and emitted, "V2 task did not reach fake host"
        opaque = base64.urlsafe_b64encode(bytes([128]) + bytes(72)).decode()
        bad_body = b64(json.dumps({"input": [{"type": "agent_message", "content": [{"type": "encrypted_content", "encrypted_content": opaque}]}]}))
        request["Payload"] = request["OriginalRequest"] = bad_body
        upstream_calls = calls.count("host.http.do_stream")
        opaque_error = invoke("executor.execute_stream", request, expect_error=True)
        assert "opaque encrypted task" in opaque_error.get("message", ""), opaque_error
        assert calls.count("host.http.do_stream") == upstream_calls, "opaque V2 task reached inference"
        request = {"AuthProvider": "opencode-go", "Model": "opencode-go/" + model, "SourceFormat": "gemini", "Format": "gemini", "Stream": True, "OriginalRequest": body, "Payload": body, "stream_id": "down-missing"}
        upstream_calls = calls.count("host.http.do_stream")
        missing_error = invoke("executor.execute_stream", request, expect_error=True)
        assert calls.count("host.http.do_stream") == upstream_calls, "missing credential contacted upstream"
        assert key not in json.dumps(missing_error) and configured_key not in json.dumps(missing_error)
        assert not failures, failures
        print(json.dumps({"abi": 1, "schema": 6, "catalog_models": len(records), "advertises_oauth": capabilities["auth_provider"], "credential_shapes": list(credential_shapes), "selected_key_differs_from_config": key != configured_key, "quota_buckets": len(buckets), "quota_credential_shapes": 3, "v2_tasks_seen": len(v2_seen), "v2_opaque_rejected": True, "stream_chunks": stream_chunks, "host_calls": len(calls), "network_requests": 0, "listeners_started": 0, "result": "PASS"}))
    finally:
        plug.shutdown()
    assert not held, "host callback buffers leaked"


if __name__ == "__main__":
    main(sys.argv[1])
