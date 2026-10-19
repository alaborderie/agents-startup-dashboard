(function () {
  "use strict";

  var body = document.getElementById("quota-body");
  var meta = document.getElementById("quota-meta");
  if (!body || !meta) { return; }

  var active = false;
  var generatedAt = "";
  // The last rendered view, redrawn when one of its readings expires between polls.
  var shown = null;
  var nextExpiry = Infinity;

  function element(tag, className, text) {
    var node = document.createElement(tag);
    if (className) { node.className = className; }
    if (text !== undefined) { node.textContent = text; }
    return node;
  }

  function relative(timestamp, prefix) {
    var time = Date.parse(timestamp);
    if (!Number.isFinite(time)) { return ""; }
    var minutes = Math.max(0, Math.ceil((time - Date.now()) / 60000));
    if (prefix === "updated") { minutes = Math.max(0, Math.floor((Date.now() - time) / 60000)); }
    if (prefix === "resets" && minutes === 0) { return "reset due"; }
    var days = Math.floor(minutes / 1440);
    var hours = Math.floor((minutes % 1440) / 60);
    var rest = minutes % 60;
    var parts = [];
    if (days) { parts.push(days + " d"); }
    if (hours) { parts.push(hours + " h"); }
    if (rest || !parts.length) { parts.push(rest + " m"); }
    return prefix + (prefix === "updated" ? " " : " in ") + parts.join(" ") + (prefix === "updated" ? " ago" : "");
  }

  // Observation readings carry a deadline; legacy windows never expire here.
  function current(item) {
    var expiry = Date.parse(item.expires_at);
    if (!item.expires_at) { return true; }
    if (!Number.isFinite(expiry) || expiry <= Date.now()) { return false; }
    nextExpiry = Math.min(nextExpiry, expiry);
    return true;
  }

  function updateTimes() {
    if (shown && Date.now() >= nextExpiry) { render(shown); return; }
    if (generatedAt) {
      meta.textContent = relative(generatedAt, "updated");
      meta.title = new Date(generatedAt).toLocaleString();
    }
    body.querySelectorAll("[data-resets-at]").forEach(function (node) {
      node.querySelector(".q-time").textContent = relative(node.dataset.resetsAt, "resets");
      node.title = new Date(node.dataset.resetsAt).toLocaleString();
    });
  }

  function replace(node) {
    body.replaceChildren(node);
    body.setAttribute("aria-busy", "false");
    updateTimes();
  }

  function prompt(rejected) {
    generatedAt = "";
    shown = null;
    meta.textContent = "";
    var form = element("form", "q-form");
    var label = element("label", "", "ai-quota READ token");
    label.htmlFor = "quota-token";
    var field = element("div", "q-field");
    var input = element("input");
    input.id = "quota-token";
    input.type = "password";
    input.required = true;
    input.maxLength = 512;
    input.autocomplete = "off";
    input.spellcheck = false;
    if (rejected) { input.setAttribute("aria-invalid", "true"); }
    var button = element("button", "btn btn-primary", "Show quota");
    button.type = "submit";
    field.append(input, button);
    form.append(label, field, element("p", "q-help", "HQ works without it."));
    if (rejected) {
      var error = element("p", "q-err", "That token was not accepted. Nothing was saved.");
      error.setAttribute("role", "alert");
      form.append(error);
    }
    form.addEventListener("submit", async function (event) {
      event.preventDefault();
      var token = input.value;
      if (!token || active) { return; }
      active = true;
      button.disabled = true;
      button.textContent = "Checking…";
      try {
        var response = await fetch("/hq/api/quota/token", {
          method: "PUT", credentials: "same-origin",
          headers: { "Content-Type": "application/json" }, body: JSON.stringify({ token: token })
        });
        input.value = "";
        if (response.status === 401) { prompt(true); return; }
        if (!response.ok) { render({ state: "unavailable" }); return; }
        render(await response.json());
      } catch (error) {
        input.value = "";
        render({ state: "unavailable" });
      } finally {
        active = false;
      }
    });
    replace(form);
  }

  function unavailable() {
    generatedAt = "";
    shown = null;
    meta.textContent = "";
    var panel = element("div");
    var note = element("p", "q-note err", "Quota unavailable — HQ unaffected.");
    note.setAttribute("role", "status");
    var retry = element("button", "btn btn-quiet", "Retry now");
    retry.type = "button";
    retry.addEventListener("click", poll);
    panel.append(note, element("p", "q-help", "Retrying in 60 s."), retry);
    replace(panel);
  }

  function windowRow(provider, windowData) {
    var row = element("div", "q-win");
    row.append(element("span", "lbl", windowData.label || windowData.id));
    var severity = windowData.severity;
    var tone = severity === "ok" ? "ok" : severity === "warn" ? "warn" : "bad";
    var value = Math.max(0, Math.min(100, Number(windowData.used_percent) || 0));
    var meter = element("div", "q-bar " + tone);
    meter.setAttribute("role", "meter");
    meter.setAttribute("aria-label", provider.display_name + " " + (windowData.label || windowData.id) + " used");
    meter.setAttribute("aria-valuemin", "0");
    meter.setAttribute("aria-valuemax", "100");
    meter.setAttribute("aria-valuenow", String(value));
    var fill = element("i");
    fill.style.setProperty("--v", value + "%");
    meter.append(fill);
    row.append(meter, element("span", "pct", Math.round(value) + "%"));
    var reset = element("span", "q-reset");
    if (windowData.resets_at) {
      reset.dataset.resetsAt = windowData.resets_at;
      reset.append(element("span", "q-time"));
    }
    if (severity !== "ok") {
      reset.append(element("span", "q-sev " + tone, windowData.limited || severity === "limited" ? "LIMITED" : severity === "critical" ? "CRITICAL" : "WARN"));
    }
    row.append(reset);
    return row;
  }

  function providerRow(provider) {
    var section = element("section", "q-prov");
    var heading = element("div", "q-prov-head");
    heading.append(element("h3", "", provider.display_name));
    if (provider.plan) { heading.append(element("span", "q-plan", provider.plan)); }
    var windows = (provider.windows || []).filter(current);
    var readings = (provider.readings || []).filter(current);
    var lapsed = !windows.length && !readings.length &&
      ((provider.windows || []).length > 0 || (provider.readings || []).length > 0);
    var stale = lapsed || provider.freshness !== "fresh" || provider.status_state !== "ok";
    heading.append(element("span", "q-fresh" + (stale ? " stale" : ""), lapsed ? "expired" : provider.status_state !== "ok" ? provider.status_state || "unavailable" : provider.freshness || "missing"));
    section.append(heading);
    if (stale) {
      section.append(element("p", "q-note", lapsed ? provider.expired_message || "Last reading expired." : provider.status_message || "Quota " + (provider.freshness || "unavailable") + "."));
    } else if (windows.length || readings.length) {
      windows.forEach(function (windowData) { section.append(windowRow(provider, windowData)); });
      readings.forEach(function (reading) {
        var row = element("div", "q-balance");
        row.append(element("span", "", reading.label || reading.id), element("b", "", reading.value));
        section.append(row);
      });
      if (provider.status_message) { section.append(element("p", "q-help", provider.status_message)); }
    } else if (provider.balance) {
      var balance = element("div", "q-balance");
      balance.append(element("span", "", provider.balance.label || "Balance left"));
      balance.append(element("b", "", "$" + Number(provider.balance.amount_usd).toFixed(2) + (provider.balance.estimated ? " (est.)" : "")));
      section.append(balance);
    } else {
      section.append(element("p", "q-help", "No quota windows reported."));
    }
    return section;
  }

  function render(data) {
    if (!data || data.state === "unavailable") { unavailable(); return; }
    if (data.state === "needs_token") {
      // A timer or tab-return poll must not erase a draft, focus, or rejection
      // feedback when the server still needs the same token submission.
      if (!body.querySelector(".q-form") || data.reason === "rejected") {
        prompt(data.reason === "rejected");
      }
      return;
    }
    if (data.state !== "ok" || !Array.isArray(data.providers)) { unavailable(); return; }
    generatedAt = data.generated_at || "";
    shown = data;
    nextExpiry = Infinity;
    var panel = element("div");
    if (!data.providers.length) { panel.append(element("p", "q-note", "No provider has reported yet.")); }
    data.providers.forEach(function (provider) { panel.append(providerRow(provider)); });
    var footer = element("div", "q-foot");
    var forget = element("button", "btn btn-ghost", "Forget token");
    forget.type = "button";
    forget.addEventListener("click", async function () {
      if (active) { return; }
      active = true;
      forget.disabled = true;
      try {
        var response = await fetch("/hq/api/quota/token", { method: "DELETE", credentials: "same-origin" });
        if (!response.ok) { throw new Error("delete failed"); }
        prompt(false);
      } catch (error) { unavailable(); } finally { active = false; }
    });
    footer.append(forget);
    panel.append(footer);
    replace(panel);
  }

  async function poll() {
    if (document.visibilityState !== "visible" || active) { return; }
    active = true;
    try {
      var response = await fetch("/hq/api/quota", { credentials: "same-origin" });
      if (!response.ok) { throw new Error("quota unavailable"); }
      render(await response.json());
    } catch (error) {
      unavailable();
    } finally {
      active = false;
    }
  }

  document.addEventListener("visibilitychange", function () {
    if (document.visibilityState === "visible") { poll(); }
  });
  window.setInterval(poll, 60000);
  window.setInterval(updateTimes, 30000);
  poll();
}());
