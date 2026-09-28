/* Live log — an agent run drawn as typed steps (shared by the public page and HQ).
 *
 * The laptop turns each engine's JSON event stream into steps (agent-startup
 * bin/run_events.py `steps`): message, thinking, tool, error, notice, result, raw.
 * Each step is one rectangle whose border colour says what it is; the agent's own
 * messages stand out. Nobody can answer the agent, so nothing is a chat bubble:
 * every step takes the full width of a centred column.
 *
 * Updates are keyed by step id, so a push every few seconds only adds or replaces
 * the steps that changed; an expanded step stays expanded and the reader's scroll
 * position is kept unless they are following the bottom.
 *
 *   var log = LiveLog.mount(host, { who: "Developer", stick: true, onStick: fn });
 *   log.update(run, { working: true });   // run: { log_events, log_tail }
 */
(function () {
  "use strict";

  var SVG = "http://www.w3.org/2000/svg";

  // 24px line icons, drawn as single paths.
  var ICONS = {
    shell: "M4 5h16a1 1 0 0 1 1 1v12a1 1 0 0 1-1 1H4a1 1 0 0 1-1-1V6a1 1 0 0 1 1-1zM7 10l3 2.5L7 15M12.5 15H17",
    read: "M6 3h8l4 4v14H6zM14 3v4h4M9 12h6M9 16h6",
    search: "M10.5 4a6.5 6.5 0 1 1 0 13 6.5 6.5 0 0 1 0-13zM15.5 15.5 20 20",
    edit: "M4 20h4L19 9l-4-4L4 16zM13.5 6.5l4 4",
    web: "M12 3a9 9 0 1 1 0 18 9 9 0 0 1 0-18zM3 12h18M12 3c2.8 3 2.8 15 0 18M12 3c-2.8 3-2.8 15 0 18",
    task: "M7 4a2 2 0 1 1 0 4 2 2 0 0 1 0-4zM17 16a2 2 0 1 1 0 4 2 2 0 0 1 0-4zM7 8v4a4 4 0 0 0 4 4h4",
    todo: "M4 6l1.5 1.5L8 5M4 12l1.5 1.5L8 11M4 18l1.5 1.5L8 17M11 6h9M11 12h9M11 18h9",
    skill: "M12 3l2.2 5.8L20 11l-5.8 2.2L12 19l-2.2-5.8L4 11l5.8-2.2z",
    other: "M9 3h6v4h4v6h-4v4H9v-4H5V7h4z",
    thinking: "M9 18h6M10 21h4M12 3a6 6 0 0 1 3.5 10.9V16h-7v-2.1A6 6 0 0 1 12 3z",
    error: "M12 3l9.5 17h-19zM12 10v4M12 17.5v.5",
    result: "M5 21V4M5 4h11l-2 4 2 4H5",
    message: "M4 5h16v11H9l-5 4z",
    raw: "M8 7l-5 5 5 5M16 7l5 5-5 5"
  };

  var VERBS = {
    shell: ["Ran", "Running"],
    read: ["Read", "Reading"],
    edit: ["Edited", "Editing"],
    web: ["Searched the web", "Searching the web"],
    task: ["Delegated", "Delegating"],
    todo: ["Updated the plan", "Updating the plan"],
    skill: ["Loaded skill", "Loading skill"],
    other: ["Used", "Using"]
  };

  // A few tool names deserve their own verb and icon inside their category.
  var BY_NAME = {
    grep: { verb: ["Searched code", "Searching code"], icon: "search" },
    glob: { verb: ["Listed files", "Listing files"], icon: "search" },
    list: { verb: ["Listed", "Listing"], icon: "search" },
    ls: { verb: ["Listed", "Listing"], icon: "search" },
    write: { verb: ["Wrote", "Writing"] },
    apply_patch: { verb: ["Patched", "Patching"] },
    webfetch: { verb: ["Fetched", "Fetching"] },
    web_fetch: { verb: ["Fetched", "Fetching"] }
  };

  function el(tag, cls, text) {
    var node = document.createElement(tag);
    if (cls) { node.className = cls; }
    if (text !== undefined && text !== null && text !== "") { node.textContent = String(text); }
    return node;
  }

  function add(parent) {
    for (var i = 1; i < arguments.length; i++) {
      var child = arguments[i];
      if (child) { parent.appendChild(child); }
    }
    return parent;
  }

  function icon(name) {
    var svg = document.createElementNS(SVG, "svg");
    svg.setAttribute("viewBox", "0 0 24 24");
    svg.setAttribute("aria-hidden", "true");
    svg.setAttribute("class", "ll-ico");
    var path = document.createElementNS(SVG, "path");
    path.setAttribute("d", ICONS[name] || ICONS.other);
    svg.appendChild(path);
    return svg;
  }

  function str(v) { return v === undefined || v === null ? "" : String(v); }

  function baseName(name) {
    var n = str(name).toLowerCase();
    if (n.indexOf("mcp__") === 0) { n = n.split("__").pop(); }
    return n;
  }

  function duration(ms) {
    // Under a tenth of a second is instant; a figure there is noise.
    if (!(ms >= 100)) { return ""; }
    if (ms < 950) { return (ms / 1000).toFixed(1) + "s"; }
    var s = Math.round(ms / 1000);
    if (s < 60) { return s + "s"; }
    var m = Math.floor(s / 60);
    return m + "m " + (s % 60 ? (s % 60) + "s" : "");
  }

  function clock(ts) {
    if (!(ts > 0)) { return ""; }
    try {
      return new Date(ts).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
    } catch (e) { return ""; }
  }

  function tokens(n) {
    if (!(n > 0)) { return ""; }
    return "~" + (n >= 1000 ? (n / 1000).toFixed(1).replace(/\.0$/, "") + "k" : String(n)) + " tokens";
  }

  // ------------------------------------------------------------------ light markdown

  var INLINE = /(`[^`\n]+`)|(\*\*[^*\n]+\*\*)|(\[([^\]\n]+)\]\((https?:\/\/[^)\s]+)\))|(https?:\/\/[^\s<>()]+[^\s<>().,;:!?'"])/g;

  function inline(parent, text) {
    var last = 0;
    var match;
    INLINE.lastIndex = 0;
    while ((match = INLINE.exec(text))) {
      if (match.index > last) { parent.appendChild(document.createTextNode(text.slice(last, match.index))); }
      if (match[1]) {
        parent.appendChild(el("code", "ll-code", match[1].slice(1, -1)));
      } else if (match[2]) {
        parent.appendChild(el("strong", null, match[2].slice(2, -2)));
      } else {
        var href = match[3] ? match[5] : match[6];
        var a = el("a", "ll-link", match[3] ? match[4] : href);
        a.href = href;
        a.target = "_blank";
        a.rel = "noopener noreferrer nofollow ugc";
        parent.appendChild(a);
      }
      last = INLINE.lastIndex;
    }
    if (last < text.length) { parent.appendChild(document.createTextNode(text.slice(last))); }
    return parent;
  }

  // Paragraphs, fenced code, bullet and numbered lists, headings. Built as DOM
  // nodes, never HTML: agent text is untrusted.
  function markdown(text) {
    var root = el("div", "ll-md");
    var lines = str(text).replace(/\r\n?/g, "\n").split("\n");
    var i = 0;
    var para = [];
    function flush() {
      if (para.length) { root.appendChild(inline(el("p"), para.join(" "))); para = []; }
    }
    while (i < lines.length) {
      var line = lines[i];
      var fence = /^\s*```/.test(line);
      if (fence) {
        flush();
        var code = [];
        i++;
        while (i < lines.length && !/^\s*```/.test(lines[i])) { code.push(lines[i]); i++; }
        i++;
        root.appendChild(el("pre", "ll-pre", code.join("\n")));
        continue;
      }
      var bullet = /^\s*(?:[-*•]|\d+[.)])\s+/;
      if (bullet.test(line)) {
        flush();
        var ordered = /^\s*\d/.test(line);
        var list = el(ordered ? "ol" : "ul", "ll-list");
        while (i < lines.length && bullet.test(lines[i])) {
          list.appendChild(inline(el("li"), lines[i].replace(bullet, "")));
          i++;
        }
        root.appendChild(list);
        continue;
      }
      var heading = /^\s*#{1,6}\s+(.*)$/.exec(line);
      if (heading) {
        flush();
        root.appendChild(inline(el("p", "ll-h"), heading[1]));
      } else if (!line.trim()) {
        flush();
      } else {
        para.push(line.trim());
      }
      i++;
    }
    flush();
    return root;
  }

  // ------------------------------------------------------------------ steps

  function pre(cls, text) {
    return el("pre", "ll-pre " + (cls || ""), text);
  }

  function diff(text) {
    var block = el("pre", "ll-pre ll-diff");
    str(text).split("\n").forEach(function (line, index) {
      if (index) { block.appendChild(document.createTextNode("\n")); }
      var c = line.charAt(0);
      var span = el("span", c === "+" ? "ll-add" : c === "-" ? "ll-del" : c === "@" ? "ll-hunk" : null);
      span.textContent = line;
      block.appendChild(span);
    });
    return block;
  }

  function todoList(step) {
    var list = el("ul", "ll-todos");
    (step.todos || []).forEach(function (t) {
      var status = str(t.status);
      var li = el("li", "ll-todo is-" + (status === "completed" ? "done" : status === "in_progress" ? "doing" : "pending"));
      li.appendChild(el("span", "ll-todo-box"));
      li.appendChild(el("span", "ll-todo-text", t.text));
      list.appendChild(li);
    });
    return list;
  }

  // A step with details is a <details>: native keyboard and screen-reader
  // support, and its open state lives on the element the keyed update reuses.
  function card(cls, summaryKids, body, open) {
    var node;
    if (body) {
      node = el("details", "ll-step " + cls);
      var summary = el("summary", "ll-row");
      summaryKids.forEach(function (k) { if (k) { summary.appendChild(k); } });
      summary.appendChild(el("span", "ll-chev"));
      node.appendChild(summary);
      node.appendChild(body);
      if (open) { node.open = true; }
    } else {
      node = el("div", "ll-step " + cls);
      var row = el("div", "ll-row");
      summaryKids.forEach(function (k) { if (k) { row.appendChild(k); } });
      node.appendChild(row);
    }
    return node;
  }

  function meta(parts) {
    var text = parts.filter(Boolean).join(" · ");
    return text ? el("span", "ll-meta", text) : null;
  }

  function statusBadge(status) {
    if (status === "running") { return el("span", "ll-badge is-running", "running"); }
    if (status === "error") { return el("span", "ll-badge is-error", "failed"); }
    return null;
  }

  function toolStep(step) {
    var tool = str(step.tool) || "other";
    var name = baseName(step.name);
    var special = BY_NAME[name] || {};
    var verbs = special.verb || VERBS[tool] || VERBS.other;
    var running = step.status === "running";
    var verb = verbs[running ? 1 : 0];
    if (tool === "other" && step.name) { verb += " " + step.name; }
    var mono = tool === "shell" || tool === "read" || tool === "edit" || (tool === "web" && /^https?:/.test(str(step.title)));
    var title = el(mono ? "code" : "span", "ll-title", step.title || "");
    var changes = null;
    if (step.adds || step.dels) {
      changes = el("span", "ll-changes");
      if (step.adds) { changes.appendChild(el("span", "ll-add", "+" + step.adds)); }
      if (step.dels) { changes.appendChild(el("span", "ll-del", "−" + step.dels)); }
    }
    var summary = [icon(special.icon || (tool === "read" && /search|grep/.test(name) ? "search" : tool)),
      el("span", "ll-verb", verb), title, changes, statusBadge(step.status), meta([duration(step.ms)])];

    var body = null;
    if (tool === "todo" && step.todos && step.todos.length) {
      var node = card("ll-tool", summary, null);
      node.appendChild(todoList(step));
      return decorate(node, step, tool);
    }
    var parts = [];
    if (step.detail) { parts.push(el("p", "ll-detail", step.detail)); }
    if (step.command) { parts.push(pre("ll-cmd", step.command)); }
    if (step.diff) { parts.push(diff(step.diff)); }
    if (step.output) { parts.push(pre(step.status === "error" ? "ll-out is-error" : "ll-out", step.output)); }
    if (parts.length) {
      body = el("div", "ll-body");
      parts.forEach(function (p) { body.appendChild(p); });
    }
    return decorate(card("ll-tool", summary, body, step.status === "error"), step, tool);
  }

  function decorate(node, step, tool) {
    if (tool) { node.setAttribute("data-tool", tool); }
    if (step.status) { node.setAttribute("data-status", step.status); }
    if (step.sub) { node.classList.add("is-sub"); }
    return node;
  }

  function messageStep(step, who) {
    var node = el("article", "ll-step ll-msg");
    var head = el("div", "ll-msg-head");
    add(head, icon("message"), el("span", "ll-msg-who", step.sub ? "Sub-agent" : who || "Agent"),
      step.ts ? el("time", "ll-meta", clock(step.ts)) : null);
    add(node, head, markdown(step.text));
    return decorate(node, step);
  }

  function thinkingStep(step) {
    var text = str(step.text);
    var summary = [icon("thinking"), el("span", "ll-verb", "Thinking"),
      text ? el("span", "ll-title ll-preview", text.split("\n")[0]) : el("span", "ll-title is-muted", "reasoning not shown"),
      meta([tokens(step.tokens)])];
    var body = null;
    if (text && (text.length > 90 || text.indexOf("\n") !== -1)) {
      body = el("div", "ll-body");
      body.appendChild(el("p", "ll-think-text", text));
    }
    return decorate(card("ll-think", summary, body), step);
  }

  function errorStep(step) {
    var node = el("div", "ll-step ll-err");
    add(node, add(el("div", "ll-row"), icon("error"), el("span", "ll-verb", step.title || "Error")),
      step.text ? pre("ll-out", step.text) : null);
    return decorate(node, step);
  }

  function resultStep(step) {
    var node = el("div", "ll-step ll-result");
    add(node, add(el("div", "ll-row"), icon("result"), el("span", "ll-verb", step.title || "Finished")),
      step.text ? pre("ll-out", step.text) : null);
    return decorate(node, step);
  }

  function noticeStep(step) {
    var node = el("div", "ll-notice");
    node.appendChild(el("span", null, [step.title, step.text].filter(Boolean).join(" · ")));
    return decorate(node, step);
  }

  function rawStep(step) {
    var node = el("div", "ll-step ll-raw");
    add(node, add(el("div", "ll-row"), icon("raw"), el("span", "ll-verb", "Output")), pre("ll-out", step.text || step.title || ""));
    return decorate(node, step);
  }

  function build(step, who) {
    switch (step.kind) {
      case "message": return messageStep(step, who);
      case "thinking": return thinkingStep(step);
      case "tool": return toolStep(step);
      case "error": return errorStep(step);
      case "result": return resultStep(step);
      case "notice": return noticeStep(step);
      default: return rawStep(step);
    }
  }

  // ------------------------------------------------------------------ component

  function LiveLog(host, opts) {
    opts = opts || {};
    this.who = opts.who || "Agent";
    this.onStick = opts.onStick || function () {};
    this.stick = opts.stick !== false;
    this.nodes = {};
    this.host = host;
    host.classList.add("ll");
    this.scroller = el("div", "ll-scroll");
    this.scroller.tabIndex = 0;
    this.scroller.setAttribute("role", "log");
    this.scroller.setAttribute("aria-live", "off");
    this.scroller.setAttribute("aria-label", opts.label || "Agent steps");
    this.feed = el("div", "ll-feed");
    this.empty = el("p", "ll-empty", "Waiting for the first step…");
    this.working = el("div", "ll-working");
    add(this.working, el("span", "ll-dots"), el("span", null, this.who + " is working"));
    add(this.scroller, this.feed);
    this.jump = el("button", "ll-jump", "Jump to latest");
    this.jump.type = "button";
    this.jump.hidden = true;
    host.replaceChildren(this.scroller, this.jump);
    var self = this;
    this.scroller.addEventListener("scroll", function () {
      var atBottom = self.atBottom();
      if (atBottom !== self.stick) {
        self.stick = atBottom;
        self.onStick(atBottom);
      }
      if (atBottom) { self.jump.hidden = true; }
    });
    // A step the reader opens near the bottom must not be scrolled away by the
    // next push. Only their clicks count: a failed step opens itself on arrival.
    this.feed.addEventListener("click", function (event) {
      if (!event.target.closest || !event.target.closest("summary")) { return; }
      window.setTimeout(function () {
        if (self.stick && !self.atBottom()) {
          self.stick = false;
          self.onStick(false);
        }
      }, 0);
    });
    this.jump.addEventListener("click", function () {
      self.stick = true;
      self.onStick(true);
      self.jump.hidden = true;
      self.toBottom();
    });
  }

  LiveLog.prototype.atBottom = function () {
    return this.scroller.scrollHeight - this.scroller.scrollTop - this.scroller.clientHeight < 32;
  };

  LiveLog.prototype.toBottom = function () {
    this.scroller.scrollTop = this.scroller.scrollHeight;
  };

  LiveLog.prototype.update = function (run, state) {
    state = state || {};
    var steps = run && Array.isArray(run.log_events) ? run.log_events : [];
    if (!steps.length && run && run.log_tail) {
      steps = [{ id: "tail", kind: "raw", text: String(run.log_tail) }];
    }
    var feed = this.feed;
    var previous = this.nodes;
    var next = {};
    var order = [];
    var changed = false;
    var self = this;
    steps.forEach(function (step) {
      if (!step || typeof step !== "object") { return; }
      var id = str(step.id) || "step-" + order.length;
      while (next[id]) { id += "~"; }
      var sig = JSON.stringify(step);
      var old = previous[id];
      var entry = old;
      if (!old || old.sig !== sig) {
        var node = build(step, self.who);
        if (old) {
          // Keep the reader's choice: a step they opened stays open when it updates.
          if (old.node.tagName === "DETAILS" && node.tagName === "DETAILS") { node.open = old.node.open; }
          if (old.node.parentNode === feed) { feed.replaceChild(node, old.node); }
        }
        entry = { sig: sig, node: node };
        changed = true;
      }
      next[id] = entry;
      order.push(entry.node);
    });
    Object.keys(previous).forEach(function (id) {
      if (!next[id]) {
        changed = true;
        if (previous[id].node.parentNode === feed) { feed.removeChild(previous[id].node); }
      }
    });
    this.nodes = next;

    // What is left in the feed is already in step order (steps only ever append,
    // or fall off the front), so this pass only inserts the new ones.
    if (this.empty.parentNode === feed) { feed.removeChild(this.empty); }
    if (this.working.parentNode === feed) { feed.removeChild(this.working); }
    var cursor = feed.firstChild;
    order.forEach(function (node) {
      if (node === cursor) { cursor = cursor.nextSibling; return; }
      feed.insertBefore(node, cursor);
    });
    while (cursor) {
      var stray = cursor;
      cursor = cursor.nextSibling;
      feed.removeChild(stray);
    }
    if (!order.length) {
      this.empty.textContent = state.ended ? "No output was recorded for this run." : "Waiting for the first step…";
      feed.appendChild(this.empty);
    }
    if (state.working) { feed.appendChild(this.working); }
    if (changed) {
      if (this.stick) {
        this.toBottom();
        // The host may not be in the document yet (a drawer builds off-screen).
        window.setTimeout(function () { if (self.stick) { self.toBottom(); } }, 0);
      } else {
        this.jump.hidden = false;
      }
    }
  };

  window.LiveLog = {
    mount: function (host, opts) { return new LiveLog(host, opts); }
  };
})();
