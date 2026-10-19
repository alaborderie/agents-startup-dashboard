// Run with: node --test internal/web/quota_ui_test.cjs
const assert = require("node:assert/strict");
const { readFileSync } = require("node:fs");
const { test } = require("node:test");
const vm = require("node:vm");

// Minimal DOM boundary for running the actual card script without a browser.
class Element {
  constructor(tag) {
    this.tag = tag;
    this.children = [];
    this.events = {};
    this.attributes = {};
    this.value = "";
    this.textContent = "";
    this.dataset = {};
    this.style = { setProperty() {} };
  }
  append(...nodes) { this.children.push(...nodes); }
  replaceChildren(...nodes) { this.children = nodes; }
  setAttribute(name, value) { this.attributes[name] = value; }
  addEventListener(name, callback) { this.events[name] = callback; }
  querySelectorAll(selector) {
    return this.children.flatMap(node => {
      const matches = selector.startsWith(".")
        ? (node.className || "").split(" ").includes(selector.slice(1))
        : node.tag === selector;
      return [...(matches ? [node] : []), ...node.querySelectorAll(selector)];
    });
  }
  querySelector(selector) { return this.querySelectorAll(selector)[0] || null; }
}

const settle = () => new Promise(resolve => setImmediate(resolve));

async function card() {
  const body = new Element("div");
  const meta = new Element("span");
  const events = {};
  const intervals = new Map();
  const requests = [];
  let reply = { state: "needs_token" };
  let status = 200;
  let release;
  const clock = { now: Date.parse("2026-10-19T12:00:00Z") };
  class FakeDate extends Date { static now() { return clock.now; } }
  const document = {
    visibilityState: "visible",
    getElementById: id => id === "quota-body" ? body : meta,
    createElement: tag => new Element(tag),
    addEventListener: (name, callback) => { events[name] = callback; }
  };
  vm.runInNewContext(readFileSync(`${__dirname}/static/quota.js`, "utf8"), {
    document,
    Date: FakeDate,
    window: { setInterval: (callback, ms) => intervals.set(ms, callback) },
    fetch: async (url, options) => {
      requests.push({ url, ...options });
      if (release) { await new Promise(resolve => { release = resolve; }); }
      return { ok: status < 400, status, json: async () => reply };
    }
  });
  await settle();
  return {
    body, document, requests,
    clock,
    poll: () => intervals.get(60000)(),
    tick: () => intervals.get(30000)(),
    visible: () => events.visibilitychange(),
    reply: (data, code = 200) => { reply = data; status = code; },
    hold: () => { release = true; },
    release: () => { const done = release; release = null; done(); }
  };
}

test("periodic polling preserves an unsubmitted token and its form", async () => {
  const ui = await card();
  const form = ui.body.querySelector("form");
  const input = ui.body.querySelector("input");
  input.value = "synthetic-unsubmitted-token";
  await ui.poll();
  assert.equal(ui.body.querySelector("input").value, "synthetic-unsubmitted-token");
  assert.equal(ui.body.querySelector("form"), form, "poll must preserve input focus/selection");
});

test("return-to-tab response cannot erase a token pasted while GET is in flight", async () => {
  const ui = await card();
  ui.hold();
  ui.visible();
  ui.body.querySelector("input").value = "synthetic-pasted-token";
  ui.release();
  await settle();
  assert.equal(ui.body.querySelector("input").value, "synthetic-pasted-token");
});

test("rejection remains visible across polls but a new submission clears the input", async () => {
  const ui = await card();
  ui.reply({ error: "token not accepted" }, 401);
  ui.body.querySelector("input").value = "synthetic-rejected-token";
  await ui.body.querySelector("form").events.submit({ preventDefault() {} });
  assert.ok(ui.body.querySelector(".q-err"));
  assert.equal(ui.body.querySelector("input").value, "");
  ui.reply({ state: "needs_token" });
  await ui.poll();
  assert.ok(ui.body.querySelector(".q-err"), "poll must not silently clear rejection feedback");
  ui.body.querySelector("input").value = "synthetic-replacement-token";
  ui.reply({ state: "ok", providers: [] });
  const input = ui.body.querySelector("input");
  await ui.body.querySelector("form").events.submit({ preventDefault() {} });
  assert.equal(input.value, "");
  assert.equal(ui.body.querySelector("form"), null);
  assert.equal(ui.requests.filter(request => request.method === "PUT").length, 2);
});

test("a token saved in another tab still replaces the prompt on the next poll", async () => {
  const ui = await card();
  ui.reply({ state: "ok", providers: [] });
  await ui.poll();
  assert.equal(ui.body.querySelector("form"), null);
  assert.ok(ui.body.querySelector(".q-foot"));
});

const texts = node => [node.textContent, ...node.children.flatMap(texts)].filter(Boolean);

test("observation readings show native amounts and a partial-failure note", async () => {
  const ui = await card();
  ui.reply({ state: "ok", generated_at: "2026-10-19T12:00:00Z", providers: [
    { provider: "cursor", display_name: "Cursor", freshness: "fresh", status_state: "ok", status_message: "Timed out", readings: [],
      windows: [{ id: "weekly", label: "Week", used_percent: 72.5, severity: "warn", resets_at: "2026-10-22T00:00:00Z", limited: false }] },
    { provider: "xai_api", display_name: "xAI API", freshness: "fresh", status_state: "ok", windows: [],
      readings: [{ id: "prepaid-ledger-total", label: "Balance", value: "−$10.00" }] },
    { provider: "github_copilot", display_name: "GitHub Copilot", freshness: "missing", status_state: "error",
      status_message: "Authentication failed", windows: [], readings: [] }
  ] });
  await ui.poll();
  const [cursor, xai, copilot] = ui.body.querySelectorAll(".q-prov");
  assert.ok(cursor.querySelector(".q-bar"));
  assert.deepEqual(texts(cursor.querySelector(".q-help")), ["Timed out"]);
  assert.deepEqual(texts(xai.querySelector(".q-balance")), ["Balance", "−$10.00"]);
  assert.equal(xai.querySelector(".q-help"), null);
  assert.equal(copilot.querySelector(".q-balance"), null);
  assert.deepEqual(texts(copilot.querySelector(".q-note")), ["Authentication failed"]);
});

test("observation readings disappear at their deadline even without a new poll", async () => {
  const ui = await card();
  ui.reply({ state: "ok", generated_at: "2026-10-19T12:00:00Z", providers: [
    { provider: "xai_api", display_name: "xAI API", freshness: "fresh", status_state: "ok", windows: [], readings: [
      { id: "ledger", label: "Ledger balance", value: "$2.50", expires_at: "2026-10-19T12:10:00Z" },
      { id: "invoice", label: "Reported · Month", value: "$4.00", expires_at: "2026-10-19T12:30:00Z" }] },
    { provider: "claude", display_name: "Claude", freshness: "fresh", status_state: "ok",
      windows: [{ id: "weekly", label: "Week", used_percent: 10, severity: "ok", limited: false }] }
  ] });
  await ui.poll();
  assert.equal(ui.body.querySelectorAll(".q-balance").length, 2);
  ui.clock.now = Date.parse("2026-10-19T12:15:00Z");
  ui.tick();
  assert.deepEqual(ui.body.querySelectorAll(".q-balance").map(row => texts(row)[0]), ["Reported · Month"]);
  ui.clock.now = Date.parse("2026-10-19T12:30:00Z");
  ui.tick();
  const [xai, claude] = ui.body.querySelectorAll(".q-prov");
  assert.equal(xai.querySelector(".q-balance"), null);
  assert.deepEqual(texts(xai.querySelector(".q-note")), ["Last reading expired."]);
  assert.deepEqual(texts(xai.querySelector(".q-fresh")), ["expired"]);
  assert.ok(claude.querySelector(".q-bar"), "legacy windows have no deadline");
});

test("a collection failure stays visible after the last good reading expires", async () => {
  const ui = await card();
  ui.reply({ state: "ok", generated_at: "2026-10-19T12:00:00Z", providers: [
    { provider: "cursor", display_name: "Cursor", freshness: "fresh", status_state: "ok", readings: [],
      status_message: "Last good reading; latest attempt: Authentication failed",
      expired_message: "Last reading expired · Latest attempt: Authentication failed",
      windows: [{ id: "weekly", label: "Week", used_percent: 30, severity: "ok", limited: false, expires_at: "2026-10-19T12:10:00Z" }] }
  ] });
  await ui.poll();
  assert.deepEqual(texts(ui.body.querySelector(".q-help")), ["Last good reading; latest attempt: Authentication failed"]);
  ui.clock.now = Date.parse("2026-10-19T12:10:00Z");
  ui.tick();
  assert.equal(ui.body.querySelector(".q-bar"), null);
  assert.deepEqual(texts(ui.body.querySelector(".q-note")), ["Last reading expired · Latest attempt: Authentication failed"]);
});
