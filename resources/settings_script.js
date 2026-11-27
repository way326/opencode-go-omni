window.initializeSettingsPanel = (() => {
  let initialized = false;
  return function initializeSettingsPanel() {
    if (initialized) return;
    initialized = true;
    (() => {
"use strict";
    const pluginID = "opencode-go-omni";
    const configURL = "/v0/management/plugins/" + pluginID + "/config";
    const form = document.getElementById("settings-form");
    const status = document.getElementById("settings-status");
    const keyList = document.getElementById("key-list");
    let current = {};
    let apiKeys = [];
    let apiKeysChanged = false;

    function managementKey() {
      try {
        const raw = localStorage.getItem("cli-proxy-auth");
        if (!raw) return "";
        let decoded = raw;
        const legacy = raw.startsWith("enc::v1::");
        if (legacy || raw.startsWith("enc::v2::")) {
          const bytes = Uint8Array.from(atob(raw.slice(9)), char => char.charCodeAt(0));
          const seed = new TextEncoder().encode("cli-proxy-api-webui::secure-storage|" + location.host + (legacy ? "|" + navigator.userAgent : ""));
          for (let i = 0; i < bytes.length; i++) bytes[i] ^= seed[i % seed.length];
          decoded = new TextDecoder().decode(bytes);
        }
        const parsed = JSON.parse(decoded);
        return parsed && parsed.state && typeof parsed.state.managementKey === "string" ? parsed.state.managementKey : "";
      } catch (_) { return ""; }
    }
    async function request(url, method, body) {
      const key = managementKey();
      if (!key) throw new Error("请先在 Management Center 登录，并开启“记住密码”后再使用此设置页。");
      const headers = {"Accept": "application/json", "Authorization": "Bearer " + key};
      if (body !== undefined) headers["Content-Type"] = "application/json";
      let response;
      try {
        response = await fetch(url, {method, headers, body: body === undefined ? undefined : JSON.stringify(body)});
      } catch (_) {
        throw new Error("无法连接 CPA 管理接口，请确认 CPA 正在运行并使用受信任的同源页面。");
      }
      if (!response.ok) {
        if (response.status === 401 || response.status === 403) throw new Error("管理认证失败，请重新登录并开启“记住密码”。");
        throw new Error("CPA 管理接口返回错误（HTTP " + response.status + ")，请检查 CPA 版本及插件配置。");
      }
      if (response.status === 204) return {};
      return response.json();
    }
    function showStatus(message, kind) {
      status.textContent = message;
      status.dataset.kind = kind || "";
    }
    function fieldValue(key, fallback) {
      return Object.prototype.hasOwnProperty.call(current, key) ? current[key] : fallback;
    }
    function keyValue(entry) {
      return entry && typeof entry.value === "string" ? entry.value : "";
    }
    function envName(value) {
      const match = /^\$\{([A-Za-z_][A-Za-z0-9_]*)\}$/.exec(value);
      return match ? match[1] : "";
    }
    function renderKeys() {
      keyList.replaceChildren();
      if (!apiKeys.length) {
        const empty = document.createElement("p");
        empty.className = "settings-help";
        empty.textContent = "尚未配置 Key。保存时至少需要一项有效凭据。";
        keyList.appendChild(empty);
      }
      apiKeys.forEach((entry, index) => {
        const row = document.createElement("div");
        row.className = "settings-key-row";
        const field = document.createElement("div");
        field.className = "settings-key-field";
        const defaultName = "Key " + (index + 1);
        const remarkLabel = document.createElement("label");
        remarkLabel.textContent = "备注（可选）";
        const remarkInput = document.createElement("input");
        remarkInput.type = "text";
        remarkInput.autocomplete = "off";
        remarkInput.maxLength = 80;
        remarkInput.placeholder = "例如：个人订阅";
        remarkInput.value = typeof entry.remark === "string" ? entry.remark : "";
        remarkInput.setAttribute("aria-label", defaultName + "备注");
        remarkLabel.appendChild(remarkInput);
        const label = document.createElement("label");
        const displayName = document.createElement("span");
        displayName.className = "settings-key-display-name";
        displayName.textContent = (remarkInput.value.trim() || defaultName) + " · 密钥";
        label.appendChild(displayName);
        const input = document.createElement("input");
        input.type = "password";
        input.autocomplete = "off";
        input.spellcheck = false;
        input.value = keyValue(entry);
        input.setAttribute("aria-label", "API Key " + (remarkInput.value.trim() || index + 1));
        remarkInput.addEventListener("input", () => {
          apiKeys[index].remark = remarkInput.value;
          apiKeysChanged = true;
          displayName.textContent = (remarkInput.value.trim() || defaultName) + " · 密钥";
          input.setAttribute("aria-label", "API Key " + (remarkInput.value.trim() || index + 1));
        });
        input.addEventListener("input", () => {
          apiKeys[index].value = input.value;
          apiKeysChanged = true;
        });
        label.appendChild(input);
        field.append(remarkLabel, label);
        const reference = envName(keyValue(entry));
        if (reference) {
          const detail = document.createElement("small");
          detail.className = "settings-key-ref";
          detail.textContent = "环境变量引用：${" + reference + "}";
          label.appendChild(detail);
        }
        const controls = document.createElement("div");
        controls.className = "settings-key-controls";
        const reveal = document.createElement("button");
        reveal.className = "settings-button";
        reveal.type = "button";
        reveal.textContent = "显示密钥";
        reveal.setAttribute("aria-label", "显示密钥：" + (remarkInput.value.trim() || defaultName));
        reveal.addEventListener("click", () => {
          const visible = input.type === "password";
          input.type = visible ? "text" : "password";
          reveal.textContent = visible ? "隐藏密钥" : "显示密钥";
          reveal.setAttribute("aria-label", (visible ? "隐藏密钥：" : "显示密钥：") + (remarkInput.value.trim() || defaultName));
        });
        const remove = document.createElement("button");
        remove.className = "settings-button settings-button-danger";
        remove.type = "button";
        remove.textContent = "从配置移除";
        remove.setAttribute("aria-label", "从本插件配置移除 Key " + (index + 1));
        remove.setAttribute("aria-expanded", "false");
        const confirmation = document.createElement("div");
        confirmation.className = "settings-full";
        confirmation.hidden = true;
        const note = document.createElement("p");
        note.className = "settings-help";
        note.setAttribute("role", "status");
        note.textContent = "将此 Key 从本插件配置列表中移除？不会删除 CPA 已生成的 Auth 记录，也不会撤销上游 Key。若 CPA 仍选择旧记录，相关请求可能因未知凭据失败。保存设置后才生效。";
        const confirmRemoval = document.createElement("button");
        confirmRemoval.className = "settings-button settings-button-danger";
        confirmRemoval.type = "button";
        confirmRemoval.textContent = "确认移除";
        const cancelRemoval = document.createElement("button");
        cancelRemoval.className = "settings-button";
        cancelRemoval.type = "button";
        cancelRemoval.textContent = "取消";
        remove.addEventListener("click", () => {
          confirmation.hidden = false;
          remove.setAttribute("aria-expanded", "true");
          cancelRemoval.focus();
        });
        cancelRemoval.addEventListener("click", () => {
          confirmation.hidden = true;
          remove.setAttribute("aria-expanded", "false");
          remove.focus();
        });
        confirmRemoval.addEventListener("click", () => {
          apiKeys.splice(index, 1);
          apiKeysChanged = true;
          renderKeys();
          document.getElementById("add-key").focus();
        });
        const confirmationActions = document.createElement("div");
        confirmationActions.className = "settings-key-controls";
        confirmationActions.append(cancelRemoval, confirmRemoval);
        confirmation.append(note, confirmationActions);
        controls.append(reveal, remove);
        row.append(field, controls, confirmation);
        keyList.appendChild(row);
      });
    }
    function fill(config) {
      current = config && typeof config === "object" && !Array.isArray(config) ? config : {};
      apiKeys = Array.isArray(current["api-keys"]) ? current["api-keys"].map(entry => {
        if (entry && typeof entry === "object" && !Array.isArray(entry)) return {...entry, value: typeof entry.value === "string" ? entry.value : "", remark: typeof entry.remark === "string" ? entry.remark : ""};
        return {value: typeof entry === "string" ? entry : "", remark: ""};
      }) : [];
      apiKeysChanged = false;
      document.getElementById("base-url").value = fieldValue("base-url", "https://opencode.ai/inference/go/v1") || "";
      document.getElementById("catalog-url").value = fieldValue("catalog-url", "") || "";
      document.getElementById("request-timeout").value = fieldValue("request-timeout", "5m") || "";
      document.getElementById("max-response-bytes").value = String(fieldValue("max-response-bytes", 67108864));
      const prefix = current["model-prefix"] || {};
      document.getElementById("prefix-enabled").checked = prefix.enabled !== false;
      document.getElementById("prefix-value").value = prefix.value || "opencode-go";
      const protocols = current.protocols || {};
      document.getElementById("protocol-chat").checked = protocols["chat-completions"] !== false;
      document.getElementById("protocol-messages").checked = protocols.messages !== false;
      document.getElementById("protocol-responses").checked = protocols.responses !== false;
      document.getElementById("protocol-gemini").checked = protocols.gemini !== false;
      const catalog = current.catalog || {};
      document.getElementById("refresh-interval").value = catalog["refresh-interval"] || "15m";
      document.getElementById("stale-enabled").checked = catalog["stale-while-unavailable"] !== false;
      document.getElementById("allow-http").checked = current["allow-http"] === true;
      const overrides = current["route-overrides"] || {};
      document.getElementById("route-overrides").value = JSON.stringify(overrides, null, 2);
      renderKeys();
      form.hidden = false;
    }
    function parseRouteOverrides() {
      const raw = document.getElementById("route-overrides").value.trim();
      const value = raw ? JSON.parse(raw) : {};
      if (!value || typeof value !== "object" || Array.isArray(value)) throw new Error("路由覆盖必须是 JSON 对象。");
      return value;
    }
    function sameJSON(a, b) { return JSON.stringify(a) === JSON.stringify(b); }
    function durationNanos(value, label) {
      const part = /(\d+(?:\.\d+)?)(ns|us|µs|ms|s|m|h)/gy;
      const scale = {ns: 1, us: 1000, "µs": 1000, ms: 1e6, s: 1e9, m: 60e9, h: 3600e9};
      let cursor = 0, total = 0, match;
      while (cursor < value.length) {
        part.lastIndex = cursor;
        match = part.exec(value);
        if (!match) throw new Error(label + "格式无效，例如 30s 或 5m。");
        total += Number(match[1]) * scale[match[2]];
        cursor = part.lastIndex;
      }
      if (!total || !Number.isFinite(total) || total > 9223372036854775807) throw new Error(label + "必须是有效的正数时间预算。");
      return total;
    }
    function validateURL(value, label, allowHTTP) {
      let url;
      try { url = new URL(value); } catch (_) { throw new Error(label + "不是有效 URL。"); }
      if (!url.hostname || url.username || url.password || url.search || url.hash) throw new Error(label + "不得包含账号密码、查询参数或片段。");
      if (url.protocol !== "https:" && !(allowHTTP && url.protocol === "http:")) throw new Error(label + "必须使用 HTTPS；仅本地测试可启用 HTTP。");
    }
    function validate() {
      if (apiKeysChanged) {
        const apiValues = apiKeys.map(keyValue);
        if (!apiValues.length || apiValues.some(value => !value.trim())) throw new Error("API Key 列表至少需要一项非空凭据。");
        if (new Set(apiValues.map(value => value.trim())).size !== apiValues.length) throw new Error("新的 API Key 列表有重复值。");
      }
      const allowHTTP = document.getElementById("allow-http").checked;
      const baseURL = document.getElementById("base-url").value.trim();
      validateURL(baseURL || "https://opencode.ai/inference/go/v1", "Base URL", allowHTTP);
      const catalogURL = document.getElementById("catalog-url").value.trim();
      if (catalogURL) validateURL(catalogURL, "模型目录 URL", allowHTTP);
      durationNanos(document.getElementById("request-timeout").value.trim(), "请求时间预算");
      if (durationNanos(document.getElementById("refresh-interval").value.trim(), "模型目录刷新间隔") < 60e9) throw new Error("模型目录刷新间隔至少为 1m。");
      const maxBytes = Number(document.getElementById("max-response-bytes").value);
      if (!Number.isSafeInteger(maxBytes) || maxBytes <= 0) throw new Error("最大响应大小必须是正整数。");
      if (document.getElementById("prefix-enabled").checked && !/^[A-Za-z0-9][A-Za-z0-9._-]*$/.test(document.getElementById("prefix-value").value.trim())) throw new Error("启用前缀时，只允许字母、数字、点、下划线和连字符，且首字符必须为字母或数字。");
      const overrides = parseRouteOverrides();
      const protocols = new Set(["chat-completions", "messages", "responses", "gemini"]);
      for (const [model, route] of Object.entries(overrides)) {
        if (!model.trim() || !route || typeof route !== "object" || !protocols.has(route.protocol) || (route.endpoint !== undefined && typeof route.endpoint !== "string") || (route.model !== undefined && typeof route.model !== "string") || (route.action !== undefined && typeof route.action !== "string")) throw new Error("路由覆盖中的模型名或协议无效。");
        if (route.protocol === "gemini") {
          if (route.endpoint && (!route.endpoint.startsWith("/") || /[?#]/.test(route.endpoint) || route.model || route.action)) throw new Error("Gemini endpoint 必须以 / 开头，且不能与 model/action 同时填写或含查询/片段。");
          if (route.model && route.model !== "{model}" && (!/^[A-Za-z0-9._-]+$/.test(route.model) || /^\.+$/.test(route.model))) throw new Error("Gemini model 只能包含字母、数字、点、下划线和连字符，且不能是点号路径。");
          if (route.action && !["generateContent", "streamGenerateContent"].includes(route.action)) throw new Error("Gemini action 仅支持 generateContent 或 streamGenerateContent。");
        } else if (!route.endpoint || !route.endpoint.startsWith("/") || route.model || route.action) {
          throw new Error("非 Gemini 路由必须提供 / 开头的 endpoint，且不能设置 model/action。");
        }
      }
    }
    function makePatch() {
      const patch = {};
      const putIfChanged = (key, value, previous) => { if (value !== previous) patch[key] = value; };
      const baseURL = document.getElementById("base-url").value.trim();
      putIfChanged("base-url", baseURL, fieldValue("base-url", "https://opencode.ai/inference/go/v1"));
      const catalogURL = document.getElementById("catalog-url").value.trim();
      const oldCatalogURL = fieldValue("catalog-url", "");
      if (catalogURL !== oldCatalogURL) patch["catalog-url"] = catalogURL || null;
      putIfChanged("request-timeout", document.getElementById("request-timeout").value.trim(), fieldValue("request-timeout", "5m"));
      putIfChanged("max-response-bytes", Number(document.getElementById("max-response-bytes").value), fieldValue("max-response-bytes", 67108864));

      const oldPrefix = current["model-prefix"] || {};
      const prefix = {...oldPrefix, enabled: document.getElementById("prefix-enabled").checked, value: document.getElementById("prefix-value").value.trim()};
      if (prefix.enabled !== (oldPrefix.enabled !== false) || prefix.value !== (oldPrefix.value || "opencode-go")) patch["model-prefix"] = prefix;

      const oldProtocols = current.protocols || {};
      const protocols = {...oldProtocols,
        "chat-completions": document.getElementById("protocol-chat").checked,
        messages: document.getElementById("protocol-messages").checked,
        responses: document.getElementById("protocol-responses").checked,
        gemini: document.getElementById("protocol-gemini").checked};
      if (protocols["chat-completions"] !== (oldProtocols["chat-completions"] !== false) || protocols.messages !== (oldProtocols.messages !== false) || protocols.responses !== (oldProtocols.responses !== false) || protocols.gemini !== (oldProtocols.gemini !== false)) patch.protocols = protocols;

      const oldCatalog = current.catalog || {};
      const catalog = {...oldCatalog,
        "refresh-interval": document.getElementById("refresh-interval").value.trim(),
        "stale-while-unavailable": document.getElementById("stale-enabled").checked};
      if (catalog["refresh-interval"] !== (oldCatalog["refresh-interval"] || "15m") || catalog["stale-while-unavailable"] !== (oldCatalog["stale-while-unavailable"] !== false)) patch.catalog = catalog;

      const allowHTTP = document.getElementById("allow-http").checked;
      putIfChanged("allow-http", allowHTTP, fieldValue("allow-http", false));
      const overrides = parseRouteOverrides();
      if (!sameJSON(overrides, current["route-overrides"] || {})) patch["route-overrides"] = overrides;
      if (apiKeysChanged) patch["api-keys"] = apiKeys;
      return patch;
    }
    document.getElementById("add-key").addEventListener("click", () => {
      apiKeys.push({value: "", remark: ""});
      apiKeysChanged = true;
      renderKeys();
      const inputs = keyList.querySelectorAll("input");
      if (inputs.length) inputs[inputs.length - 1].focus();
    });
    form.addEventListener("submit", async event => {
      event.preventDefault();
      const save = document.getElementById("save-settings");
      save.disabled = true;
      try {
        validate();
        const patch = makePatch();
        if (!Object.keys(patch).length) {
          showStatus("没有需要保存的更改。", "");
          return;
        }
        const syncQuotaNames = apiKeysChanged;
        const expectedRemarks = {
          remarks: apiKeys.map(entry => typeof entry.remark === "string" ? entry.remark.trim() : ""),
          previousRemarks: Array.isArray(current["api-keys"]) ? current["api-keys"].map(entry => entry && typeof entry.remark === "string" ? entry.remark.trim() : "") : []
        };
        await request(configURL, "PATCH", patch);
        if (syncQuotaNames) {
          const savedConfig = await request(configURL, "GET");
          const savedKeys = savedConfig && Array.isArray(savedConfig["api-keys"]) ? savedConfig["api-keys"] : [];
          const remarksPersisted = savedKeys.length === expectedRemarks.remarks.length && expectedRemarks.remarks.every((remark, index) => {
            const savedRemark = savedKeys[index] && typeof savedKeys[index].remark === "string" ? savedKeys[index].remark.trim() : "";
            return savedRemark === remark;
          });
          if (!remarksPersisted) throw new Error("CPA 接受了保存请求，但重新读取时备注字段不一致；请确认配置已写回，再重试。密钥内容未显示在错误信息中。");
        }
        showStatus("配置已写回 CPA；CPA 正在异步重载插件。" + (syncQuotaNames ? "额度页名称会在重载完成后自动同步。" : "") + "请检查 CPA 日志确认插件已成功应用。", "success");
        for (const [key, value] of Object.entries(patch)) {
          if (key === "catalog-url" && value === null) delete current[key];
          else current[key] = value;
        }
        apiKeysChanged = false;
        renderKeys();
        if (syncQuotaNames && typeof window.refreshQuotaCredentials === "function") {
          window.refreshQuotaCredentials(expectedRemarks).then(updated => {
            if (updated) showStatus("配置已保存，额度页凭据名称已同步。", "success");
            else showStatus("配置已保存；CPA 插件可能仍在重载，请稍后返回额度页或刷新页面。", "success");
          }).catch(() => showStatus("配置已保存；额度名称同步暂未完成，请稍后刷新额度页。", "success"));
        }
      } catch (error) {
        showStatus(error instanceof Error ? error.message : "保存失败，请检查 CPA 管理接口和配置。", "error");
      } finally {
        save.disabled = false;
      }
    });
    request(configURL, "GET").then(fill).catch(error => showStatus(error.message || "读取设置失败。", "error"));

    })();
  };
})();
