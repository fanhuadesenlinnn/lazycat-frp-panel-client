/* 懒猫 frp-panel 客户端 — 管理界面脚本
   纯原生 JS，无外部依赖。所有变更类请求都带自定义请求头，
   与服务端要求一致（X-Requested-With: lzc-frpp-client）。 */
(function () {
  "use strict";

  var HEADER_MARK = "lzc-frpp-client";
  var STATUS_INTERVAL = 2000;
  var LOG_INTERVAL = 2000;

  var STATE_TEXT = {
    stopped: "已停止",
    starting: "启动中",
    running: "运行中",
    backoff: "异常退出，等待重启",
    stopping: "停止中"
  };

  var logCursor = 0;
  var logCap = 800;
  var authed = false;
  var statusTimer = null;
  var logTimer = null;

  function $(id) { return document.getElementById(id); }

  function api(method, path, body) {
    var opts = {
      method: method,
      headers: {
        "X-Requested-With": HEADER_MARK,
        "Accept": "application/json"
      },
      credentials: "same-origin"
    };
    if (body !== undefined && body !== null) {
      opts.headers["Content-Type"] = "application/json";
      opts.body = JSON.stringify(body);
    }
    return fetch(path, opts).then(function (resp) {
      return resp.text().then(function (text) {
        var data = null;
        if (text) {
          try { data = JSON.parse(text); } catch (e) { data = null; }
        }
        if (!resp.ok) {
          var msg = (data && data.error) ? data.error : ("请求失败（HTTP " + resp.status + "）");
          var err = new Error(msg);
          err.status = resp.status;
          throw err;
        }
        return data;
      });
    });
  }

  function setAuthed(value) {
    authed = value;
    document.body.classList.toggle("authed", value);
    $("login-panel").hidden = value;
    $("btn-logout").hidden = !value;
    if (value) { startPolling(); } else { stopPolling(); }
  }

  function showLogin(message) {
    setAuthed(false);
    $("identity-chip").textContent = "身份：未认证";
    if (message) { showNotice("login-notice", message, "error"); }
  }

  function startPolling() {
    if (statusTimer === null) {
      // 先登记定时器再立即轮询一次，避免 pollStatus 内部再次调用
      // startPolling 时递归。
      statusTimer = window.setInterval(pollStatus, STATUS_INTERVAL);
      pollStatus();
    }
    if (logTimer === null) {
      logTimer = window.setInterval(pollLogs, LOG_INTERVAL);
      pollLogs();
    }
  }

  function stopPolling() {
    if (statusTimer !== null) { window.clearInterval(statusTimer); statusTimer = null; }
    if (logTimer !== null) { window.clearInterval(logTimer); logTimer = null; }
  }

  // 地址栏里的 ?token=... 只用于交换一次登录会话，随后立即从 URL 中移除，
  // 避免令牌长期残留在浏览历史里。
  function consumeUrlToken() {
    var token = null;
    try {
      var url = new URL(window.location.href);
      token = url.searchParams.get("token");
      if (token) {
        url.searchParams.delete("token");
        window.history.replaceState(null, "", url.pathname + url.search + url.hash);
      }
    } catch (e) { token = null; }
    return token;
  }

  function toast(message, kind) {
    var box = $("toasts");
    var el = document.createElement("div");
    el.className = "toast" + (kind === "error" ? " is-error" : kind === "ok" ? " is-ok" : "");
    el.textContent = message;
    box.appendChild(el);
    window.setTimeout(function () {
      el.style.opacity = "0";
      window.setTimeout(function () { el.remove(); }, 220);
    }, 4200);
  }

  function showNotice(id, text, kind) {
    var el = $(id);
    if (!text) { el.hidden = true; el.textContent = ""; return; }
    el.hidden = false;
    el.textContent = text;
    el.classList.toggle("is-error", kind === "error");
    el.classList.toggle("is-ok", kind === "ok");
  }

  function fmtDuration(seconds) {
    seconds = Math.max(0, Math.floor(seconds || 0));
    var h = Math.floor(seconds / 3600);
    var m = Math.floor((seconds % 3600) / 60);
    var s = seconds % 60;
    if (h > 0) { return h + " 小时 " + m + " 分"; }
    if (m > 0) { return m + " 分 " + s + " 秒"; }
    return s + " 秒";
  }

  function fmtClock(iso) {
    if (!iso) { return "—"; }
    var d = new Date(iso);
    if (isNaN(d.getTime())) { return "—"; }
    return d.toLocaleTimeString("zh-CN", { hour12: false });
  }

  function fmtExit(exit) {
    if (!exit) { return "无记录"; }
    var parts = [];
    if (exit.signal) { parts.push("信号 " + exit.signal); }
    else { parts.push("退出码 " + exit.code); }
    parts.push("运行 " + fmtDuration((exit.uptime_ms || 0) / 1000));
    parts.push(fmtClock(exit.at));
    return parts.join(" · ");
  }

  function setBusy(selector, busy) {
    Array.prototype.forEach.call(document.querySelectorAll(selector), function (el) {
      el.disabled = busy;
    });
  }

  function renderStatus(data) {
    var proc = data.process || {};
    var state = proc.state || "stopped";
    $("state-dot").setAttribute("data-state", state);
    $("state-label").textContent = STATE_TEXT[state] || state;

    $("m-pid").textContent = proc.pid ? String(proc.pid) : "—";
    $("m-uptime").textContent = state === "running" ? fmtDuration(proc.uptime_seconds) : "—";
    $("m-restarts").textContent = String(proc.restarts || 0);
    $("m-exit").textContent = fmtExit(proc.last_exit);

    var notice = "";
    var kind = "";
    if (proc.unstable) {
      notice = "客户端已连续 " + proc.consecutive_fast_failures + " 次快速退出，疑似配置错误或网络不通，请查看日志。";
      kind = "error";
    } else if (state === "backoff" && proc.next_restart_at) {
      var wait = Math.max(0, Math.round((new Date(proc.next_restart_at).getTime() - Date.now()) / 1000));
      notice = "上次异常退出，将在约 " + wait + " 秒后自动重启（退避重启）。";
    } else if (state === "stopped" && proc.last_error) {
      notice = proc.last_error;
      kind = "error";
    }
    showNotice("state-notice", notice, kind);

    var config = data.config || {};
    $("btn-start").disabled = state === "running" || state === "starting" || state === "stopping";
    $("btn-stop").disabled = state === "stopped" || state === "stopping";
    $("btn-restart").disabled = !config.configured;

    if (data.version) {
      $("version-chip").textContent = "版本 " + data.version;
      $("foot-version").textContent = data.version;
    }
    if (data.runtime) {
      $("foot-paths").textContent = "数据目录 " + data.runtime.data_dir +
        " · 客户端 " + (data.runtime.client_binary || "未找到");
      $("identity-chip").textContent = data.runtime.require_admin ? "身份：管理员入口" : "身份：任意登录用户";
    }

    var probe = (data.remote && data.remote.probe) || {};
    $("p-target").textContent = probe.target || "未配置";
    $("p-tcp").textContent = probe.tcp === "ok" ? "连接成功" : probe.tcp === "fail" ? "连接失败" : "未执行";
    if (probe.http === "ok") {
      $("p-http").textContent = "HTTP " + probe.http_status;
    } else if (probe.http === "fail") {
      $("p-http").textContent = "失败";
    } else {
      $("p-http").textContent = "未执行";
    }
    var t = [];
    if (typeof probe.latency_ms === "number" && probe.at) { t.push(probe.latency_ms + " ms"); }
    if (probe.at) { t.push(fmtClock(probe.at)); }
    if (probe.error) { t.push(probe.error); }
    $("p-time").textContent = t.length ? t.join(" · ") : "—";

    applyConfigToForm(data.config || {}, false);
  }

  function applyConfigToForm(config, force) {
    if (!force && document.activeElement && document.activeElement.closest && document.activeElement.closest("#config-form")) {
      // 不要覆盖用户正在输入的内容
    } else {
      if (config.client_id !== undefined) { $("f-client-id").value = config.client_id || ""; }
      if (config.api_url !== undefined) { $("f-api-url").value = config.api_url || ""; }
      if (config.rpc_url !== undefined) { $("f-rpc-url").value = config.rpc_url || ""; }
    }
    if (config.log_level) { $("f-log-level").value = config.log_level; }
    $("f-auto-start").checked = !!config.auto_start;
    $("f-functions").checked = !!config.enable_functions;
    $("f-remote-shell").checked = !!config.enable_remote_shell;
    $("secret-hint").textContent = config.secret_set
      ? ("已保存密钥（长度 " + config.secret_length + "），留空表示不修改")
      : "尚未保存密钥";
  }

  function renderLogs(lines, dropped) {
    if (dropped) {
      var warn = document.createElement("span");
      warn.className = "ln";
      warn.textContent = "… 早期日志已超出内存缓冲，被丢弃 …";
      $("log-view").appendChild(warn);
    }
    var view = $("log-view");
    var follow = $("log-follow").checked;
    lines.forEach(function (line) {
      var span = document.createElement("span");
      span.className = "ln";
      var ts = document.createElement("span");
      ts.className = "ln-ts";
      ts.textContent = fmtClock(line.time);
      span.appendChild(ts);
      span.appendChild(document.createTextNode(line.text));
      view.appendChild(span);
    });
    if (lines.length) {
      logCursor = lines[lines.length - 1].seq;
    }
    // 只保留有限行数，避免长时间运行后页面卡顿
    while (view.childElementCount > logCap) {
      view.removeChild(view.firstChild);
    }
    if (follow && lines.length) {
      view.scrollTop = view.scrollHeight;
    }
  }

  function pollStatus() {
    api("GET", "/api/status").then(function (data) {
      renderStatus(data);
      setAuthed(true);
    }).catch(function (err) {
      if (err.status === 401) {
        showLogin(err.message);
        return;
      }
      $("state-label").textContent = "无法获取状态";
      $("state-dot").setAttribute("data-state", "stopped");
      showNotice("state-notice", err.message, "error");
    });
  }

  function pollLogs() {
    api("GET", "/api/logs?since=" + logCursor).then(function (data) {
      renderLogs(data.lines || [], data.dropped);
    }).catch(function () { /* 静默重试 */ });
  }

  function control(action) {
    setBusy(".actions .btn", true);
    api("POST", "/api/control", { action: action }).then(function () {
      toast(action === "start" ? "已发送启动指令" : action === "stop" ? "已发送停止指令" : "已发送重启指令", "ok");
      pollStatus();
    }).catch(function (err) {
      toast(err.message, "error");
    }).then(function () {
      setBusy(".actions .btn", false);
    });
  }

  function collectConfig() {
    var body = {
      client_id: $("f-client-id").value.trim(),
      api_url: $("f-api-url").value.trim(),
      rpc_url: $("f-rpc-url").value.trim(),
      log_level: $("f-log-level").value,
      auto_start: $("f-auto-start").checked,
      enable_functions: $("f-functions").checked,
      enable_remote_shell: $("f-remote-shell").checked
    };
    var secret = $("f-secret").value;
    if (secret) { body.client_secret = secret; }
    return body;
  }

  function saveConfig(event) {
    if (event) { event.preventDefault(); }
    setBusy("#config-form button", true);
    api("POST", "/api/config", collectConfig()).then(function (data) {
      $("f-secret").value = "";
      applyConfigToForm(data.config, true);
      var msg = "配置已保存";
      if (data.restarted) { msg += "，客户端已重启以应用新配置"; }
      showNotice("config-notice", msg, "ok");
      toast(msg, "ok");
      pollStatus();
    }).catch(function (err) {
      showNotice("config-notice", err.message, "error");
      toast(err.message, "error");
    }).then(function () {
      setBusy("#config-form button", false);
    });
  }

  function renderPreview(preview) {
    var list = $("parsed-list");
    list.innerHTML = "";
    var rows = [
      ["客户端 ID", preview.client_id || "（未识别）"],
      ["密钥", preview.secret_detected ? "已识别（长度 " + (preview.secret_length || "?") + "，不会回传）" : "（未识别）"],
      ["API URL", preview.api_url || "（未识别）"],
      ["RPC URL", preview.rpc_url || "（未识别）"],
      ["命令中的程序", preview.program_hint || "（未提供）"]
    ];
    rows.forEach(function (row) {
      var div = document.createElement("div");
      var dt = document.createElement("dt");
      dt.textContent = row[0];
      var dd = document.createElement("dd");
      dd.textContent = row[1];
      div.appendChild(dt);
      div.appendChild(dd);
      list.appendChild(div);
    });
    var warnBox = $("parsed-warnings");
    warnBox.innerHTML = "";
    (preview.warnings || []).forEach(function (w) {
      var li = document.createElement("li");
      li.textContent = w;
      warnBox.appendChild(li);
    });
    if (preview.missing && preview.missing.length) {
      var li2 = document.createElement("li");
      li2.textContent = "缺少字段：" + preview.missing.join("、");
      warnBox.appendChild(li2);
    }
    $("parse-result").hidden = false;
  }

  function previewCommand() {
    var command = $("command-input").value;
    setBusy("#btn-preview", true);
    api("POST", "/api/command/preview", { command: command }).then(function (data) {
      renderPreview(data.preview);
      showNotice("command-notice", "解析成功，可点击“解析、保存并启动”写入配置。", "ok");
    }).catch(function (err) {
      $("parse-result").hidden = true;
      showNotice("command-notice", err.message, "error");
    }).then(function () {
      setBusy("#btn-preview", false);
    });
  }

  function applyCommand() {
    var command = $("command-input").value;
    setBusy("#btn-apply", true);
    api("POST", "/api/command/apply", { command: command, start: true }).then(function (data) {
      (data.warnings || []).forEach(function (w) { toast(w); });
      $("command-input").value = "";
      $("parse-result").hidden = true;
      applyConfigToForm(data.config, true);
      showNotice("command-notice", "配置已写入，并已请求启动客户端。", "ok");
      toast("配置已写入并启动", "ok");
      pollStatus();
    }).catch(function (err) {
      showNotice("command-notice", err.message, "error");
      toast(err.message, "error");
    }).then(function () {
      setBusy("#btn-apply", false);
    });
  }

  function reloadConfig() {
    api("GET", "/api/config").then(function (data) {
      applyConfigToForm(data.config, true);
      showNotice("config-notice", "已从服务端重新载入配置。", null);
    }).catch(function (err) {
      showNotice("config-notice", err.message, "error");
    });
  }

  function clearLogs() {
    api("POST", "/api/logs/clear", {}).then(function () {
      $("log-view").innerHTML = "";
      logCursor = 0;
      toast("日志已清空", "ok");
    }).catch(function (err) { toast(err.message, "error"); });
  }

  // 访问令牌登录：令牌只通过同源 JSON 请求体发送一次，服务端换取
  // HttpOnly 会话 Cookie，浏览器脚本读不到它。
  function submitLogin(event) {
    if (event) { event.preventDefault(); }
    var token = $("f-token").value.trim();
    if (!token) {
      showNotice("login-notice", "请输入访问令牌", "error");
      return;
    }
    setBusy("#login-form button", true);
    api("POST", "/api/login", { token: token }).then(function () {
      $("f-token").value = "";
      showNotice("login-notice", "", null);
      return api("GET", "/api/status");
    }).then(function (data) {
      renderStatus(data);
      setAuthed(true);
      toast("登录成功", "ok");
    }).catch(function (err) {
      showNotice("login-notice", err.message, "error");
    }).then(function () {
      setBusy("#login-form button", false);
    });
  }

  function doLogout() {
    setBusy("#btn-logout", true);
    api("POST", "/api/logout", {}).then(function () {
      $("log-view").innerHTML = "";
      logCursor = 0;
      showLogin(null);
      toast("已退出登录", "ok");
    }).catch(function (err) {
      toast(err.message, "error");
    }).then(function () {
      setBusy("#btn-logout", false);
    });
  }

  // 启动时：先用地址栏里一次性的 ?token= 换取会话（并立即把它从 URL 中移除），
  // 否则直接尝试访问 API —— 懒猫入口会自动带上身份，独立部署则回到登录表单。
  function bootstrap() {
    var urlToken = consumeUrlToken();
    if (urlToken) {
      setBusy("#login-form button", true);
      api("POST", "/api/login", { token: urlToken }).then(function () {
        return api("GET", "/api/status");
      }).then(function (data) {
        renderStatus(data);
        setAuthed(true);
      }).catch(function (err) {
        showLogin("自动登录失败：" + err.message);
      }).then(function () {
        setBusy("#login-form button", false);
      });
      return;
    }
    api("GET", "/api/status").then(function (data) {
      renderStatus(data);
      setAuthed(true);
    }).catch(function (err) {
      showLogin(err.status === 401 ? err.message : ("无法连接到管理器：" + err.message));
    });
  }

  function init() {
    $("btn-start").addEventListener("click", function () { control("start"); });
    $("btn-stop").addEventListener("click", function () { control("stop"); });
    $("btn-restart").addEventListener("click", function () { control("restart"); });
    $("config-form").addEventListener("submit", saveConfig);
    $("btn-reload").addEventListener("click", reloadConfig);
    $("btn-preview").addEventListener("click", previewCommand);
    $("btn-apply").addEventListener("click", applyCommand);
    $("btn-log-clear").addEventListener("click", clearLogs);
    $("login-form").addEventListener("submit", submitLogin);
    $("btn-logout").addEventListener("click", doLogout);
    $("log-cap").textContent = String(logCap);

    bootstrap();
  }

  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", init);
  } else {
    init();
  }
})();
