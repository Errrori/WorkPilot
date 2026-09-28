"use strict";

const $ = (id) => document.getElementById(id);

const esc = (value) =>
  String(value ?? "").replace(/[&<>"']/g, (ch) => ({
    "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;",
  }[ch]));

const fmtTime = (iso) => {
  const date = new Date(iso);
  return Number.isNaN(date.getTime()) ? "" : date.toLocaleString("zh-CN", { hour12: false });
};

const fmtSize = (bytes) => {
  if (bytes < 1024) return `${bytes} B`;
  if (bytes < 1048576) return `${(bytes / 1024).toFixed(1)} KB`;
  return `${(bytes / 1048576).toFixed(1)} MB`;
};

const PRIORITY = { low: "低", medium: "中", high: "高" };
const PARSE_BADGE = {
  pending: ["等待解析", "warn"],
  parsing: ["解析中", "warn"],
  parsed: ["已解析", "ok"],
  failed: ["解析失败", "err"],
  unsupported: ["不支持", "muted"],
};
const INDEX_BADGE = {
  pending: ["等待索引", "warn"],
  indexing: ["索引中", "warn"],
  indexed: ["已索引", "ok"],
  failed: ["索引失败", "err"],
  skipped: ["跳过", "muted"],
};
const TASK_COLUMNS = [
  ["suggested", "待确认"],
  ["todo", "待办"],
  ["doing", "进行中"],
  ["done", "已完成"],
  ["rejected", "已忽略"],
];
const SEVERITY = { low: "低", medium: "中", high: "高" };
const RISK_COLUMNS = [
  ["suggested", "待确认"],
  ["open", "待处理"],
  ["mitigating", "处理中"],
  ["resolved", "已解决"],
  ["dismissed", "已忽略"],
];
const SOURCE_LABEL = { messages: "消息", tasks: "任务", risks: "风险", files: "文件" };
const CALL_SOURCE_LABEL = { qa: "问答", task_extract: "任务抽取", risk_extract: "风险识别", ai_task: "定时 AI 任务", unknown: "未知" };
const REPORT_BADGE = { succeeded: ["成功", "ok"], failed: ["失败", "err"] };
const TASK_STATUS_LABEL = Object.fromEntries(TASK_COLUMNS);
const RISK_STATUS_LABEL = Object.fromEntries(RISK_COLUMNS);

const state = {
  groups: [],
  groupId: "",
  user: localStorage.getItem("wp_user") || "alice",
  messages: [],
  files: [],
  tasks: [],
  risks: [],
  aiTasks: [],
  reports: [],
  ws: null,
  wsTimer: null,
  streaming: false,
};

async function api(path, options = {}) {
  const res = await fetch(path, options);
  const text = await res.text();
  let data = null;
  try {
    data = text ? JSON.parse(text) : null;
  } catch {
    data = null;
  }
  if (!res.ok) throw new Error((data && data.error) || `HTTP ${res.status}`);
  return data;
}

let toastTimer = null;
function toast(message, isError = false) {
  const el = $("toast");
  el.textContent = message;
  el.className = `toast show${isError ? " error" : ""}`;
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => { el.className = "toast"; }, 3500);
}

function setWsStatus(connected) {
  const el = $("ws-status");
  el.className = `dot ${connected ? "on" : "off"}`;
  el.title = connected ? "WebSocket 已连接" : "WebSocket 未连接";
}

function openModal(title, body) {
  $("modal-title").textContent = title;
  $("modal-body").textContent = body;
  $("modal").classList.add("show");
}

function closeModal() {
  $("modal").classList.remove("show");
}

async function loadGroups() {
  const data = await api("/api/groups");
  state.groups = data.groups || [];
  $("group-select").innerHTML = state.groups
    .map((g) => `<option value="${esc(g.id)}">${esc(g.name)}</option>`)
    .join("");
}

async function selectGroup(groupId) {
  state.groupId = groupId;
  localStorage.setItem("wp_group", groupId);
  connectWS();
  try {
    await Promise.all([loadMessages(), loadFiles(), loadTasks(), loadRisks(), loadAiTasks(), loadReports(), loadUsage()]);
  } catch (error) {
    toast(error.message, true);
  }
}

function connectWS() {
  if (state.ws) {
    state.ws.onclose = null;
    state.ws.close();
    state.ws = null;
  }
  if (!state.groupId || !state.user) return;
  const proto = location.protocol === "https:" ? "wss" : "ws";
  const ws = new WebSocket(
    `${proto}://${location.host}/ws?group_id=${encodeURIComponent(state.groupId)}&user=${encodeURIComponent(state.user)}`
  );
  state.ws = ws;
  ws.onopen = () => setWsStatus(true);
  ws.onmessage = (event) => {
    try {
      handleEvent(JSON.parse(event.data));
    } catch {
      /* ignore malformed payloads */
    }
  };
  ws.onclose = () => {
    setWsStatus(false);
    if (state.ws !== ws) return;
    state.ws = null;
    clearTimeout(state.wsTimer);
    state.wsTimer = setTimeout(connectWS, 2000);
  };
  ws.onerror = () => ws.close();
}

function handleEvent(event) {
  switch (event.type) {
    case "message":
      upsertMessage(event.message);
      break;
    case "error":
      toast(event.error || "WebSocket 错误", true);
      break;
    case "file_uploaded":
    case "file_parsed":
    case "file_parse_failed":
    case "file_indexed":
    case "file_index_failed":
      upsertFile(event.file);
      break;
    case "file_deleted":
      state.files = state.files.filter((f) => f.id !== event.file.id);
      renderFiles();
      break;
    case "task_suggested":
    case "task_created":
    case "task_updated":
      upsertTask(event.task);
      break;
    case "task_deleted":
      state.tasks = state.tasks.filter((t) => t.id !== event.task.id);
      renderTasks();
      break;
    case "risk_suggested":
    case "risk_created":
    case "risk_updated":
      upsertRisk(event.risk);
      break;
    case "risk_deleted":
      state.risks = state.risks.filter((r) => r.id !== event.risk.id);
      renderRisks();
      break;
    case "ai_task_created":
    case "ai_task_updated":
      upsertAiTask(event.ai_task);
      break;
    case "ai_task_deleted":
      state.aiTasks = state.aiTasks.filter((t) => t.id !== event.ai_task.id);
      renderAiTasks();
      break;
    case "report_created":
      upsertReport(event.report);
      toast(`新报告：${event.report.title}`);
      break;
  }
}

async function loadMessages() {
  const data = await api(`/api/groups/${state.groupId}/messages?limit=100`);
  state.messages = data.messages || [];
  renderMessages();
  const box = $("messages");
  box.scrollTop = box.scrollHeight;
}

function upsertMessage(message) {
  if (!message || message.group_id !== state.groupId) return;
  if (!state.messages.some((m) => m.id === message.id)) state.messages.push(message);
  renderMessages();
  const box = $("messages");
  box.scrollTop = box.scrollHeight;
}

function renderMessages() {
  const box = $("messages");
  if (!state.messages.length) {
    box.innerHTML = '<div class="empty">暂无消息，先发一条吧</div>';
    return;
  }
  box.innerHTML = state.messages.map((m) => {
    const citations = (m.citations || []).map((c) =>
      `<a href="#" class="cite" data-file="${c.file_id}" data-chunk="${c.chunk_index}">[${c.index}] ${esc(c.file_name)} #${c.chunk_index}</a>`
    ).join(" ");
    return `<div class="msg${m.sender_name === state.user ? " mine" : ""}">
      <div class="msg-meta">${esc(m.sender_name)} · ${fmtTime(m.created_at)}</div>
      <div class="msg-body">${esc(m.content).replace(/\n/g, "<br>")}</div>
      ${citations ? `<div class="cites">${citations}</div>` : ""}
    </div>`;
  }).join("");
}

async function loadFiles() {
  const data = await api(`/api/groups/${state.groupId}/files`);
  state.files = data.files || [];
  renderFiles();
}

function upsertFile(file) {
  if (!file || file.group_id !== state.groupId) return;
  const index = state.files.findIndex((f) => f.id === file.id);
  if (index >= 0) state.files[index] = file;
  else state.files.unshift(file);
  renderFiles();
}

function renderFiles() {
  const body = $("files-body");
  if (!state.files.length) {
    body.innerHTML = '<tr><td colspan="7" class="empty">暂无文件，先上传一份会议纪要或 PRD 试试</td></tr>';
    return;
  }
  body.innerHTML = state.files.map((f) => {
    const [parseText, parseClass] = PARSE_BADGE[f.parse_status] || [f.parse_status, "muted"];
    const [indexText, indexClass] = INDEX_BADGE[f.index_status] || [f.index_status, "muted"];
    return `<tr>
      <td class="fname">${esc(f.file_name)}
        ${f.parse_error ? `<div class="err-text">${esc(f.parse_error)}</div>` : ""}
        ${f.index_error ? `<div class="err-text">${esc(f.index_error)}</div>` : ""}
      </td>
      <td>${fmtSize(f.size_bytes)}</td>
      <td>${esc(f.uploader_name)}</td>
      <td><span class="badge ${parseClass}">${parseText}</span></td>
      <td><span class="badge ${indexClass}">${indexText}</span></td>
      <td>${f.chunk_count || 0}</td>
      <td class="ops">
        <a class="btn" href="/api/groups/${state.groupId}/files/${f.id}/download">下载</a>
        <button data-action="content" data-id="${f.id}" type="button">内容</button>
        <button data-action="chunks" data-id="${f.id}" type="button">分块</button>
        <button data-action="parse" data-id="${f.id}" type="button"${f.parse_status === "parsing" ? " disabled" : ""}>重试解析</button>
        <button data-action="index" data-id="${f.id}" type="button"${f.parse_status !== "parsed" || f.index_status === "indexing" ? " disabled" : ""}>重建索引</button>
        <button data-action="extract" data-id="${f.id}" type="button">抽取建议</button>
        <button data-action="delete" data-id="${f.id}" class="danger" type="button">删除</button>
      </td>
    </tr>`;
  }).join("");
}

async function openFileContent(fileId, chunkIndex) {
  try {
    const data = await api(`/api/groups/${state.groupId}/files/${fileId}/chunks`);
    const chunks = data.chunks || [];
    const text = chunks.map((c) => {
      const mark = chunkIndex != null && c.chunk_index === chunkIndex ? "  <-- 引用位置" : "";
      return `#${c.chunk_index}${mark}  (${c.char_count} 字)\n${c.content}`;
    }).join("\n\n────────\n\n");
    openModal(`文件分块（file #${fileId}，共 ${chunks.length} 块）`, text);
  } catch {
    try {
      const content = await api(`/api/groups/${state.groupId}/files/${fileId}/content`);
      openModal(`解析内容（file #${fileId}）`, content.content);
    } catch (error) {
      toast(error.message, true);
    }
  }
}

async function loadTasks() {
  const data = await api(`/api/groups/${state.groupId}/tasks?limit=500`);
  state.tasks = data.tasks || [];
  renderTasks();
}

function upsertTask(task) {
  if (!task || task.group_id !== state.groupId) return;
  const index = state.tasks.findIndex((t) => t.id === task.id);
  if (index >= 0) state.tasks[index] = task;
  else state.tasks.unshift(task);
  renderTasks();
}

function taskCard(task) {
  const citations = (task.citations || []).map((c) =>
    `<a href="#" class="cite" data-file="${c.file_id}" data-chunk="${c.chunk_index}">[${c.index}] ${esc(c.file_name)}</a>`
  ).join(" ");
  const actions = [];
  if (task.status === "suggested") actions.push(["todo", "采纳"], ["rejected", "忽略"]);
  else if (task.status === "todo") actions.push(["doing", "开始"], ["done", "完成"]);
  else if (task.status === "doing") actions.push(["done", "完成"]);
  else if (task.status === "done") actions.push(["todo", "重开"]);
  else if (task.status === "rejected") actions.push(["todo", "恢复"]);
  actions.push(["assign", "指派"], ["edit", "改标题"], ["delete", "删除"]);
  return `<div class="card pri-${task.priority}">
    <div class="card-title">${esc(task.title)}</div>
    ${task.description ? `<div class="card-desc">${esc(task.description)}</div>` : ""}
    <div class="card-meta">
      <span class="badge">${PRIORITY[task.priority] || esc(task.priority)}优先级</span>
      <span class="badge">${task.assignee ? esc(task.assignee) : "未指派"}</span>
      <span class="badge">${task.source === "extracted" ? "AI 建议" : "手工"}</span>
      ${task.confirmed_by ? `<span class="badge">${esc(task.confirmed_by)} 确认</span>` : ""}
    </div>
    ${citations ? `<div class="cites">${citations}</div>` : ""}
    <div class="card-actions">
      ${actions.map(([action, label]) =>
        `<button data-action="${action}" data-id="${task.id}" type="button"${action === "delete" ? ' class="danger"' : ""}>${label}</button>`
      ).join("")}
    </div>
  </div>`;
}

function renderTasks() {
  $("board").innerHTML = TASK_COLUMNS.map(([status, label]) => {
    const items = state.tasks.filter((t) => t.status === status);
    return `<div class="column">
      <div class="column-head">${label}<span class="count">${items.length}</span></div>
      <div class="column-body">
        ${items.map(taskCard).join("") || '<div class="empty small">暂无</div>'}
      </div>
    </div>`;
  }).join("");
}

async function patchTask(taskId, patch) {
  try {
    await api(`/api/groups/${state.groupId}/tasks/${taskId}`, {
      method: "PATCH",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ user: state.user, ...patch }),
    });
  } catch (error) {
    toast(error.message, true);
  }
}

async function loadRisks() {
  const data = await api(`/api/groups/${state.groupId}/risks?limit=500`);
  state.risks = data.risks || [];
  renderRisks();
}

function upsertRisk(risk) {
  if (!risk || risk.group_id !== state.groupId) return;
  const index = state.risks.findIndex((r) => r.id === risk.id);
  if (index >= 0) state.risks[index] = risk;
  else state.risks.unshift(risk);
  renderRisks();
}

function riskCard(risk) {
  const citations = (risk.citations || []).map((c) =>
    `<a href="#" class="cite" data-file="${c.file_id}" data-chunk="${c.chunk_index}">[${c.index}] ${esc(c.file_name)}</a>`
  ).join(" ");
  const related = (risk.related_task_ids || []).map((id) =>
    `<span class="badge">任务 #${id}</span>`
  ).join(" ");
  const actions = [];
  if (risk.status === "suggested") actions.push(["open", "确认"], ["dismissed", "忽略"]);
  else if (risk.status === "open") actions.push(["mitigating", "开始处理"], ["resolved", "已解决"]);
  else if (risk.status === "mitigating") actions.push(["resolved", "已解决"]);
  else if (risk.status === "resolved") actions.push(["open", "重开"]);
  else if (risk.status === "dismissed") actions.push(["open", "恢复"]);
  actions.push(["owner", "跟进人"], ["edit", "改标题"], ["delete", "删除"]);
  return `<div class="card pri-${risk.severity}">
    <div class="card-title">${esc(risk.title)}</div>
    ${risk.description ? `<div class="card-desc">${esc(risk.description)}</div>` : ""}
    <div class="card-meta">
      <span class="badge">${SEVERITY[risk.severity] || esc(risk.severity)}风险</span>
      <span class="badge">${risk.owner ? esc(risk.owner) : "未指派"}</span>
      <span class="badge">${risk.source === "extracted" ? "AI 识别" : "手工"}</span>
      ${risk.confirmed_by ? `<span class="badge">${esc(risk.confirmed_by)} 确认</span>` : ""}
      ${related}
    </div>
    ${citations ? `<div class="cites">${citations}</div>` : ""}
    <div class="card-actions">
      ${actions.map(([action, label]) =>
        `<button data-action="${action}" data-id="${risk.id}" type="button"${action === "delete" ? ' class="danger"' : ""}>${label}</button>`
      ).join("")}
    </div>
  </div>`;
}

function renderRisks() {
  $("risk-board").innerHTML = RISK_COLUMNS.map(([status, label]) => {
    const items = state.risks.filter((r) => r.status === status);
    return `<div class="column">
      <div class="column-head">${label}<span class="count">${items.length}</span></div>
      <div class="column-body">
        ${items.map(riskCard).join("") || '<div class="empty small">暂无</div>'}
      </div>
    </div>`;
  }).join("");
}

async function patchRisk(riskId, patch) {
  try {
    await api(`/api/groups/${state.groupId}/risks/${riskId}`, {
      method: "PATCH",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ user: state.user, ...patch }),
    });
  } catch (error) {
    toast(error.message, true);
  }
}

async function loadAiTasks() {
  const data = await api(`/api/groups/${state.groupId}/ai-tasks`);
  state.aiTasks = data.ai_tasks || [];
  renderAiTasks();
}

function upsertAiTask(task) {
  if (!task || task.group_id !== state.groupId) return;
  const index = state.aiTasks.findIndex((t) => t.id === task.id);
  if (index >= 0) state.aiTasks[index] = task;
  else state.aiTasks.unshift(task);
  renderAiTasks();
}

function aiTaskCard(task) {
  const sources = (task.sources || []).map((s) => SOURCE_LABEL[s] || s).join(" / ");
  const last = task.last_status
    ? `<span class="badge ${task.last_status === "succeeded" ? "ok" : "err"}">上次${task.last_status === "succeeded" ? "成功" : "失败"}</span>`
    : "";
  return `<div class="card${task.enabled ? "" : " muted-card"}">
    <div class="card-title">${esc(task.name)} <span class="badge ${task.enabled ? "ok" : ""}">${task.enabled ? "启用" : "停用"}</span></div>
    ${task.prompt ? `<div class="card-desc">${esc(task.prompt)}</div>` : ""}
    <div class="card-meta">
      <span class="badge">cron ${esc(task.schedule)}</span>
      <span class="badge">${esc(task.timezone)}</span>
      <span class="badge">回看 ${task.lookback_days} 天</span>
      <span class="badge">素材 ${esc(sources)}</span>
    </div>
    <div class="card-meta">
      <span class="badge">下次 ${fmtTime(task.next_run_at)}</span>
      ${task.last_run_at ? `<span class="badge">上次运行 ${fmtTime(task.last_run_at)}</span>` : ""}
      ${last}
    </div>
    ${task.last_error ? `<div class="err-text">${esc(task.last_error)}</div>` : ""}
    <div class="card-actions">
      <button data-action="run" data-id="${task.id}" type="button">立即生成</button>
      <button data-action="toggle" data-id="${task.id}" type="button">${task.enabled ? "停用" : "启用"}</button>
      <button data-action="delete" data-id="${task.id}" class="danger" type="button">删除</button>
    </div>
  </div>`;
}

function renderAiTasks() {
  $("ai-task-count").textContent = state.aiTasks.length;
  $("ai-task-list").innerHTML = state.aiTasks.map(aiTaskCard).join("")
    || '<div class="empty small">暂无定时 AI 任务，点「周报模板」快速创建</div>';
}

async function loadReports() {
  const data = await api(`/api/groups/${state.groupId}/reports?limit=30`);
  state.reports = data.reports || [];
  renderReports();
}

function upsertReport(report) {
  if (!report || report.group_id !== state.groupId) return;
  const index = state.reports.findIndex((r) => r.id === report.id);
  if (index >= 0) state.reports[index] = report;
  else state.reports.unshift(report);
  renderReports();
}

function reportPeriodText(report) {
  if (!report.period_start || !report.period_end) return "";
  const start = new Date(report.period_start);
  const end = new Date(report.period_end);
  const format = (d) => Number.isNaN(d.getTime()) ? "" : d.toLocaleDateString("zh-CN");
  return start.toDateString() === end.toDateString()
    ? format(end)
    : `${format(start)} ~ ${format(end)}`;
}

function reportMetricsText(report) {
  const metrics = report.metrics || {};
  const parts = [`消息 ${metrics.messages || 0}`, `文件 ${metrics.files || 0}`];
  const tasks = Object.entries(metrics.tasks || {}).map(([k, v]) => `${TASK_STATUS_LABEL[k] || k} ${v}`).join(" / ");
  const risks = Object.entries(metrics.risks || {}).map(([k, v]) => `${RISK_STATUS_LABEL[k] || k} ${v}`).join(" / ");
  if (tasks) parts.push(`任务 ${tasks}`);
  if (risks) parts.push(`风险 ${risks}`);
  return parts.join(" · ");
}

function reportCard(report) {
  const [statusText, statusClass] = REPORT_BADGE[report.status] || [report.status, "muted"];
  const period = reportPeriodText(report);
  return `<div class="card">
    <div class="card-title">${esc(report.title)}</div>
    <div class="card-meta">
      <span class="badge ${statusClass}">${statusText}</span>
      <span class="badge">${report.trigger === "manual" ? "手动" : "定时"}</span>
      ${period ? `<span class="badge">${esc(period)}</span>` : ""}
      ${report.ai_task_id ? `<span class="badge">任务 #${report.ai_task_id}</span>` : ""}
      <span class="badge">${fmtTime(report.created_at)}</span>
    </div>
    <div class="card-desc">${esc(reportMetricsText(report))}</div>
    ${report.error ? `<div class="err-text">${esc(report.error)}</div>` : ""}
    <div class="card-actions">
      <button data-action="view" data-id="${report.id}" type="button">查看</button>
      <button data-action="delete" data-id="${report.id}" class="danger" type="button">删除</button>
    </div>
  </div>`;
}

function renderReports() {
  $("report-count").textContent = state.reports.length;
  $("report-list").innerHTML = state.reports.map(reportCard).join("")
    || '<div class="empty small">暂无报告，创建任务后点「立即生成」或等待定时触发</div>';
}

function openReport(report) {
  const [statusText] = REPORT_BADGE[report.status] || [report.status];
  const meta = [
    `状态：${statusText} · ${report.trigger === "manual" ? "手动触发" : "定时触发"}`,
    reportPeriodText(report) ? `周期：${reportPeriodText(report)}` : "",
    reportMetricsText(report),
    report.error ? `错误：${report.error}` : "",
  ].filter(Boolean).join("\n");
  openModal(report.title, `${meta}\n\n${report.content || "（无正文）"}`);
}

async function loadUsage() {
  const days = Number($("usage-window").value) || 7;
  const from = new Date(Date.now() - days * 86400000 - 60000).toISOString();
  const data = await api(`/api/groups/${state.groupId}/usage?from=${encodeURIComponent(from)}&limit=20`);
  renderUsage(data);
}

function fmtTokens(value) {
  return Number(value || 0).toLocaleString("zh-CN");
}

function renderUsage(data) {
  const summary = data.summary || {};
  const pricing = data.pricing || {};
  const hasPricing = (pricing.input_per_mtok || 0) > 0 || (pricing.output_per_mtok || 0) > 0;
  const costText = hasPricing ? `${Number(summary.cost || 0).toFixed(4)} ${esc(pricing.currency || "")}` : "未配置单价";
  const windowText = data.window ? `${fmtTime(data.window.from)} ~ ${fmtTime(data.window.to)}` : "";
  $("usage-summary").innerHTML = [
    ["调用次数", fmtTokens(summary.calls), `${fmtTokens(summary.failed)} 次失败`],
    ["总 tokens", fmtTokens(summary.total_tokens), `Prompt ${fmtTokens(summary.prompt_tokens)} / Completion ${fmtTokens(summary.completion_tokens)}`],
    ["预估成本", costText, `输入 ${pricing.input_per_mtok ?? 0} / 输出 ${pricing.output_per_mtok ?? 0}（每百万 tokens）`],
    ["平均延迟", `${fmtTokens(summary.avg_latency_ms)} ms`, windowText],
  ].map(([label, value, hint]) => `<div class="usage-card">
      <div class="usage-label">${label}</div>
      <div class="usage-value">${value}</div>
      <div class="usage-hint">${hint}</div>
    </div>`).join("");

  const sources = data.by_source || [];
  $("usage-by-source").innerHTML = sources.map((s) => `<tr>
      <td>${CALL_SOURCE_LABEL[s.source] || esc(s.source)}</td>
      <td>${fmtTokens(s.calls)}</td>
      <td>${fmtTokens(s.failed)}</td>
      <td>${fmtTokens(s.prompt_tokens)}</td>
      <td>${fmtTokens(s.completion_tokens)}</td>
      <td>${fmtTokens(s.total_tokens)}</td>
      <td>${fmtTokens(s.avg_latency_ms)} ms</td>
      <td>${Number(s.cost || 0).toFixed(4)}</td>
    </tr>`).join("") || '<tr><td colspan="8" class="empty small">窗口内暂无调用</td></tr>';

  const recent = data.recent || [];
  $("usage-recent").innerHTML = recent.map((u) => `<tr>
      <td>${fmtTime(u.created_at)}</td>
      <td>${CALL_SOURCE_LABEL[u.source] || esc(u.source)}</td>
      <td>${esc(u.model)}</td>
      <td>${fmtTokens(u.total_tokens)}</td>
      <td>${fmtTokens(u.latency_ms)} ms</td>
      <td>${u.status === "succeeded" ? '<span class="badge ok">成功</span>' : `<span class="badge err">失败</span>${u.error ? `<div class="err-text">${esc(u.error)}</div>` : ""}`}</td>
    </tr>`).join("") || '<tr><td colspan="6" class="empty small">暂无调用记录</td></tr>';
}

async function patchAiTask(taskId, patch) {
  try {
    await api(`/api/groups/${state.groupId}/ai-tasks/${taskId}`, {
      method: "PATCH",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ user: state.user, ...patch }),
    });
  } catch (error) {
    toast(error.message, true);
  }
}

function renderSources(sources) {
  $("ask-sources").innerHTML = sources.length
    ? `<div class="sources-title">引用来源（${sources.length}）</div>` + sources.map((s) => `
        <a href="#" class="source" data-file="${s.file_id}" data-chunk="${s.chunk_index}">
          <span class="source-score">${Number(s.score).toFixed(3)}</span>
          <span>[${s.index}]</span>
          <span class="source-name">${esc(s.file_name)} #${s.chunk_index}</span>
          <div class="source-snippet">${esc(s.snippet)}</div>
        </a>`).join("")
    : "";
}

async function ask(question) {
  if (state.streaming) return;
  state.streaming = true;
  const button = $("ask-btn");
  button.disabled = true;
  $("ask-sources").innerHTML = "";
  $("ask-answer").textContent = "";
  let answer = "";
  try {
    const res = await fetch(`/api/groups/${state.groupId}/ask`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ user: state.user, question }),
    });
    if (!res.ok) {
      let message = `HTTP ${res.status}`;
      try { message = (await res.json()).error || message; } catch { /* keep default */ }
      throw new Error(message);
    }
    const reader = res.body.getReader();
    const decoder = new TextDecoder();
    let buffer = "";
    let failure = null;
    while (!failure) {
      const { done, value } = await reader.read();
      if (done) break;
      buffer += decoder.decode(value, { stream: true });
      let boundary;
      while ((boundary = buffer.indexOf("\n\n")) >= 0) {
        const block = buffer.slice(0, boundary);
        buffer = buffer.slice(boundary + 2);
        let event = "message";
        let data = "";
        for (const line of block.split("\n")) {
          if (line.startsWith("event:")) event = line.slice(6).trim();
          else if (line.startsWith("data:")) data += line.slice(5);
        }
        if (!data) continue;
        let payload = null;
        try { payload = JSON.parse(data); } catch { continue; }
        if (event === "sources") renderSources(payload.sources || []);
        else if (event === "delta") {
          answer += payload.text || "";
          $("ask-answer").textContent = answer;
        } else if (event === "done") {
          if (payload.message) upsertMessage(payload.message);
        } else if (event === "error") {
          failure = new Error(payload.error || "问答失败");
        }
      }
    }
    if (failure) throw failure;
    if (!answer) $("ask-answer").textContent = "（没有返回内容）";
  } catch (error) {
    toast(error.message, true);
  } finally {
    state.streaming = false;
    button.disabled = false;
  }
}

function activateTab(name) {
  document.querySelectorAll(".tab").forEach((t) => t.classList.toggle("active", t.dataset.tab === name));
  document.querySelectorAll(".panel").forEach((p) => p.classList.toggle("active", p.id === `panel-${name}`));
}

function bindUI() {
  document.querySelectorAll(".tab").forEach((tab) => {
    tab.addEventListener("click", () => {
      activateTab(tab.dataset.tab);
      history.replaceState(null, "", `#${tab.dataset.tab}`);
      if (tab.dataset.tab === "usage") {
        loadUsage().catch((error) => toast(error.message, true));
      }
    });
  });

  $("group-select").addEventListener("change", (event) => selectGroup(event.target.value));

  $("user-input").addEventListener("change", (event) => {
    const value = event.target.value.trim();
    if (!value) {
      event.target.value = state.user;
      return;
    }
    state.user = value;
    localStorage.setItem("wp_user", value);
    connectWS();
  });

  $("chat-form").addEventListener("submit", (event) => {
    event.preventDefault();
    const input = $("chat-input");
    const content = input.value.trim();
    if (!content) return;
    if (!state.ws || state.ws.readyState !== WebSocket.OPEN) {
      toast("WebSocket 未连接，稍后再试", true);
      return;
    }
    state.ws.send(JSON.stringify({ type: "message", content }));
    input.value = "";
  });

  $("upload-btn").addEventListener("click", async () => {
    const input = $("file-input");
    if (!input.files || !input.files.length) {
      toast("请先选择文件", true);
      return;
    }
    const form = new FormData();
    form.append("file", input.files[0]);
    form.append("user", state.user);
    const button = $("upload-btn");
    button.disabled = true;
    try {
      const data = await api(`/api/groups/${state.groupId}/files`, { method: "POST", body: form });
      if (data && data.file) upsertFile(data.file);
      input.value = "";
      toast("上传成功，正在解析");
    } catch (error) {
      toast(error.message, true);
    } finally {
      button.disabled = false;
    }
  });

  $("files-body").addEventListener("click", async (event) => {
    const button = event.target.closest("button[data-action]");
    if (!button) return;
    const fileId = Number(button.dataset.id);
    const file = state.files.find((f) => f.id === fileId);
    const action = button.dataset.action;
    try {
      if (action === "content" || action === "chunks") {
        await openFileContent(fileId);
      } else if (action === "parse") {
        await api(`/api/groups/${state.groupId}/files/${fileId}/parse`, { method: "POST" });
        toast("已重新入队解析");
      } else if (action === "index") {
        await api(`/api/groups/${state.groupId}/files/${fileId}/index`, { method: "POST" });
        toast("已重新入队索引");
      } else if (action === "extract") {
        const data = await api(`/api/groups/${state.groupId}/tasks/extract`, {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ user: state.user, file_id: fileId }),
        });
        toast(data.count ? `新增 ${data.count} 条任务建议` : "该文件没有新的任务建议");
      } else if (action === "delete") {
        if (!confirm(`删除文件「${file ? file.file_name : fileId}」？解析内容与索引也会移除`)) return;
        await api(`/api/groups/${state.groupId}/files/${fileId}`, { method: "DELETE" });
        toast("文件已删除");
      }
    } catch (error) {
      toast(error.message, true);
    }
  });

  $("ask-form").addEventListener("submit", (event) => {
    event.preventDefault();
    const question = $("ask-input").value.trim();
    if (question) ask(question);
  });

  $("extract-btn").addEventListener("click", async () => {
    const button = $("extract-btn");
    button.disabled = true;
    button.textContent = "抽取中…";
    try {
      const data = await api(`/api/groups/${state.groupId}/tasks/extract`, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ user: state.user }),
      });
      (data.tasks || []).forEach(upsertTask);
      toast(data.count ? `新增 ${data.count} 条任务建议` : "没有新的任务建议");
    } catch (error) {
      toast(error.message, true);
    } finally {
      button.disabled = false;
      button.textContent = "从群内资料抽取任务建议";
    }
  });

  $("task-form").addEventListener("submit", async (event) => {
    event.preventDefault();
    const title = $("task-title").value.trim();
    if (!title) {
      toast("请填写任务标题", true);
      return;
    }
    try {
      await api(`/api/groups/${state.groupId}/tasks`, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          user: state.user,
          title,
          priority: $("task-priority").value,
          assignee: $("task-assignee").value.trim(),
        }),
      });
      $("task-title").value = "";
      $("task-assignee").value = "";
      toast("任务已创建");
    } catch (error) {
      toast(error.message, true);
    }
  });

  $("board").addEventListener("click", async (event) => {
    const button = event.target.closest("button[data-action]");
    if (!button) return;
    const taskId = Number(button.dataset.id);
    const task = state.tasks.find((t) => t.id === taskId);
    if (!task) return;
    const action = button.dataset.action;
    if (action === "assign") {
      const name = prompt("指派给谁？（留空表示取消指派）", task.assignee || "");
      if (name === null) return;
      patchTask(taskId, { assignee: name.trim() });
    } else if (action === "edit") {
      const title = prompt("修改任务标题", task.title);
      if (title === null || !title.trim()) return;
      patchTask(taskId, { title: title.trim() });
    } else if (action === "delete") {
      if (!confirm(`删除任务「${task.title}」？`)) return;
      try {
        await api(`/api/groups/${state.groupId}/tasks/${taskId}`, { method: "DELETE" });
      } catch (error) {
        toast(error.message, true);
      }
    } else {
      patchTask(taskId, { status: action });
    }
  });

  $("risk-extract-btn").addEventListener("click", async () => {
    const button = $("risk-extract-btn");
    button.disabled = true;
    button.textContent = "识别中…";
    try {
      const data = await api(`/api/groups/${state.groupId}/risks/extract`, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ user: state.user }),
      });
      (data.risks || []).forEach(upsertRisk);
      toast(data.count ? `新增 ${data.count} 条风险建议` : "没有新的风险建议");
    } catch (error) {
      toast(error.message, true);
    } finally {
      button.disabled = false;
      button.textContent = "从任务与资料识别风险";
    }
  });

  $("risk-form").addEventListener("submit", async (event) => {
    event.preventDefault();
    const title = $("risk-title").value.trim();
    if (!title) {
      toast("请填写风险摘要", true);
      return;
    }
    try {
      await api(`/api/groups/${state.groupId}/risks`, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          user: state.user,
          title,
          severity: $("risk-severity").value,
          owner: $("risk-owner").value.trim(),
        }),
      });
      $("risk-title").value = "";
      $("risk-owner").value = "";
      toast("风险已创建");
    } catch (error) {
      toast(error.message, true);
    }
  });

  $("risk-board").addEventListener("click", async (event) => {
    const button = event.target.closest("button[data-action]");
    if (!button) return;
    const riskId = Number(button.dataset.id);
    const risk = state.risks.find((r) => r.id === riskId);
    if (!risk) return;
    const action = button.dataset.action;
    if (action === "owner") {
      const name = prompt("跟进人是谁？（留空表示取消指派）", risk.owner || "");
      if (name === null) return;
      patchRisk(riskId, { owner: name.trim() });
    } else if (action === "edit") {
      const title = prompt("修改风险摘要", risk.title);
      if (title === null || !title.trim()) return;
      patchRisk(riskId, { title: title.trim() });
    } else if (action === "delete") {
      if (!confirm(`删除风险「${risk.title}」？`)) return;
      try {
        await api(`/api/groups/${state.groupId}/risks/${riskId}`, { method: "DELETE" });
      } catch (error) {
        toast(error.message, true);
      }
    } else {
      patchRisk(riskId, { status: action });
    }
  });

  $("ai-task-form").addEventListener("submit", async (event) => {
    event.preventDefault();
    const name = $("ai-task-name").value.trim();
    const schedule = $("ai-task-schedule").value.trim();
    if (!name || !schedule) {
      toast("请填写任务名称与 cron 计划", true);
      return;
    }
    try {
      await api(`/api/groups/${state.groupId}/ai-tasks`, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          user: state.user,
          name,
          schedule,
          prompt: $("ai-task-prompt").value.trim(),
          lookback_days: Number($("ai-task-lookback").value) || 7,
        }),
      });
      $("ai-task-name").value = "";
      toast("AI 任务已创建");
    } catch (error) {
      toast(error.message, true);
    }
  });

  $("weekly-template-btn").addEventListener("click", () => {
    $("ai-task-name").value = "每周进展周报";
    $("ai-task-schedule").value = "0 18 * * 5";
    $("ai-task-lookback").value = "7";
    if (!$("ai-task-prompt").value.trim()) {
      $("ai-task-prompt").value = "汇总本周期进展、任务变化与风险阻塞，并给出下周计划建议。";
    }
    toast("已填入周报模板（每周五 18:00）");
  });

  $("ai-task-list").addEventListener("click", async (event) => {
    const button = event.target.closest("button[data-action]");
    if (!button) return;
    const taskId = Number(button.dataset.id);
    const task = state.aiTasks.find((t) => t.id === taskId);
    if (!task) return;
    const action = button.dataset.action;
    try {
      if (action === "run") {
        button.disabled = true;
        button.textContent = "生成中…";
        await api(`/api/groups/${state.groupId}/ai-tasks/${taskId}/run`, {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ user: state.user }),
        });
        toast("已加入队列，生成完成后自动推送");
      } else if (action === "toggle") {
        await patchAiTask(taskId, { enabled: !task.enabled });
      } else if (action === "delete") {
        if (!confirm(`删除 AI 任务「${task.name}」？已生成的报告会保留`)) return;
        await api(`/api/groups/${state.groupId}/ai-tasks/${taskId}`, { method: "DELETE" });
        toast("AI 任务已删除");
      }
    } catch (error) {
      button.disabled = false;
      toast(error.message, true);
    }
  });

  $("report-list").addEventListener("click", async (event) => {
    const button = event.target.closest("button[data-action]");
    if (!button) return;
    const reportId = Number(button.dataset.id);
    const report = state.reports.find((r) => r.id === reportId);
    if (!report) return;
    if (button.dataset.action === "view") {
      openReport(report);
      return;
    }
    if (button.dataset.action === "delete") {
      if (!confirm(`删除报告「${report.title}」？`)) return;
      try {
        await api(`/api/groups/${state.groupId}/reports/${reportId}`, { method: "DELETE" });
        state.reports = state.reports.filter((r) => r.id !== reportId);
        renderReports();
        toast("报告已删除");
      } catch (error) {
        toast(error.message, true);
      }
    }
  });

  $("usage-refresh").addEventListener("click", async () => {
    const button = $("usage-refresh");
    button.disabled = true;
    try {
      await loadUsage();
    } catch (error) {
      toast(error.message, true);
    } finally {
      button.disabled = false;
    }
  });

  document.addEventListener("click", (event) => {
    const cite = event.target.closest("[data-file]");
    if (!cite) return;
    event.preventDefault();
    openFileContent(Number(cite.dataset.file), Number(cite.dataset.chunk));
  });

  $("modal-close").addEventListener("click", closeModal);
  $("modal").addEventListener("click", (event) => {
    if (event.target === $("modal")) closeModal();
  });
  document.addEventListener("keydown", (event) => {
    if (event.key === "Escape") closeModal();
  });
}

async function init() {
  bindUI();
  $("user-input").value = state.user;
  try {
    await loadGroups();
  } catch (error) {
    toast(error.message, true);
    return;
  }
  if (!state.groups.length) {
    toast("没有群组数据，请先执行迁移或准备演示数据", true);
    return;
  }
  const wanted = new URLSearchParams(location.search).get("group");
  const saved = localStorage.getItem("wp_group");
  const initial = state.groups.some((g) => g.id === wanted)
    ? wanted
    : state.groups.some((g) => g.id === saved)
      ? saved
      : state.groups[0].id;
  $("group-select").value = initial;
  const hashTab = location.hash.replace("#", "");
  if (["chat", "files", "ask", "board", "risks", "reports", "usage"].includes(hashTab)) activateTab(hashTab);
  selectGroup(initial);
}

init();
