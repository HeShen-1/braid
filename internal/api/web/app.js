"use strict";
// braid 审批台(T4.4):登录 → 待办列表 → 详情 → 批准/驳回。
// 页面动作全部经既有 API 面:POST /api/v1/auth/login(T4.4)、
// GET /api/v1/approvals/pending(T4.4)、
// GET+POST /api/v1/executions/:id/approvals/:node_id(/decision)
// ——POST 决定即真实驱动流程续跑(同事务落 decision + 审计 +
// execution.resume outbox,worker 侧续跑)。
//
// 安全纪律(与 web.go CSP 同套):
//   - 服务端数据一律 textContent 渲染,innerHTML/outerHTML 禁用
//     (instructions/comment 是他人自由文本 = XSS 向量);
//   - token 存 sessionStorage(D-t44:Bearer 载体,per-tab 生命周期,
//     非 cookie 无 CSRF 自动携带面);
//   - 零第三方脚本、零内联脚本(CSP default-src 'none')。

const TOKEN_KEY = "braid_token";
const PAGE_LIMIT = 20;

const ERR_TEXT = {
  invalid_credentials: "用户名或密码错误。",
  user_disabled: "账号已停用。",
  not_approver: "你不是本节点的审批人。",
  role_denied: "缺少审批角色(approver/admin)。",
  not_waiting: "该审批已收口,无需再操作。",
  not_approval: "目标节点不是审批节点。",
  already_decided: "已有审批人投出决定(或签首票定局)。",
  decision_conflict: "你已投过不同的决定,不可改判。",
  execution_terminal: "执行已终结,该审批不再生效。",
  bad_comment: "备注超过 2000 字上限。",
};

function $(id) { return document.getElementById(id); }

function el(tag, cls, text) {
  const n = document.createElement(tag);
  if (cls) n.className = cls;
  if (text !== undefined && text !== null) n.textContent = String(text);
  return n;
}

function errText(e) { return ERR_TEXT[e.code] || ("请求失败(" + e.code + ")。"); }

function ApiError(status, code) { this.status = status; this.code = code; }
ApiError.prototype = Object.create(Error.prototype);
ApiError.prototype.name = "ApiError";

function token() { return sessionStorage.getItem(TOKEN_KEY) || ""; }
function setToken(t) { sessionStorage.setItem(TOKEN_KEY, t); }
function clearToken() { sessionStorage.removeItem(TOKEN_KEY); }

async function api(path, opts) {
  opts = opts || {};
  const headers = Object.assign({}, opts.headers);
  const t = token();
  if (t) headers["Authorization"] = "Bearer " + t;
  if (opts.body !== undefined) headers["Content-Type"] = "application/json";
  const res = await fetch(path, { method: opts.method || "GET", headers: headers, body: opts.body });
  if (res.status === 401 && t) {
    // 会话过期只对已认证调用生效;登录面自身的 401(invalid_credentials)
    // 必须落正常错误提取,否则错误文案被「登录已过期」覆盖。
    enterLoggedOut("登录已过期,请重新登录。");
    throw new ApiError(401, "unauthorized");
  }
  let data = null;
  try { data = await res.json(); } catch (_) { /* 空 body */ }
  if (!res.ok) throw new ApiError(res.status, (data && data.error) || ("http_" + res.status));
  return data;
}

// —— 视图切换与登入态 ——
function showView(name) {
  for (const v of ["login-view", "list-view", "detail-view"]) $(v).hidden = v !== name;
}

function enterLoggedOut(message) {
  clearToken();
  // 共享终端防线(对抗审计):换人/登出即终结全部痕迹——表单凭证、
  // 分页偏移(陈旧 offset 会让下一用户首屏落空页)一并清零。
  $("username").value = "";
  $("password").value = "";
  $("comment").value = "";
  $("who").textContent = "";
  $("logout").hidden = true;
  page.offset = 0;
  page.total = 0;
  showView("login-view");
  $("login-error").textContent = message || "";
}

function enterApp() {
  $("logout").hidden = false;
  showView("list-view");
  loadPending();
}

// —— 登录 ——
function wireLogin() {
  $("login-form").addEventListener("submit", async (ev) => {
    ev.preventDefault();
    $("login-error").textContent = "";
    try {
      const out = await api("/api/v1/auth/login", {
        method: "POST",
        body: JSON.stringify({ username: $("username").value, password: $("password").value }),
      });
      setToken(out.token);
      $("password").value = ""; // 凭证不留在 DOM(隐藏视图同样算残留面)
      $("who").textContent = out.username;
      enterApp();
    } catch (e) {
      $("login-error").textContent = errText(e);
    }
  });
}

// —— 待办列表 ——
const page = { offset: 0, total: 0 };
// listSeq 是列表请求序号(对抗审计:快速连点时后发先至的陈旧响应会
// 覆盖新页渲染——序号不等即丢弃)。
let listSeq = 0;

function deadlineCell(iso) {
  const td = el("td", null, iso ? new Date(iso).toLocaleString() : "—");
  if (iso && new Date(iso).getTime() < Date.now()) td.classList.add("overdue");
  return td;
}

function renderPending(data) {
  page.total = (data.page && data.page.total) || 0;
  const tbody = $("todo-body");
  tbody.replaceChildren();
  const items = data.items || [];
  for (const it of items) {
    const tr = el("tr");
    const open = el("button", null, "处理");
    open.type = "button";
    open.addEventListener("click", () => openDetail(it));
    tr.append(
      el("td", null, it.definition_name + " v" + it.definition_version),
      el("td", "mono", it.execution_id),
      el("td", null, it.node_id),
      deadlineCell(it.deadline_at),
      el("td", null, "")
    );
    tr.lastChild.append(open);
    tbody.append(tr);
  }
  $("todo-empty").hidden = items.length !== 0;
  $("prev-page").disabled = page.offset <= 0;
  $("next-page").disabled = page.offset + PAGE_LIMIT >= page.total;
}

async function loadPending() {
  $("list-error").textContent = "";
  const seq = ++listSeq;
  try {
    const data = await api("/api/v1/approvals/pending?limit=" + PAGE_LIMIT + "&offset=" + page.offset);
    if (seq !== listSeq) return; // 陈旧响应:已被更新的请求取代,丢弃
    renderPending(data);
  } catch (e) {
    if (seq !== listSeq) return;
    if (e.status !== 401) $("list-error").textContent = errText(e);
  }
}

function wireList() {
  $("refresh").addEventListener("click", () => { page.offset = 0; loadPending(); });
  $("prev-page").addEventListener("click", () => {
    page.offset = Math.max(0, page.offset - PAGE_LIMIT); loadPending();
  });
  $("next-page").addEventListener("click", () => {
    if (page.offset + PAGE_LIMIT < page.total) { page.offset += PAGE_LIMIT; loadPending(); }
  });
  $("logout").addEventListener("click", () => enterLoggedOut(""));
  $("back-to-list").addEventListener("click", () => { showView("list-view"); loadPending(); });
}

// —— 详情 + 决定 ——
function metaRow(term, value) {
  const dt = el("dt", null, term);
  const dd = el("dd", "mono", value);
  const div = el("div");
  div.append(dt, dd);
  return div;
}

function decisionFace(v) { return v === "approve" ? "批准" : v === "reject" ? "驳回" : v; }

// 会签中间态(T14.2):mode 徽标词面 + 待投名单(仅 WAITING 时非空;
// 部分批准的可见进度 = 名单 − 本轮已投)。
function modeFace(m) {
  if (m === "all") return "会签(全部批准才过,任一驳回即败)";
  if (m === "any") return "或签(任一票定局)";
  return m || "或签(任一票定局)";
}

function renderDetail(d) {
  $("detail-title").textContent = d.definition_name
    ? (d.definition_name + " · " + d.node_id)
    : ("执行 " + d.execution_id + " · " + d.node_id);
  const pending = d.pending_approvers || [];
  const meta = $("detail-meta");
  meta.replaceChildren(
    metaRow("执行 ID", d.execution_id),
    metaRow("节点状态", d.node_status),
    metaRow("审批模式", modeFace(d.mode)),
    metaRow("审批人", (d.approvers || []).join(", ")),
    metaRow("待投名单", pending.length ? pending.join(", ") : "—"),
    metaRow("截止时间", d.deadline_at ? new Date(d.deadline_at).toLocaleString() : "—")
  );
  $("detail-instructions").textContent = d.instructions || "(工作流未提供审批说明)";
  const ul = $("detail-decisions");
  ul.replaceChildren();
  for (const dec of d.decisions || []) {
    const li = el("li", null,
      new Date(dec.decided_at).toLocaleString() + " · " + dec.decided_by + " · " + decisionFace(dec.decision)
      + (dec.comment ? (" · " + dec.comment) : ""));
    ul.append(li);
  }
  // 已收口节点只读:决定表单隐藏(POST 也会 409,这里前置免无效提交)。
  const actionable = d.node_status === "WAITING_APPROVAL";
  $("decision-form").hidden = !actionable;
}

async function openDetail(item) {
  $("detail-error").textContent = "";
  // 备注与节点一一对应(对抗审计):进入新详情即清空上一节点未提交的
  // 备注,防其作为下一节点的决定 comment 持久化进 decision 行与审计。
  $("comment").value = "";
  try {
    const d = await api("/api/v1/executions/" + item.execution_id + "/approvals/" + item.node_id);
    // 列表行补充详情响应没有的字段(definition_name;execution_id 双方
    // 同为字符串形态,覆写幂等)。
    d.execution_id = item.execution_id;
    d.definition_name = item.definition_name;
    renderDetail(d);
    showView("detail-view");
    wireDecision(item.execution_id, item.node_id);
  } catch (e) {
    if (e.status !== 401) {
      $("list-error").textContent = errText(e);
    }
  }
}

function wireDecision(executionID, nodeID) {
  const path = "/api/v1/executions/" + executionID + "/approvals/" + nodeID + "/decision";
  const submit = async (decision) => {
    // in-flight 防抖(对抗审计):双击并发两笔 POST,数据面有服务端
    // 幂等守卫,但第二响应的错误文案会写进已隐藏视图/重复弹窗。
    if ($("approve").disabled) return;
    $("approve").disabled = true;
    $("reject").disabled = true;
    $("detail-error").textContent = "";
    try {
      const out = await api(path, {
        method: "POST",
        body: JSON.stringify({ decision: decision, comment: $("comment").value }),
      });
      $("comment").value = "";
      window.alert("已记录:" + decisionFace(out.decision) + (out.comment ? " · " + out.comment : ""));
      showView("list-view");
      page.offset = 0;
      loadPending();
    } catch (e) {
      if (e.status !== 401) $("detail-error").textContent = errText(e);
    } finally {
      $("approve").disabled = false;
      $("reject").disabled = false;
    }
  };
  $("approve").onclick = () => { submit("approve"); };
  $("reject").onclick = () => { submit("reject"); };
}

// —— 启动 ——
document.addEventListener("DOMContentLoaded", () => {
  wireLogin();
  wireList();
  if (token()) {
    // who 显示需要一次读面;省一跳:token 有效期内直接进列表,失败面
    // 由 api() 的 401 分支统一收口回登录。
    enterApp();
  } else {
    enterLoggedOut("");
  }
});
