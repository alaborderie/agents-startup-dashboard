// Run with: node --test internal/web/hq_ui_test.cjs
const assert = require("node:assert/strict");
const { readFileSync } = require("node:fs");
const { test } = require("node:test");
const vm = require("node:vm");

// DOM boundary only: the production crew renderer, state and selectors run intact.
class Element {
  constructor(tag) {
    this.tag = tag;
    this.children = [];
    this.attributes = {};
    this.className = "";
    this.style = { setProperty() {} };
    this.classList = { add: name => { this.className += " " + name; } };
    if (tag === "template") { this.content = { firstElementChild: new Element("svg") }; }
  }
  appendChild(node) { this.children.push(node); return node; }
  replaceChildren(...nodes) { this.children = nodes; }
  setAttribute(name, value) { this.attributes[name] = value; }
  getAttribute(name) { return this.attributes[name] || null; }
  addEventListener() {}
  querySelectorAll(selector) {
    return this.children.flatMap(node => [
      ...((node.className || "").split(" ").includes(selector.slice(1)) ? [node] : []),
      ...node.querySelectorAll(selector)
    ]);
  }
  set textContent(text) { this.children = []; this.text = String(text); }
  get textContent() { return (this.text || "") + this.children.map(n => n.textContent).join(""); }
}

function crew() {
  const nodes = { "crew-body": new Element("div"), "crew-meta": new Element("span") };
  const document = {
    body: new Element("body"),
    getElementById: id => nodes[id],
    createElement: tag => new Element(tag),
    createTextNode: text => { const node = new Element("#text"); node.textContent = text; return node; }
  };
  const context = vm.createContext({ document, Node: Element, URL, console });
  const script = readFileSync(`${__dirname}/static/hq.js`, "utf8");
  // Expose the closed-over renderer only inside this test; do not add a test API to production.
  const boot = "  var page = document.body";
  assert.ok(script.includes(boot));
  vm.runInContext(script.replace(boot, "  globalThis.crewTest = { state: S, render: renderCrew };\n" + boot), context);
  const { state, render } = context.crewTest;
  return {
    render(roles, runs, age = 0) {
      state.hq = { doc: { roles } };
      state.live = { runs, tick_running: runs.length > 0, age_seconds: age };
      state.liveAt = Date.now();
      render();
      return nodes["crew-body"];
    }
  };
}

const configured = [{ role: "developer", engine: "codex", model: "configured-default" }];
const run = (model = "actual-model", leg = "primary", slug = "alpha") => ({
  role: "developer", run_id: slug + "|developer", slug, engine: "claude", model, leg,
  started_at: new Date().toISOString()
});
const chips = body => body.querySelectorAll(".mono").map(node => node.textContent);

test("resting crew never displays configured or previously active models", () => {
  const ui = crew();
  assert.deepEqual(chips(ui.render(configured, [])), []);
  assert.deepEqual(chips(ui.render(configured, [run()])), ["actual-model"]);
  const body = ui.render([{ ...configured[0], model: "actual-model", working: true }], []);
  assert.deepEqual(chips(body), []);
  assert.match(body.querySelectorAll(".crew-tile")[0].getAttribute("aria-label"), /is resting/);
});

test("active crew uses each current leg, including concurrent runs and fallback", () => {
  const ui = crew();
  assert.deepEqual(chips(ui.render(configured, [run()])), ["actual-model"]);
  assert.deepEqual(chips(ui.render(configured, [run("fallback-model", "fallback")])), ["fallback-model"]);
  assert.deepEqual(chips(ui.render(configured, [run("one"), run("two", "primary", "beta")])), ["one", "two"]);
});

test("active crew with an unknown model never falls back to configuration", () => {
  assert.deepEqual(chips(crew().render(configured, [run("")])), []);
});

test("lost live signal hides the last known model until fresh data arrives", () => {
  const ui = crew();
  assert.deepEqual(chips(ui.render(configured, [run()], 3600)), []);
  assert.deepEqual(chips(ui.render(configured, [run("fresh-model")], 0)), ["fresh-model"]);
});
