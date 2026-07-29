// loop-eng web dashboard — pure vanilla JS, no dependencies.
// Fetches the read API (/api/overview, /api/tasks/{id}, /api/tasks/{id}/trace)
// and POSTs commands (/api/tasks/{id}/command) for resume/cancel.
(function () {
  "use strict";

  var REFRESH_MS = 3000; // ~3s auto-refresh of the overview
  var STATUS_KEYS = [
    "running", "new", "needs-review", "needs-info",
    "needs-human-decision", "blocked", "done", "cancelled",
  ];

  var refreshTimer = null;

  function $(sel) { return document.querySelector(sel); }

  // —— render helpers ——

  function badge(status) {
    return '<span class="badge status-' + status + '">' + esc(status) + "</span>";
  }

  function renderCounts(counts) {
    var el = $("#counts");
    el.innerHTML = STATUS_KEYS
      .filter(function (k) { return counts[k]; })
      .map(function (k) {
        return '<span class="count-pill status-' + k + '">' +
          esc(k) + " <b>" + counts[k] + "</b></span>";
      })
      .join("");
  }

  function renderRunning(running) {
    var el = $("#running-banner");
    if (!running) { el.hidden = true; el.textContent = ""; return; }
    el.hidden = false;
    var retry = running.retry > 1 ? " · retry " + running.retry : "";
    el.innerHTML =
      '<span class="dot status-running">●</span> 进行中：' +
      esc(running.phase || "?") + retry +
      (running.started_at ? " · " + esc(shortTime(running.started_at)) : "");
  }

  function renderOverview(ov) {
    renderCounts(ov.counts || {});
    renderRunning(ov.running || null);
    var tasks = ov.tasks || [];
    $("#empty-hint").hidden = tasks.length > 0;
    $("#task-list").innerHTML = tasks.map(function (t) {
      return '<li class="task ' + t.status + '" data-id="' + esc(t.id) + '">' +
        '<div class="task-main">' +
          badge(t.status) +
          '<span class="task-ref">' + esc(t.issue_ref || t.id) + "</span>" +
          '<span class="task-desc">' + esc(t.description || "(无描述)") + "</span>" +
        "</div>" +
        '<div class="task-meta">' +
          (t.last_run_at ? "<span>last " + esc(shortTime(t.last_run_at)) + "</span>" : "") +
          "<span>" + esc(t.task_type || "task") + "</span>" +
        "</div>" +
      "</li>";
    }).join("");
    $("#last-updated").textContent = "更新于 " + clockNow();
  }

  // —— data fetch ——

  function fetchJSON(url) {
    return fetch(url).then(function (res) {
      if (!res.ok) throw new Error(url + " -> " + res.status);
      return res.json();
    });
  }

  function refreshOverview() {
    fetchJSON("/api/overview")
      .then(renderOverview)
      .catch(function (e) { console.error("overview refresh failed", e); });
  }

  function startAutoRefresh() {
    stopAutoRefresh();
    refreshOverview();
    refreshTimer = setInterval(refreshOverview, REFRESH_MS);
  }
  function stopAutoRefresh() {
    if (refreshTimer) { clearInterval(refreshTimer); refreshTimer = null; }
  }

  // —— detail view ——

  function showOverview() {
    stopAutoRefresh();
    $("#detail").hidden = true;
    $("#overview").hidden = false;
    startAutoRefresh();
  }

  function showDetail(id) {
    stopAutoRefresh();
    $("#overview").hidden = true;
    $("#detail").hidden = false;
    var body = $("#detail-body");
    body.innerHTML = '<p class="hint">加载中…</p>';

    var detailP = fetchJSON("/api/tasks/" + encodeURIComponent(id));
    var traceP = fetchJSON("/api/tasks/" + encodeURIComponent(id) + "/trace")
      .catch(function () { return null; });

    Promise.all([detailP, traceP])
      .then(function (res) {
        body.innerHTML = renderDetail(res[0]) + renderTrace(res[1]);
        bindDetailActions(id);
      })
      .catch(function (e) {
        body.innerHTML = '<p class="hint error">加载失败：' + esc(e.message) + "</p>";
      });
  }

  function renderDetail(d) {
    var tiers = (d.tiers || []).map(function (t) {
      return '<li class="tier tier-' + tierStatusClass(t) + '">' +
        '<span class="tier-no">tier-' + t.tier + "</span>" +
        '<span class="tier-label">' + esc(t.label) + "</span>" +
        '<span class="tier-status">' + esc(t.status) + "</span>" +
      "</li>";
    }).join("");

    var criteria = (d.criteria || []).map(function (c) {
      return "<li>" + esc(c) + "</li>";
    }).join("");
    if (!criteria) { criteria = '<li class="hint">（无）</li>'; }

    var budget = d.budget
      ? d.budget.used + " / " + d.budget.limit + " tokens"
      : "—";

    return (
      '<h2>' + badge(d.status) +
        '<span class="task-ref">' + esc(d.issue_ref || d.id) + "</span></h2>" +
      '<p class="desc">' + esc(d.description || "") + "</p>" +
      section("验收标准", '<ul class="criteria">' + criteria + "</ul>") +
      section("验收方式", '<ul class="tiers">' + (tiers || '<li class="hint">（无）</li>') + "</ul>") +
      section("预算", '<p class="budget">' + budget + "</p>") +
      '<section class="actions">' +
        '<button data-verb="resume" class="btn btn-resume">resume</button>' +
        '<button data-verb="cancel" class="btn btn-cancel">cancel</button>' +
      "</section>"
    );
  }

  function renderTrace(tr) {
    if (!tr || !tr.runs || tr.runs.length === 0) {
      return section("时间线", '<p class="hint">暂无 run 记录。</p>');
    }
    var runs = tr.runs.map(function (run) {
      var steps = (run.steps || []).map(function (st) {
        return '<li class="step">' +
          '<span class="step-role">' + esc(st.role) + "</span>" +
          '<span class="step-status status-' + stepStatusClass(st) + '">' + esc(st.status) + "</span>" +
          (st.skill ? '<span class="step-skill">' + esc(st.skill) + "</span>" : "") +
          '<span class="step-tokens">' + st.tokens_in + "↓ " + st.tokens_out + "↑</span>" +
          (st.error ? '<span class="step-error">' + esc(st.error) + "</span>" : "") +
        "</li>";
      }).join("");

      var vers = (run.verifications || []).map(function (v) {
        return '<li class="ver ' + (v.passed ? "pass" : "fail") + '">tier-' +
          v.tier + " " + (v.passed ? "✓" : "✗") + " " + esc(v.detail || "") + "</li>";
      }).join("");

      var budget = (run.budget || []).map(function (b) {
        return "<li>" + esc(b.scope) + "/" + esc(b.kind) + ": " + b.amount +
          (b.limit ? " / " + b.limit : "") + "</li>";
      }).join("");

      var open = run.outcome ? "" : " open"; // still-running run expanded
      var summary = "run " + String(run.run_id).slice(-6) + " · " +
        esc(run.outcome || "running") + " · " + esc(shortTime(run.started_at));
      return '<details class="run"' + open + '><summary>' + summary + "</summary>" +
        '<ul class="steps">' + (steps || '<li class="hint">（无 step）</li>') + "</ul>" +
        (vers ? '<ul class="vers">' + vers + "</ul>" : "") +
        (budget ? '<ul class="budget-ledger">' + budget + "</ul>" : "") +
      "</details>";
    }).join("");

    return section("时间线", runs);
  }

  function bindDetailActions(id) {
    var btns = document.querySelectorAll("#detail .btn[data-verb]");
    Array.prototype.forEach.call(btns, function (btn) {
      btn.addEventListener("click", function () {
        var verb = btn.getAttribute("data-verb");
        btn.disabled = true;
        fetch("/api/tasks/" + encodeURIComponent(id) + "/command", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ verb: verb, payload: "" }),
        }).then(function (res) {
          if (!res.ok) throw new Error("status " + res.status);
          flash(btn, "已下发 ✓", false);
        }).catch(function (e) {
          flash(btn, "失败：" + e.message, true);
        }).finally(function () {
          btn.disabled = false;
        });
      });
    });
  }

  // —— small utils ——

  function section(title, inner) {
    return "<section><h3>" + esc(title) + "</h3>" + inner + "</section>";
  }
  function flash(btn, msg, err) {
    var orig = btn.textContent;
    btn.textContent = msg;
    if (err) { btn.classList.add("error"); }
    setTimeout(function () { btn.textContent = orig; btn.classList.remove("error"); }, 1200);
  }
  function esc(s) {
    return String(s == null ? "" : s).replace(/[&<>"']/g, function (c) {
      return { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c];
    });
  }
  function shortTime(iso) {
    if (!iso) return "";
    return String(iso).replace("T", " ").replace(/\.\d+Z?$/, "").replace("Z", "");
  }
  function tierStatusClass(t) {
    if (t.passed === true) return "pass";
    if (t.passed === false) return "fail";
    return "idle";
  }
  function stepStatusClass(st) {
    if (st.status === "ok") return "done";
    if (st.status === "error" || st.status === "fail") return "blocked";
    return "new";
  }
  // stable HH:MM:SS for the "last updated" stamp (display only)
  function clockNow() {
    var d = new Date();
    function p(n) { return (n < 10 ? "0" : "") + n; }
    return p(d.getHours()) + ":" + p(d.getMinutes()) + ":" + p(d.getSeconds());
  }

  // —— wire up ——

  document.addEventListener("DOMContentLoaded", function () {
    $("#back-btn").addEventListener("click", showOverview);
    $("#task-list").addEventListener("click", function (e) {
      var li = e.target.closest(".task");
      if (li) { showDetail(li.getAttribute("data-id")); }
    });
    showOverview();
  });
})();
