/* HQ control room.
 *
 * One script for three pages, picked by <body data-page>:
 *   "hq"          the authenticated control room (WebSocket + polling fallback)
 *   "login"       the unauthenticated /hq screen (decorative crew only)
 *   "login-link"  the magic-link landing page (/hq/login#t=…)
 *
 * Security rules this file follows everywhere:
 *   - Every string from the server is untrusted. It reaches the DOM only through
 *     textContent / createTextNode / setAttribute on non-URL attributes.
 *   - innerHTML is used for exactly one thing: static SVG markup authored in this
 *     file (icons and crew art), never interpolated with server data.
 *   - href is only ever set from safeUrl(), which accepts http: and https: only.
 *   - No inline styles: dynamic values go through el.style.setProperty (CSSOM),
 *     which the page CSP (style-src 'self') allows. */
(function () {
  "use strict";

  // ------------------------------------------------------------------ constants

  // Lookup tables keyed by server data have no prototype, so a key such as
  // "constructor" or "__proto__" misses instead of returning an Object method.
  function dict(obj) {
    var d = Object.create(null);
    Object.keys(obj || {}).forEach(function (k) { d[k] = obj[k]; });
    return d;
  }

  var STAGES = [
    { id: "IDEATION", label: "Ideation", role: "inventor", phase: 0 },
    { id: "SCREENED", label: "Screening", role: "screener", phase: 0 },
    { id: "BUSINESS_CASE", label: "Business case", role: "analyst", phase: 0 },
    { id: "GATE_BUILD", label: "Build go/no-go", gate: "build", phase: 0 },
    { id: "ARCHITECTURE", label: "Architecture", role: "architect", phase: 1 },
    { id: "BACKLOG", label: "Backlog", role: "pm", phase: 1 },
    { id: "DESIGN", label: "Design", role: "designer", phase: 1 },
    { id: "DESIGN_REVIEW", label: "Design review", gate: "design", phase: 1 },
    { id: "BUILD", label: "Build", role: "developer", phase: 2 },
    { id: "REVIEW", label: "Review", role: "reviewer", also: "security", phase: 2 },
    { id: "WAIT_MERGE", label: "Waiting for merge", wait: true, phase: 2 },
    { id: "INFRA", label: "Infra", role: "devops", phase: 3 },
    { id: "GATE_DEPLOY", label: "Deploy go/no-go", gate: "deploy", phase: 3 },
    { id: "WAIT_DEPLOY", label: "Waiting for deploy", wait: true, phase: 3 },
    { id: "DEPLOYED", label: "Launch", role: "sales", phase: 3 },
    { id: "GTM", label: "Go to market", role: "documentation", phase: 3 }
  ];
  var PHASES = ["Discover", "Plan", "Build", "Ship"];
  var STAGE_BY_ID = dict();
  STAGES.forEach(function (s, i) { s.index = i; STAGE_BY_ID[s.id] = s; });

  var FALLBACK_ROLES = ["inventor", "screener", "analyst", "architect", "pm", "designer",
    "developer", "reviewer", "security", "devops", "sales", "documentation", "doctor", "triage"];

  var GATES = dict({
    build: { kicker: "Gate 1 · build go/no-go", ask: "The business case is in. Approve to start architecture and the build, or send it back." },
    design: { kicker: "Design review", ask: "The designer wants your eyes on the canvas. Approve it to start building." },
    deploy: { kicker: "Gate 2 · deploy go/no-go", ask: "Infra is ready. Approve to go live." }
  });

  var CMD_LABEL = dict({
    approve: "Approve", changes: "Request changes", reject: "Reject", note: "Note",
    answer: "Answer & resume", resume: "Resume", suggest: "Idea", tick: "Tick"
  });
  var ACTION_KINDS = dict({ approve: 1, changes: 1, reject: 1, answer: 1, resume: 1 });
  var TEXT_LIMIT = dict({ changes: 3000, reject: 3000, note: 3000, answer: 3000, suggest: 2000 });
  var CMD_STATUSES = dict({ queued: 1, delivered: 1, done: 1, failed: 1 });
  var HQ_STALE_S = 180;
  var LIVE_STALE_S = 90;
  var POLL_MS = 10000;

  // ------------------------------------------------------------------ icons

  var ICONS = dict({
    bulb: '<path d="M9 18h6M10 21h4M12 3a6 6 0 0 0-3.5 10.9c.6.5 1 1.2 1 2V16h5v-.1c0-.8.4-1.5 1-2A6 6 0 0 0 12 3z"/>',
    funnel: '<path d="M3 4h18l-7 8.5V19l-4 2v-8.5z"/>',
    bars: '<path d="M5 20V11M11 20V5M17 20v-7M3 20h18"/>',
    grid: '<rect x="3" y="3" width="18" height="18" rx="2"/><path d="M3 9h18M3 15h18M9 3v18M15 3v18"/>',
    kanban: '<rect x="3" y="3" width="18" height="18" rx="3"/><path d="M8 7v7M12 7v4M16 7v9"/>',
    palette: '<path d="M12 3a9 9 0 1 0 0 18c1.2 0 1.8-.8 1.8-1.7 0-.5-.2-.9-.5-1.2-.3-.3-.5-.7-.5-1.2 0-.9.8-1.7 1.7-1.7H17a4 4 0 0 0 4-4c0-4.4-4-8.2-9-8.2z"/><circle cx="7.5" cy="11" r="1.2"/><circle cx="10.5" cy="7" r="1.2"/><circle cx="15" cy="7.5" r="1.2"/>',
    terminal: '<rect x="2.5" y="4" width="19" height="16" rx="3"/><path d="M7 10l3 2.5L7 15M12.5 15H17"/>',
    search: '<circle cx="10.5" cy="10.5" r="6.5"/><path d="M20 20l-4.8-4.8"/>',
    shield: '<path d="M12 3l8 3v6c0 5-3.4 8.3-8 9.5C7.4 20.3 4 17 4 12V6z"/><path d="M8.5 12l2.5 2.5 4.5-5"/>',
    gear: '<circle cx="12" cy="12" r="3.2"/><path d="M12 2.5v3M12 18.5v3M2.5 12h3M18.5 12h3M5.3 5.3l2.1 2.1M16.6 16.6l2.1 2.1M5.3 18.7l2.1-2.1M16.6 7.4l2.1-2.1"/>',
    megaphone: '<path d="M3 10v4a1 1 0 0 0 1 1h3l8 5V4L7 9H4a1 1 0 0 0-1 1zM19 8.5a4.5 4.5 0 0 1 0 7"/>',
    book: '<path d="M4 4.5A1.5 1.5 0 0 1 5.5 3H20v15H5.5A1.5 1.5 0 0 0 4 19.5zM4 19.5A1.5 1.5 0 0 0 5.5 21H20v-3"/>',
    cross: '<path d="M9 3h6v6h6v6h-6v6H9v-6H3V9h6z"/>',
    siren: '<path d="M6 18v-6a6 6 0 0 1 12 0v6M4 18h16v3H4zM12 2v2M4.2 5.2l1.4 1.4M19.8 5.2l-1.4 1.4"/>',
    play: '<path d="M7 4.5v15l12-7.5z"/>',
    check: '<path d="M4.5 12.5l5 5 10-11"/>',
    x: '<path d="M6 6l12 12M18 6L6 18"/>',
    swap: '<path d="M4 8h13l-3.5-3.5M20 16H7l3.5 3.5"/>',
    pause: '<path d="M8 5v14M16 5v14"/>',
    ban: '<circle cx="12" cy="12" r="8.5"/><path d="M6 6l12 12"/>',
    flag: '<path d="M5 21V4M5 4h11l-2 4 2 4H5"/>',
    sparkle: '<path d="M12 3l1.8 5.4L19 10l-5.2 1.6L12 17l-1.8-5.4L5 10l5.2-1.6z"/>',
    resume: '<path d="M4 12a8 8 0 1 0 2.4-5.7M4 4v4h4"/>',
    arrow: '<path d="M4 12h15M13 6l6 6-6 6"/>',
    ext: '<path d="M14 4h6v6M20 4l-9 9M18 14v5a1 1 0 0 1-1 1H5a1 1 0 0 1-1-1V7a1 1 0 0 1 1-1h5"/>',
    back: '<path d="M15 5l-7 7 7 7"/>',
    bolt: '<path d="M13 2L4 14h7l-1 8 9-12h-7z"/>',
    laptop: '<rect x="4" y="5" width="16" height="11" rx="1.5"/><path d="M2 19h20"/>',
    alert: '<path d="M12 3.5l9.5 16.5h-19z"/><path d="M12 10v4.5M12 17.5v.1"/>',
    users: '<circle cx="9" cy="8" r="3.5"/><path d="M2.5 20a6.5 6.5 0 0 1 13 0M16 4.5a3.5 3.5 0 0 1 0 7M18 14.5a6.5 6.5 0 0 1 3.5 5.5"/>',
    columns: '<rect x="3" y="4" width="18" height="16" rx="2"/><path d="M9 4v16M15 4v16"/>',
    pulse: '<path d="M3 12h4l3-7 4 14 3-7h4"/>',
    archive: '<rect x="3" y="4" width="18" height="5" rx="1"/><path d="M5 9v10a1 1 0 0 0 1 1h12a1 1 0 0 0 1-1V9M10 13h4"/>',
    hourglass: '<path d="M6 3h12M6 21h12M7 3c0 5 5 6 5 9s-5 4-5 9M17 3c0 5-5 6-5 9s5 4 5 9"/>',
    note: '<path d="M5 3h10l4 4v14H5z"/><path d="M9 11h6M9 15h4"/>',
    send: '<path d="M21 3L10 14M21 3l-7 18-4-7-7-4z"/>',
    question: '<circle cx="12" cy="12" r="9"/><path d="M9.5 9.5a2.5 2.5 0 1 1 3.5 2.3c-.6.3-1 .9-1 1.6v.6M12 17v.1"/>',
    log: '<path d="M4 6h16M4 12h10M4 18h13"/>'
  });

  function svgFrom(markup) {
    var t = document.createElement("template");
    t.innerHTML = markup; // static, authored in this file only
    return t.content.firstElementChild;
  }

  function icon(name, cls) {
    var span = document.createElement("span");
    span.className = "icon" + (cls ? " " + cls : "");
    span.setAttribute("aria-hidden", "true");
    span.appendChild(svgFrom('<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.2" stroke-linecap="round" stroke-linejoin="round" focusable="false">' + (ICONS[name] || ICONS.question) + "</svg>"));
    return span;
  }

  // ------------------------------------------------------------------ crew art

  function gearTeeth() {
    var out = "";
    for (var a = 0; a < 360; a += 45) {
      out += '<rect class="b" x="27" y="1.5" width="10" height="12" rx="2.5" transform="rotate(' + a + ' 32 32)"/>';
    }
    return '<g class="gear-teeth">' + out + "</g>";
  }

  // body: SVG (64×64) drawn under the face; face: [x, y, scale]; dark: face drawn in role colour
  var ART = {
    inventor: { name: "Inventor", tag: "dreams up ideas", glyph: "bulb", face: [32, 27, 1],
      body: '<path class="b" d="M32 4C19.3 4 9 13.9 9 26.3c0 7.8 4 13.8 8.9 17.9 1.5 1.3 2.1 2.8 2.1 4.6V51h24v-2.2c0-1.8.6-3.3 2.1-4.6C51 40.1 55 34.1 55 26.3 55 13.9 44.7 4 32 4z"/><rect class="d" x="22" y="53" width="20" height="4" rx="2"/><rect class="d" x="25.5" y="58.5" width="13" height="3.5" rx="1.75"/>' },
    screener: { name: "Screener", tag: "kills weak ideas early", glyph: "funnel", face: [32, 28, 1],
      body: '<path class="b" d="M32 4l24.2 14v28L32 60 7.8 46V18z"/><path class="d-soft" d="M22 43h20l-7 7v4h-6v-4z"/>' },
    analyst: { name: "Analyst", tag: "sizes the market", glyph: "bars", face: [32, 24, .95],
      body: '<rect class="b" x="7" y="7" width="50" height="50" rx="16"/><rect class="d-soft" x="17" y="41" width="7" height="9" rx="2"/><rect class="d-soft" x="28.5" y="37" width="7" height="13" rx="2"/><rect class="d-soft" x="40" y="33" width="7" height="17" rx="2"/>' },
    architect: { name: "Architect", tag: "draws the system", glyph: "grid", face: [32, 30, 1],
      body: '<rect class="b" x="7" y="7" width="50" height="50" rx="6"/><path class="ln" d="M7 19.5h50M7 32h50M7 44.5h50M19.5 7v50M32 7v50M44.5 7v50"/><path class="d-soft" d="M43 53h10V43z"/>' },
    pm: { name: "PM", tag: "slices the backlog", glyph: "kanban", face: [32, 28, 1],
      body: '<rect class="b" x="10" y="9" width="44" height="51" rx="12"/><rect class="d" x="23" y="4" width="18" height="9" rx="4"/><rect class="d-soft" x="17" y="42" width="8" height="11" rx="2"/><rect class="d-soft" x="28" y="42" width="8" height="7" rx="2"/><rect class="d-soft" x="39" y="42" width="8" height="9" rx="2"/>' },
    designer: { name: "Designer", tag: "makes it lovely", glyph: "palette", face: [31, 31, 1],
      body: '<path class="b" d="M33.5 5.5c9.8.4 19.6 6.2 22.3 15.8 2.8 10-1.4 18.8-6.6 26.2-5.1 7.2-12.8 12.4-21.4 10.8-8.6-1.6-17.2-8.6-19.3-17.6C6.3 31.7 9.8 21 16.4 14 21 9 26.4 5.2 33.5 5.5z"/><circle class="dot-a b-stroke" cx="47" cy="14" r="4.2"/><circle class="dot-b b-stroke" cx="53" cy="25" r="3.4"/><circle class="dot-c b-stroke" cx="17" cy="47" r="3.6"/>' },
    developer: { name: "Developer", tag: "writes the code", glyph: "terminal", face: [32, 34, .95], dark: true,
      body: '<rect class="b" x="4" y="8" width="56" height="48" rx="10"/><rect class="screen" x="9.5" y="18" width="45" height="33" rx="5"/><circle class="d" cx="11.5" cy="13" r="2"/><circle class="d" cx="17.5" cy="13" r="2"/><circle class="d" cx="23.5" cy="13" r="2"/>' },
    reviewer: { name: "Reviewer", tag: "reads every diff", glyph: "search", face: [27, 27, .85],
      body: '<rect class="b" x="41" y="40" width="10" height="23" rx="5" transform="rotate(-45 46 51.5)"/><circle class="b" cx="27" cy="27" r="22"/><circle class="lens" cx="27" cy="27" r="15.5"/>' },
    security: { name: "Security", tag: "guards the gates", glyph: "shield", face: [32, 28, 1],
      body: '<path class="b" d="M32 3.5l23 8.2v16.8c0 14.8-9.6 25.8-23 31.5C18.6 54.3 9 43.3 9 28.5V11.7z"/><path class="d-soft" d="M32 9.5l17.5 6.2v12.8c0 11.3-7 20-17.5 24.8z"/>' },
    devops: { name: "DevOps", tag: "ships the infra", glyph: "gear", face: [32, 32, 1],
      body: gearTeeth() + '<circle class="b" cx="32" cy="32" r="22"/>' },
    sales: { name: "Sales", tag: "finds the buyers", glyph: "megaphone", face: [32, 27, 1],
      body: '<path class="b" d="M13 6h38a9 9 0 0 1 9 9v25a9 9 0 0 1-9 9H31L18 60V49h-5a9 9 0 0 1-9-9V15a9 9 0 0 1 9-9z"/>' },
    documentation: { name: "Docs", tag: "writes it down", glyph: "book", face: [30, 30, 1],
      body: '<path class="b" d="M12 4h28l16 16v36a4 4 0 0 1-4 4H12a4 4 0 0 1-4-4V8a4 4 0 0 1 4-4z"/><path class="d-soft" d="M40 4v12a4 4 0 0 0 4 4h12z"/><path class="ln ln-strong" d="M16 47h26M16 52h18"/>' },
    doctor: { name: "Doctor", tag: "heals the loop", glyph: "cross", face: [32, 33, 1],
      body: '<path class="b" d="M24 4h16a3 3 0 0 1 3 3v14h14a3 3 0 0 1 3 3v17a3 3 0 0 1-3 3H43v13a3 3 0 0 1-3 3H24a3 3 0 0 1-3-3V44H7a3 3 0 0 1-3-3V24a3 3 0 0 1 3-3h14V7a3 3 0 0 1 3-3z"/><path class="plus" d="M30 8.5h4v3h3v4h-3v3h-4v-3h-3v-4h3z"/>' },
    triage: { name: "Triage", tag: "sorts the mess", glyph: "siren", face: [32, 36, 1],
      body: '<path class="b" d="M32 9l25 26-25 26L7 35z"/><path class="siren" d="M25 12.5a7 7 0 0 1 14 0z"/>' },
    founder: { name: "You", tag: "the founder", glyph: "flag", face: [32, 38, 1],
      body: '<circle class="b" cx="32" cy="37" r="23"/><path class="crown" d="M17 18l4-13 11 8 11-8 4 13z"/>' },
    unknown: { name: "", tag: "crew member", glyph: "question", face: [32, 33, 1],
      body: '<path class="ln ln-strong" d="M32 8V2"/><circle class="b" cx="32" cy="1.5" r="3"/><circle class="b" cx="32" cy="34" r="25"/>' }
  };

  function faceSVG(f, dark) {
    return '<g class="face' + (dark ? " face-dark" : "") + '" transform="translate(' + f[0] + " " + f[1] + ") scale(" + f[2] + ')">' +
      '<g class="eyes awake"><ellipse class="eye" cx="-7" cy="0" rx="3.1" ry="3.8"/><ellipse class="eye" cx="7" cy="0" rx="3.1" ry="3.8"/>' +
      '<circle class="glint" cx="-6" cy="-1.5" r="1.1"/><circle class="glint" cx="8" cy="-1.5" r="1.1"/></g>' +
      '<path class="lid asleep" d="M-10 .5q3 3 6 0M4 .5q3 3 6 0"/>' +
      '<circle class="blush" cx="-12.5" cy="6" r="2.6"/><circle class="blush" cx="12.5" cy="6" r="2.6"/>' +
      '<path class="mouth awake" d="M-4.5 6.5q4.5 4.5 9 0"/><path class="mouth asleep" d="M-2.5 8h5"/></g>';
  }

  function roleKey(role) {
    var r = String(role || "").toLowerCase();
    return Object.prototype.hasOwnProperty.call(ART, r) && r !== "unknown" ? r : "unknown";
  }

  function roleName(role) {
    var k = roleKey(role);
    if (k !== "unknown") { return ART[k].name; }
    var r = String(role || "agent");
    return r.charAt(0).toUpperCase() + r.slice(1);
  }

  function hueFor(str) {
    var h = 0;
    for (var i = 0; i < str.length; i++) { h = (h * 31 + str.charCodeAt(i)) >>> 0; }
    return "hsl(" + (h % 360) + " 70% 72%)";
  }

  // Apply the role colour class (and a hashed hue for roles we have never met).
  function roleClass(el, role) {
    var k = roleKey(role);
    el.classList.add("role-" + k);
    if (k === "unknown") { el.style.setProperty("--role-hue", hueFor(String(role || ""))); }
    return el;
  }

  function avatar(role, size, opts) {
    opts = opts || {};
    var k = roleKey(role);
    var art = ART[k];
    var span = document.createElement("span");
    span.className = "av av-" + (size || "md") + (opts.awake ? " is-awake" : "") + (opts.working ? " is-working" : "");
    span.setAttribute("aria-hidden", "true");
    roleClass(span, role);
    span.appendChild(svgFrom('<svg viewBox="0 0 64 64" focusable="false"><g class="av-body">' + art.body + faceSVG(art.face, art.dark) + "</g></svg>"));
    return span;
  }

  // ------------------------------------------------------------------ DOM helpers

  function $(id) { return document.getElementById(id); }

  function h(tag, props) {
    var el = document.createElement(tag);
    if (props) {
      Object.keys(props).forEach(function (k) {
        var v = props[k];
        if (v === null || v === undefined || v === false) { return; }
        if (k === "class") { el.className = v; }
        else if (k === "text") { el.textContent = v; }
        else if (k === "on") { Object.keys(v).forEach(function (ev) { el.addEventListener(ev, v[ev]); }); }
        else if (k === "vars") { Object.keys(v).forEach(function (n) { el.style.setProperty(n, v[n]); }); }
        else { el.setAttribute(k, v === true ? "" : String(v)); }
      });
    }
    appendKids(el, Array.prototype.slice.call(arguments, 2));
    return el;
  }

  function appendKids(el, kids) {
    kids.forEach(function (c) {
      if (c === null || c === undefined || c === false) { return; }
      if (Array.isArray(c)) { appendKids(el, c); }
      else if (c instanceof Node) { el.appendChild(c); }
      else { el.appendChild(document.createTextNode(String(c))); }
    });
  }

  function put(parent, child) {
    if (child !== null && child !== undefined && child !== false) { parent.appendChild(child); }
    return parent;
  }

  function safeUrl(u) {
    if (typeof u !== "string" || !u) { return null; }
    try {
      var url = new URL(u, location.href);
      return (url.protocol === "https:" || url.protocol === "http:") ? url.href : null;
    } catch (e) {
      return null;
    }
  }

  function extLink(u, label, cls) {
    var href = safeUrl(u);
    if (!href) { return null; }
    return h("a", { href: href, class: cls || null, target: "_blank", rel: "noopener noreferrer" },
      label, h("span", { class: "sr-only", text: " (opens in a new tab)" }), icon("ext"));
  }

  function fkId(fk) { return "f-" + String(fk).replace(/[^A-Za-z0-9_-]/g, "_"); }

  function cssEsc(s) { return (window.CSS && CSS.escape) ? CSS.escape(s) : String(s).replace(/["\\]/g, "\\$&"); }

  function reducedMotion() {
    return window.matchMedia && window.matchMedia("(prefers-reduced-motion: reduce)").matches;
  }

  function rid() {
    var a = new Uint8Array(12);
    (window.crypto || window.msCrypto).getRandomValues(a);
    return "w" + Array.prototype.map.call(a, function (b) { return ("0" + b.toString(16)).slice(-2); }).join("");
  }

  function arr(x) { return Array.isArray(x) ? x : []; }
  function str(x) { return typeof x === "string" ? x : (x === null || x === undefined ? "" : String(x)); }

  // ------------------------------------------------------------------ light, safe artifact formatting

  var INLINE_RE = /(`[^`\n]+`)|(\*\*[^*\n]+\*\*)|(https?:\/\/[^\s<>()"'`]*[^\s<>()"'`.,;:!?*])/g;

  function inline(text) {
    var out = [];
    var s = str(text);
    var last = 0;
    var m;
    INLINE_RE.lastIndex = 0;
    while ((m = INLINE_RE.exec(s)) !== null) {
      if (m.index > last) { out.push(s.slice(last, m.index)); }
      if (m[1]) { out.push(h("code", { text: m[1].slice(1, -1) })); }
      else if (m[2]) { out.push(h("strong", { text: m[2].slice(2, -2) })); }
      else {
        var a = safeUrl(m[3]);
        out.push(a ? h("a", { href: a, target: "_blank", rel: "noopener noreferrer", text: m[3] }) : m[3]);
      }
      last = m.index + m[0].length;
    }
    if (last < s.length) { out.push(s.slice(last)); }
    return out;
  }

  function renderDoc(text) {
    var root = h("div", { class: "doc" });
    var body = str(text);
    var fm = /^---\r?\n([\s\S]*?)\r?\n---[ \t]*(?:\r?\n|$)/.exec(body);
    if (fm) {
      var dl = h("dl", { class: "doc-fm" });
      fm[1].split(/\r?\n/).forEach(function (line) {
        var kv = /^([A-Za-z0-9_-]+):\s*(.*)$/.exec(line);
        if (kv) {
          dl.appendChild(h("dt", { text: kv[1] }));
          dl.appendChild(h("dd", null, inline(kv[2] === ">" || kv[2] === "|" ? "" : kv[2])));
        } else if (line.trim() && dl.lastElementChild) {
          dl.lastElementChild.appendChild(document.createTextNode(" " + line.trim()));
        }
      });
      root.appendChild(dl);
      body = body.slice(fm[0].length);
    }
    var para = [];
    var code = null;
    function flush() {
      if (para.length) { root.appendChild(h("p", { class: "doc-p" }, inline(para.join("\n")))); }
      para = [];
    }
    body.split(/\r?\n/).forEach(function (line) {
      if (/^\s*```/.test(line)) {
        if (code) { root.appendChild(h("pre", { class: "doc-code", text: code.join("\n") })); code = null; }
        else { flush(); code = []; }
        return;
      }
      if (code) { code.push(line); return; }
      var hm = /^(#{1,6})\s+(.*)$/.exec(line);
      if (hm) {
        flush();
        var lvl = hm[1].length;
        root.appendChild(h("p", { class: "doc-h doc-h" + Math.min(lvl, 3), role: "heading", "aria-level": String(Math.min(lvl + 3, 6)) }, inline(hm[2])));
        return;
      }
      var li = /^\s*[-*+]\s+(.*)$/.exec(line);
      if (li) { flush(); root.appendChild(h("p", { class: "doc-li" }, inline(li[1]))); return; }
      if (!line.trim()) { flush(); return; }
      para.push(line.replace(/\s+$/, ""));
    });
    if (code) { root.appendChild(h("pre", { class: "doc-code", text: code.join("\n") })); }
    flush();
    return root;
  }

  // ------------------------------------------------------------------ time

  var S = {
    ventures: [], live: null, liveAt: 0, status: null, hq: null, hqAt: 0,
    commands: new Map(), bridge: null, skew: 0, lastServerTime: 0, loaded: false
  };

  function now() { return Date.now() + S.skew; }
  function tms(s) { var t = s ? Date.parse(s) : NaN; return isFinite(t) ? t : NaN; }
  function pad(n) { return (n < 10 ? "0" : "") + n; }

  function dur(sec) {
    sec = Math.max(0, Math.floor(sec));
    var hh = Math.floor(sec / 3600);
    var mm = Math.floor((sec % 3600) / 60);
    var ss = sec % 60;
    return hh ? hh + ":" + pad(mm) + ":" + pad(ss) : mm + ":" + pad(ss);
  }

  function ago(sec) {
    sec = Math.max(0, Math.round(sec));
    if (sec < 45) { return "just now"; }
    if (sec < 3600) { return Math.round(sec / 60) + "m ago"; }
    if (sec < 86400) { var hh = Math.floor(sec / 3600); var mm = Math.round((sec % 3600) / 60); return hh + "h" + (mm ? " " + mm + "m" : "") + " ago"; }
    return Math.round(sec / 86400) + "d ago";
  }

  function rel(ts) {
    var t = tms(ts);
    if (!isFinite(t)) { return "—"; }
    var d = (now() - t) / 1000;
    if (d < 0) { return d > -45 ? "just now" : "in " + ago(-d).replace(" ago", ""); }
    if (d > 7 * 86400) { return new Date(t).toLocaleDateString(undefined, { month: "short", day: "numeric" }); }
    return ago(d);
  }

  function absTime(ts) {
    var t = tms(ts);
    return isFinite(t) ? new Date(t).toLocaleString() : "";
  }

  function timeEl(ts, cls) {
    return h("time", { class: cls || "feed-time", datetime: str(ts), title: absTime(ts), "data-rel": str(ts), text: rel(ts) });
  }

  // ------------------------------------------------------------------ derived state

  function doc() { return S.hq && S.hq.doc && typeof S.hq.doc === "object" ? S.hq.doc : null; }
  function hqAge() { return S.hq ? (Number(S.hq.age_seconds) || 0) + (Date.now() - S.hqAt) / 1000 : Infinity; }
  function liveAge() { return S.live ? (Number(S.live.age_seconds) || 0) + (Date.now() - S.liveAt) / 1000 : Infinity; }
  function liveStale() { return liveAge() > LIVE_STALE_S; }
  function runs() {
    return arr(S.live && S.live.runs).filter(function (r) { return r && r.role; })
      .sort(function (a, b) { return (tms(a.started_at) || 0) - (tms(b.started_at) || 0); });
  }
  function runsFor(slug) { return runs().filter(function (r) { return r.slug === slug; }); }
  function runById(id) { return runs().filter(function (r) { return r.run_id === id; })[0] || null; }
  function bridgeOnline() { return !S.bridge || S.bridge.online !== false; }

  function tickInfo() {
    var d = doc();
    var t = (d && d.tick) || {};
    var docRunning = !!(d && t.running && hqAge() < 900);
    var liveRunning = !!(S.live && S.live.tick_running && liveAge() < 600);
    var running = docRunning || liveRunning;
    var started = running ? (t.started_at || null) : null;
    if (running && !started && runs().length) { started = runs()[0].started_at; }
    var cycle = Math.max(Number(d && d.cycle) || 0, Number(S.live && S.live.cycle) || 0) || null;
    return {
      running: running, started: started, cycle: cycle,
      next: t.next || (S.status && S.status.next_tick) || null,
      last: t.last || (S.status && S.status.last_tick) || null
    };
  }

  function pubBySlug(slug) {
    for (var i = 0; i < S.ventures.length; i++) { if (S.ventures[i] && S.ventures[i].slug === slug) { return S.ventures[i]; } }
    return null;
  }

  function hqBySlug(slug) {
    var d = doc();
    return arr(d && d.ventures).filter(function (v) { return v && v.slug === slug; })[0] || null;
  }

  function fromPublic(p) {
    return {
      slug: p.slug, title: p.title, stage: p.stage, kind: p.kind, field: p.field, ticks: null, max_ticks: null,
      gate: null, blocked: false, repo_url: p.repo_url, url: p.url, design_url: p.design_url,
      artifacts: [], notes: [], events: [], focus: null, fromPublic: true
    };
  }

  // A killed or archived venture can keep a stale gate/blocked flag on disk; it
  // no longer asks the founder anything.
  var TERMINAL_STAGES = ["KILLED", "ARCHIVED"];
  function needsFounder(v) {
    return !!(v && (v.gate || v.blocked) && TERMINAL_STAGES.indexOf(v.stage) < 0);
  }

  function activeVentures() {
    var d = doc();
    if (d && Array.isArray(d.ventures)) { return d.ventures.filter(function (v) { return v && v.slug; }); }
    return S.ventures.filter(function (p) { return p && p.status === "in_progress"; }).map(fromPublic);
  }

  function titleOf(slug) {
    var v = hqBySlug(slug) || pubBySlug(slug);
    return (v && v.title) || slug || "company";
  }

  function crewRoles() {
    var d = doc();
    var list = arr(d && d.roles).filter(function (r) { return r && r.role; });
    if (!list.length) { list = FALLBACK_ROLES.map(function (r) { return { role: r }; }); }
    var seen = dict();
    list.forEach(function (r) { seen[r.role] = true; });
    runs().forEach(function (r) {
      if (!seen[r.role]) { seen[r.role] = true; list.push({ role: r.role }); }
    });
    return list;
  }

  function knownRole(name) {
    if (!name) { return false; }
    if (roleKey(name) !== "unknown") { return true; }
    return crewRoles().some(function (r) { return r.role === name; });
  }

  function commandsSorted() {
    return Array.from(S.commands.values()).sort(function (a, b) {
      return (tms(b.created_at) || 0) - (tms(a.created_at) || 0);
    });
  }

  function isFinal(c) { return c && (c.status === "done" || c.status === "failed"); }

  function pendingFor(slug, kinds) {
    return commandsSorted().filter(function (c) {
      return !isFinal(c) && (slug === null || c.slug === slug) &&
        (!kinds || Object.prototype.hasOwnProperty.call(kinds, c.kind));
    });
  }

  function cmdLabel(kind) { return CMD_LABEL[kind] || str(kind); }

  // Status chip: the class comes from a fixed set, never from the raw server string.
  function statusChip(status, text) {
    var s = str(status);
    return h("span", { class: "chip" + (CMD_STATUSES[s] ? " chip-status-" + s : ""), text: text === undefined ? s : text });
  }

  function shortModel(m) {
    var s = str(m);
    var i = s.lastIndexOf("/");
    return i >= 0 ? s.slice(i + 1) : s;
  }

  // ------------------------------------------------------------------ UI state

  var ui = {
    conn: "connecting", pollOk: null, authLost: false,
    drafts: {}, modes: {}, errors: {}, busy: {}, open: {}, sigs: {}, cids: {},
    drawer: null, drawerHistory: [], returnFk: null, logStick: true, lastRun: null, log: null,
    feedFilter: "all", feedSeen: null, cardStage: null, archiveQuery: "", archiveSlugs: null, celebrated: {}
  };

  // Replace a container's content only when its data signature changed, keeping
  // keyboard focus, caret and inner scroll positions across the swap.
  function patch(container, key, sig, build) {
    if (!container || ui.sigs[key] === sig) { return false; }
    var ae = document.activeElement;
    var hadFocus = !!(ae && ae !== document.body && container.contains(ae));
    var fk = null;
    var sel = null;
    if (hadFocus) {
      fk = ae.getAttribute("data-fk");
      if (fk && typeof ae.selectionStart === "number") {
        try { sel = [ae.selectionStart, ae.selectionEnd]; } catch (e) { sel = null; }
      }
    }
    var scrolls = Array.prototype.map.call(container.querySelectorAll("[data-keep-scroll]"), function (el) {
      return [el.getAttribute("data-keep-scroll"), el.scrollTop];
    });
    // Build first: if it throws, the old content and signature stay, and the
    // next render retries instead of skipping a half-applied update.
    var frag = build();
    container.replaceChildren(frag);
    ui.sigs[key] = sig;
    scrolls.forEach(function (s) {
      var el = container.querySelector('[data-keep-scroll="' + cssEsc(s[0]) + '"]');
      if (el) { el.scrollTop = s[1]; }
    });
    if (hadFocus) {
      var target = fk ? container.querySelector('[data-fk="' + cssEsc(fk) + '"]') : null;
      if (target && !target.disabled) {
        target.focus({ preventScroll: true });
        if (sel && target.setSelectionRange) { try { target.setSelectionRange(sel[0], sel[1]); } catch (e) { /* not a text field */ } }
      } else {
        // The focused control is gone or now disabled (e.g. a command is in
        // flight): keep keyboard focus in this region rather than on <body>.
        if (!container.hasAttribute("tabindex")) { container.setAttribute("tabindex", "-1"); }
        container.focus({ preventScroll: true });
      }
    }
    return true;
  }

  function detailsKeep(el, key, defaultOpen) {
    el.setAttribute("data-fk", key);
    var open = Object.prototype.hasOwnProperty.call(ui.open, key) ? ui.open[key] : !!defaultOpen;
    if (open) { el.open = true; }
    el.addEventListener("toggle", function () { ui.open[key] = el.open; });
    return el;
  }

  function announce(msg) {
    var r = $("alerts");
    if (!r) { return; }
    r.textContent = "";
    window.setTimeout(function () { r.textContent = msg; }, 60);
  }

  // ------------------------------------------------------------------ toasts & celebration

  function toast(tone, title, msg, ttl) {
    var host = $("toasts");
    if (!host) { return null; }
    var close = h("button", { type: "button", class: "icon-btn", "aria-label": "Dismiss" }, icon("x"));
    var el = h("div", { class: "toast is-" + tone },
      icon(tone === "ok" ? "check" : tone === "bad" ? "alert" : "send"),
      h("div", null, h("p", { class: "toast-title", text: title }), msg ? h("p", { class: "toast-msg", text: msg }) : null),
      close);
    function dismiss() {
      if (!el.isConnected) { return; }
      el.classList.add("is-leaving");
      window.setTimeout(function () { el.remove(); }, 260);
    }
    close.addEventListener("click", dismiss);
    host.appendChild(el);
    while (host.children.length > 4) { host.firstElementChild.remove(); }
    window.setTimeout(dismiss, ttl || (tone === "bad" ? 14000 : 7000));
    return el;
  }

  var CONFETTI_ROLES = ["inventor", "designer", "developer", "architect", "reviewer", "sales", "pm", "screener"];
  var CONFETTI_SHAPES = ["c-round", "c-pill", "c-diamond", ""];

  function burst(origin) {
    if (reducedMotion()) { return; }
    var layer = $("burst-layer");
    if (!layer) { return; }
    var r = origin && origin.getBoundingClientRect ? origin.getBoundingClientRect() : null;
    var cx = r ? r.left + Math.min(r.width, 60) : window.innerWidth / 2;
    var cy = r ? r.top + r.height / 2 : window.innerHeight / 3;
    for (var i = 0; i < 26; i++) {
      var el = h("span", { class: "confetti role-" + CONFETTI_ROLES[i % CONFETTI_ROLES.length] + " " + CONFETTI_SHAPES[i % 4] });
      el.style.setProperty("left", cx + "px");
      el.style.setProperty("top", cy + "px");
      layer.appendChild(el);
      var ang = Math.random() * Math.PI * 2;
      var dist = 70 + Math.random() * 150;
      var dx = Math.cos(ang) * dist;
      var dy = Math.sin(ang) * dist - 70;
      var rot = (Math.random() * 540 - 270) | 0;
      var anim = el.animate([
        { transform: "translate(-50%, -50%) scale(.3) rotate(0deg)", opacity: 1 },
        { transform: "translate(calc(-50% + " + dx + "px), calc(-50% + " + dy + "px)) scale(1) rotate(" + rot + "deg)", opacity: 1, offset: .65 },
        { transform: "translate(calc(-50% + " + dx * 1.1 + "px), calc(-50% + " + (dy + 90) + "px)) scale(.8) rotate(" + rot * 1.4 + "deg)", opacity: 0 }
      ], { duration: 1000 + Math.random() * 500, easing: "cubic-bezier(.2,.8,.2,1)" });
      anim.onfinish = (function (node) { return function () { node.remove(); }; })(el);
    }
  }

  function cmdTitle(c) {
    var label = cmdLabel(c.kind);
    if (c.kind === "tick" || c.kind === "suggest") { return label; }
    return label + " · " + titleOf(c.slug);
  }

  function onCommandFinished(c) {
    var ok = c.status === "done";
    var msg = str(c.message) || (ok ? "The laptop applied it." : "The laptop could not apply it.");
    var el = toast(ok ? "ok" : "bad", cmdTitle(c) + (ok ? " — done" : " — failed"), msg);
    if (ok && c.kind === "approve" && !ui.celebrated[c.id]) {
      ui.celebrated[c.id] = true;
      burst(el);
    }
  }

  function upsertCommand(c, quiet) {
    if (!c || typeof c !== "object" || !c.id) { return; }
    var prev = S.commands.get(c.id);
    // A finished command is final: a late redelivery ("delivered") must not revive it.
    if (prev && isFinal(prev) && !isFinal(c)) { return; }
    S.commands.set(c.id, Object.assign({}, prev || {}, c));
    if (!quiet && prev && !isFinal(prev) && isFinal(c)) { onCommandFinished(c); }
    if (S.commands.size > MAX_COMMANDS + 50) { pruneCommands(); }
  }

  // Keep memory flat on a page left open for days: drop the oldest finished
  // commands beyond MAX_COMMANDS (the server only ever resends the last 50).
  var MAX_COMMANDS = 200;
  function pruneCommands() {
    commandsSorted().filter(isFinal).slice(MAX_COMMANDS).forEach(function (c) {
      S.commands.delete(c.id);
      delete ui.celebrated[c.id];
    });
  }

  // ------------------------------------------------------------------ applying server messages

  function setLive(live) {
    if (!live || typeof live !== "object") { return; }
    S.live = live;
    S.liveAt = Date.now();
  }

  function setHq(hq) {
    if (hq === null) { S.hq = null; return; }
    if (!hq || typeof hq !== "object") { return; }
    S.hq = hq;
    S.hqAt = Date.now();
  }

  function applyState(m) {
    var st = tms(m.server_time);
    if (isFinite(st)) {
      if (st < S.lastServerTime) { return; } // an older poll answered after a newer push
      S.lastServerTime = st;
      S.skew = st - Date.now();
    }
    S.ventures = arr(m.ventures).filter(function (v) { return v && v.slug; });
    setLive(m.live);
    S.status = m.status && typeof m.status === "object" ? m.status : S.status;
    if ("hq" in m) { setHq(m.hq); }
    arr(m.commands).slice().reverse().forEach(function (c) { upsertCommand(c, !S.loaded); });
    if (m.bridge && typeof m.bridge === "object") { S.bridge = m.bridge; }
    S.loaded = true;
  }

  function handleMessage(m) {
    if (!m || typeof m !== "object") { return; }
    try { applyMessage(m); } catch (e) { reportError("message " + str(m.type), e); }
    scheduleRender();
  }

  function applyMessage(m) {
    switch (m.type) {
      case "state": applyState(m); break;
      case "live": setLive(m.live); break;
      case "ventures":
        S.ventures = arr(m.ventures).filter(function (v) { return v && v.slug; });
        if (m.status && typeof m.status === "object") { S.status = m.status; }
        break;
      case "hq": setHq(m.hq); break;
      case "command": upsertCommand(m.command, false); break;
      case "bridge": if (m.bridge && typeof m.bridge === "object") { S.bridge = m.bridge; } break;
      default: break;
    }
  }

  // ------------------------------------------------------------------ transport

  var ws = null;
  var backoff = 1000;
  var reconnectTimer = null;
  var pollTimer = null;
  var lastMsgAt = 0;
  var hiddenAt = 0;
  var WS_SESSION_ENDED = 4001; // server: the session was revoked or expired
  // While the laptop is online it pushes an HQ heartbeat at least every 60 s.
  // A socket that stays silent much longer than that is dead (sleep, NAT drop).
  var WS_SILENT_MS = 150000;

  function wsUrl() {
    return (location.protocol === "https:" ? "wss://" : "ws://") + location.host + "/hq/ws";
  }

  function connect() {
    if (ws || ui.authLost) { return; }
    window.clearTimeout(reconnectTimer);
    reconnectTimer = null;
    var sock;
    try { sock = new WebSocket(wsUrl()); } catch (e) { onSocketDown(); return; }
    ws = sock;
    var gotState = false;
    sock.onopen = function () { lastMsgAt = Date.now(); };
    sock.onmessage = function (ev) {
      if (ws !== sock) { return; }
      lastMsgAt = Date.now();
      var m;
      try { m = JSON.parse(ev.data); } catch (e) { return; }
      if (!gotState && m && m.type === "state") {
        // Only a delivered state proves the link works. Resetting the backoff
        // on "open" would hammer a server that accepts and then closes.
        gotState = true;
        backoff = 1000;
        ui.conn = "live";
        stopPolling();
      }
      handleMessage(m);
    };
    sock.onclose = function (ev) {
      if (ws !== sock) { return; }
      ws = null;
      if (ev && ev.code === WS_SESSION_ENDED) { authLost(); return; }
      onSocketDown();
    };
    sock.onerror = function () { /* onclose follows */ };
  }

  // Abandon a socket that looks dead without waiting for the browser to notice.
  function dropSocket() {
    var sock = ws;
    if (!sock) { return; }
    ws = null;
    sock.onopen = sock.onmessage = sock.onclose = sock.onerror = null;
    try { sock.close(); } catch (e) { /* already closing */ }
    backoff = 1000;
    onSocketDown();
  }

  function watchSocket() {
    if (ws && ws.readyState === 1 && S.bridge && S.bridge.online && Date.now() - lastMsgAt > WS_SILENT_MS) {
      dropSocket();
    }
  }

  function onSocketDown() {
    ui.conn = (navigator.onLine === false || ui.pollOk === false) ? "offline" : "reconnecting";
    startPolling();
    scheduleReconnect();
    scheduleRender();
  }

  function scheduleReconnect() {
    if (reconnectTimer || ui.authLost) { return; }
    var wait = backoff * (0.8 + Math.random() * 0.4);
    backoff = Math.min(backoff * 2, 30000);
    reconnectTimer = window.setTimeout(function () { reconnectTimer = null; connect(); }, wait);
  }

  function startPolling() {
    if (pollTimer) { return; }
    pollOnce();
    pollTimer = window.setInterval(pollOnce, POLL_MS);
  }

  function stopPolling() {
    window.clearInterval(pollTimer);
    pollTimer = null;
  }

  function pollOnce() {
    if (ui.authLost) { return Promise.resolve(); }
    return fetch("/hq/api/state", { credentials: "same-origin", cache: "no-store", headers: { Accept: "application/json" } })
      .then(function (res) {
        if (res.status === 401) { authLost(); throw new Error("unauthorized"); }
        if (!res.ok) { throw new Error("state " + res.status); }
        return res.json();
      })
      .then(function (m) {
        ui.pollOk = true;
        if (ui.conn !== "live") { ui.conn = "reconnecting"; }
        m.type = "state";
        handleMessage(m);
      })
      .catch(function () {
        ui.pollOk = false;
        if (ui.conn !== "live") { ui.conn = "offline"; }
        scheduleRender();
      });
  }

  function hasDrafts() {
    return Object.keys(ui.drafts).some(function (k) { return str(ui.drafts[k]).trim() !== ""; });
  }

  // The session is gone. Reload into the login screen, unless that would throw
  // away text the founder is typing, or we reloaded moments ago (loop guard).
  function authLost() {
    if (ui.authLost) { return; }
    var key = "hq-auth-reload";
    var last = 0;
    try { last = Number(sessionStorage.getItem(key)) || 0; } catch (e) { last = 0; }
    if (Date.now() - last > 20000 && !hasDrafts()) {
      try { sessionStorage.setItem(key, String(Date.now())); } catch (e) { /* private mode */ }
      location.reload();
      return;
    }
    ui.authLost = true;
    stopPolling();
    window.clearTimeout(reconnectTimer);
    reconnectTimer = null;
    var sock = ws;
    ws = null;
    if (sock) {
      sock.onclose = sock.onmessage = sock.onerror = null;
      try { sock.close(); } catch (e) { /* ignore */ }
    }
    scheduleRender();
  }

  function sendCommand(formKey, payload, onOk) {
    if (ui.busy[formKey] || ui.authLost) { return; }
    ui.busy[formKey] = true;
    delete ui.errors[formKey];
    renderAll();
    var body = {
      kind: payload.kind, slug: payload.slug || "", gate: payload.gate || "", text: payload.text || ""
    };
    // Retrying the same command after a lost response reuses its client_id, so
    // the server returns the command it may already have queued instead of
    // queueing a duplicate. A new or edited command gets a fresh id.
    var fp = JSON.stringify([body.kind, body.slug, body.gate, body.text]);
    var prev = ui.cids[formKey];
    body.client_id = prev && prev.fp === fp ? prev.id : rid();
    ui.cids[formKey] = { fp: fp, id: body.client_id };
    fetch("/hq/api/commands", {
      method: "POST",
      credentials: "same-origin",
      headers: { "Content-Type": "application/json", Accept: "application/json" },
      body: JSON.stringify(body)
    }).then(function (res) {
      if (res.status === 401) {
        delete ui.cids[formKey];
        ui.errors[formKey] = "Your session ended. Copy your text, then open a fresh link from /web.";
        announce(ui.errors[formKey]);
        authLost();
        return null;
      }
      return res.json().catch(function () { return null; }).then(function (data) {
        if (res.status === 202 && data && data.command) {
          delete ui.cids[formKey];
          upsertCommand(data.command, true);
          if (onOk) { onOk(data.command); }
          if (!bridgeOnline()) {
            toast("info", cmdTitle(data.command) + " — queued", "The laptop is offline. This runs as soon as it reconnects.");
          } else {
            announce(cmdTitle(data.command) + " queued.");
          }
        } else {
          // A 4xx is a definitive "no": the next attempt is a new command. A
          // 5xx may have queued it, so a retry keeps the same client_id.
          if (res.status < 500) { delete ui.cids[formKey]; }
          ui.errors[formKey] = (data && typeof data.error === "string" && data.error) ? data.error : "HQ answered " + res.status + ". Try again.";
          announce(ui.errors[formKey]);
        }
      });
    }).catch(function () {
      ui.errors[formKey] = "Could not reach HQ. Check your connection and try again.";
      announce(ui.errors[formKey]);
    }).then(function () {
      ui.busy[formKey] = false;
      renderAll();
    });
  }

  // ------------------------------------------------------------------ render scheduling

  var renderQueued = false;
  var renderDeferred = false; // an update arrived while the tab was hidden
  // rAF batches renders with paint; the timer covers views that are visible but
  // never painted (embedded previews), where rAF would hold every update back.
  function scheduleRender() {
    if (renderQueued) { return; }
    renderQueued = true;
    var run = function () {
      if (!renderQueued) { return; }
      renderQueued = false;
      renderAll();
    };
    window.requestAnimationFrame(run);
    window.setTimeout(run, 250);
  }

  // One failing renderer (odd data, a bug) must not take the others down, and
  // the page keeps updating while it logs each distinct error once.
  var reported = new Set();
  function reportError(where, e) {
    var msg = where + ": " + (e && e.message ? e.message : String(e));
    if (reported.has(msg) || reported.size > 50) { return; }
    reported.add(msg);
    if (window.console && console.error) { console.error("HQ " + msg, e); }
  }

  function guarded(where, fn) {
    try { fn(); } catch (e) { reportError(where, e); }
  }

  function renderAll() {
    if (document.hidden) { renderDeferred = true; return; }
    renderDeferred = false;
    guarded("top", renderTop);
    guarded("banners", renderBanners);
    guarded("inbox", renderInbox);
    guarded("crew", renderCrew);
    guarded("pipeline", renderPipeline);
    guarded("activity", renderActivity);
    guarded("ideas", renderIdeas);
    guarded("archive", renderArchive);
    guarded("drawer", renderDrawer);
  }

  // ------------------------------------------------------------------ top bar

  function renderTop() {
    var conn = $("conn-pill");
    var state = ui.authLost ? "offline" : ui.conn;
    conn.setAttribute("data-state", state);
    conn.querySelector(".pill-label").textContent =
      ui.authLost ? "Signed out" :
      state === "live" ? "Live" :
      state === "reconnecting" ? (ui.pollOk ? "Reconnecting · polling" : "Reconnecting…") :
      state === "offline" ? "Offline" : "Connecting…";

    var bridge = $("bridge-pill");
    if (!S.bridge) {
      bridge.setAttribute("data-state", "unknown");
      bridge.querySelector(".pill-label").textContent = "Laptop: unknown";
    } else if (S.bridge.online) {
      bridge.setAttribute("data-state", "online");
      bridge.querySelector(".pill-label").textContent = "Laptop online";
    } else {
      bridge.setAttribute("data-state", "offline");
      var seen = tms(S.bridge.last_seen);
      bridge.querySelector(".pill-label").textContent = "Laptop offline" + (isFinite(seen) ? " · seen " + rel(S.bridge.last_seen) : "");
    }

    var t = tickInfo();
    var stateEl = $("tick-state");
    var nextEl = $("tick-next");
    stateEl.setAttribute("data-running", t.running ? "true" : "false");
    var cyc = t.cycle ? "Cycle " + t.cycle + " · " : "";
    if (!S.loaded) {
      stateEl.textContent = "Tick: waiting for data";
    } else if (t.running) {
      var st = tms(t.started);
      stateEl.textContent = cyc + "Tick running" + (isFinite(st) ? " " + dur((now() - st) / 1000) : "");
    } else {
      stateEl.textContent = cyc + "Idle";
    }
    var nx = tms(t.next);
    if (!isFinite(nx)) {
      nextEl.textContent = S.loaded ? "Next tick: not scheduled" : "";
    } else if (nx - now() > 0) {
      nextEl.textContent = "Next tick in " + dur((nx - now()) / 1000);
    } else {
      nextEl.textContent = "Next tick due now";
    }

    var btn = $("tick-btn");
    var pendingTick = pendingFor(null, { tick: 1 })[0];
    var label = t.running ? "Tick running" : pendingTick ? (pendingTick.status === "queued" ? "Tick queued…" : "Starting tick…") : ui.busy.tick ? "Sending…" : "Run a tick now";
    btn.disabled = !S.loaded || t.running || !!pendingTick || !!ui.busy.tick || ui.authLost;
    btn.setAttribute("data-running", t.running ? "true" : "false");
    btn.title = t.running ? "A tick is already running" : !bridgeOnline() ? "The laptop is offline: the tick starts when it reconnects" : "";
    $("tick-btn-label").textContent = label;

    var brand = $("brand-sub");
    if (brand) {
      var working = runs().length;
      brand.textContent = working ? working + " on shift" : "night shift";
    }
  }

  // ------------------------------------------------------------------ banners

  function renderBanners() {
    // [id, tone, icon, headline, detail, spoken]: "spoken" is announced once
    // when the banner first appears; the visible text may tick (ages) without
    // re-announcing every minute.
    var list = [];
    if (ui.authLost) {
      list.push(["auth", "bad", "alert", "Your session ended.", " Reload the page, or send /web to the Telegram bot for a new link.", "Your session ended."]);
    }
    if (S.loaded && ui.conn === "offline" && !ui.authLost) {
      list.push(["conn", "bad", "alert", "Can't reach HQ.", " Retrying in the background; what you see may be out of date.", "Can't reach HQ. Retrying."]);
    }
    if (S.loaded && S.bridge && S.bridge.online === false) {
      list.push(["bridge", "bad", "laptop", "The laptop is offline" + (S.bridge.last_seen ? " (last seen " + rel(S.bridge.last_seen) + ")" : "") + ".",
        " You can still act: commands wait in the queue and run when it's back.", "The laptop is offline."]);
    }
    if (S.loaded && !S.hq) {
      list.push(["nohq", "info", "laptop", "Waiting for the laptop's first HQ update.", " Gates, crew details and activity appear once it arrives. Public data is shown meanwhile.", ""]);
    } else if (S.loaded && hqAge() > HQ_STALE_S) {
      list.push(["stale", "warn", "alert", "HQ data is " + ago(hqAge()).replace(" ago", "") + " old.", " The laptop may be asleep; stages and gates may have moved since.", "HQ data is out of date."]);
    }
    var t = tickInfo();
    if (S.loaded && t.running && S.live && liveAge() > 120) {
      list.push(["quiet", "warn", "pulse", "No live signal for " + ago(liveAge()).replace(" ago", "") + ".", " A tick is marked running but the live feed went quiet.", "The live feed went quiet."]);
    }
    var ids = {};
    list.forEach(function (b) {
      ids[b[0]] = true;
      if (b[5] && ui.banners && !ui.banners[b[0]]) { announce(b[5]); }
    });
    ui.banners = S.loaded ? ids : null;
    var sig = JSON.stringify(list);
    patch($("banners"), "banners", sig, function () {
      var f = document.createDocumentFragment();
      list.forEach(function (b) {
        f.appendChild(h("div", { class: "banner banner-" + b[1] }, icon(b[2]), h("p", null, h("strong", { text: b[3] }), b[4])));
      });
      return f;
    });
  }

  // ------------------------------------------------------------------ forms

  function textField(o) {
    var id = fkId(o.fk);
    var errId = id + "-err";
    var ta = h("textarea", {
      class: "textarea", id: id, "data-fk": o.fk, maxlength: String(o.max), rows: o.rows || 3,
      placeholder: o.placeholder || null, "aria-describedby": (o.error ? errId + " " : "") + id + "-count",
      "aria-invalid": o.error ? "true" : null
    });
    ta.value = ui.drafts[o.draft] || "";
    var counter = h("span", { class: "counter", id: id + "-count", "aria-live": "off" });
    function count() {
      var n = ta.value.length;
      counter.textContent = n + " / " + o.max;
      counter.classList.toggle("is-over", n >= o.max);
    }
    count();
    ta.addEventListener("input", function () { ui.drafts[o.draft] = ta.value; count(); });
    ta.addEventListener("keydown", function (e) {
      if (e.key === "Enter" && (e.metaKey || e.ctrlKey)) { e.preventDefault(); o.submit(); }
      if (e.key === "Escape" && o.cancel) { e.preventDefault(); e.stopPropagation(); o.cancel(); }
    });
    return [
      h("label", { for: id, text: o.label }),
      ta,
      o.error ? h("p", { class: "form-error", id: errId, text: o.error }) : null,
      h("div", { class: "form-row" }, o.buttons, counter)
    ];
  }

  function offlineHint() {
    if (bridgeOnline()) { return null; }
    return h("p", { class: "offline-hint" }, icon("laptop"), "Laptop offline — this will run when it's back.");
  }

  function focusSoon(fk) {
    window.requestAnimationFrame(function () {
      var el = document.querySelector('[data-fk="' + cssEsc(fk) + '"]');
      if (el) { el.focus(); }
    });
  }

  function setMode(scope, mode, focusFk) {
    if (mode) { ui.modes[scope] = mode; } else { delete ui.modes[scope]; }
    delete ui.errors[scope];
    renderAll();
    if (focusFk) { focusSoon(focusFk); }
  }

  // Gate / blocked actions for one venture. ctx separates inbox and drawer copies.
  function ventureActions(v, ctx) {
    var scope = ctx + ":" + v.slug;
    var mode = ui.modes[scope] || null;
    var inflight = pendingFor(v.slug, ACTION_KINDS);
    var busy = !!ui.busy[scope];
    var locked = inflight.length > 0 || busy || ui.authLost;
    var zone = h("div", { class: "action-zone" });
    var draftKey = scope + ":" + (mode === "reject-confirm" ? "reject" : mode);
    var text = str(ui.drafts[draftKey]).trim();

    function send(kind, extra) {
      sendCommand(scope, Object.assign({ kind: kind, slug: v.slug, gate: v.gate || "" }, extra || {}), function () {
        delete ui.drafts[draftKey];
        delete ui.modes[scope];
      });
    }
    function need(then) {
      if (!str(ui.drafts[draftKey]).trim()) {
        ui.errors[scope] = "Write a few words first.";
        announce(ui.errors[scope]);
        renderAll();
        focusSoon(scope + ":ta");
        return;
      }
      then();
    }
    var cancel = function () { setMode(scope, null, scope + ":btn-" + (mode === "reject-confirm" ? "reject" : mode)); };

    if (!mode) {
      var row = h("div", { class: "actions" });
      if (v.gate) {
        row.appendChild(h("button", { type: "button", class: "btn btn-approve", "data-fk": scope + ":btn-approve", disabled: locked,
          on: { click: function () { send("approve"); } } }, icon("check"), "Approve"));
        row.appendChild(h("button", { type: "button", class: "btn btn-quiet", "data-fk": scope + ":btn-changes", disabled: locked, "aria-expanded": "false",
          on: { click: function () { setMode(scope, "changes", scope + ":ta"); } } }, "Request changes"));
        row.appendChild(h("button", { type: "button", class: "btn btn-quiet", "data-fk": scope + ":btn-reject", disabled: locked, "aria-expanded": "false",
          on: { click: function () { setMode(scope, "reject", scope + ":ta"); } } }, icon("ban"), "Reject"));
      }
      if (v.blocked) {
        row.appendChild(h("button", { type: "button", class: "btn btn-primary", "data-fk": scope + ":btn-answer", disabled: locked, "aria-expanded": "false",
          on: { click: function () { setMode(scope, "answer", scope + ":ta"); } } }, "Answer & resume"));
        row.appendChild(h("button", { type: "button", class: "btn btn-quiet", "data-fk": scope + ":btn-resume", disabled: locked,
          on: { click: function () { send("resume"); } } }, icon("resume"), "Resume"));
      }
      put(zone, row);
      if (!inflight.length) { put(zone, offlineHint()); }
    } else if (mode === "reject-confirm") {
      put(zone, h("div", { class: "confirm-box", role: "group", "aria-label": "Confirm rejection" },
        h("p", { text: "Reject “" + (v.title || v.slug) + "”? The venture stops here. HQ can't undo this." }),
        h("div", { class: "form-row" },
          h("button", { type: "button", class: "btn btn-danger", "data-fk": scope + ":confirm", disabled: locked,
            on: { click: function () { send("reject", { text: text }); } } }, busy ? "Sending…" : "Yes, reject it"),
          h("button", { type: "button", class: "btn btn-ghost", "data-fk": scope + ":back",
            on: { click: function () { setMode(scope, "reject", scope + ":ta"); } } }, "Back")),
        ui.errors[scope] ? h("p", { class: "form-error", text: ui.errors[scope] }) : null,
        offlineHint()));
    } else {
      var spec = {
        changes: { label: "What should change?", btn: "Send changes", cls: "btn-primary", kind: "changes" },
        reject: { label: "Why reject it? This goes into the venture's notes.", btn: "Continue", cls: "btn-danger", kind: "reject" },
        answer: { label: "Your answer to the crew", btn: "Answer & resume", cls: "btn-primary", kind: "answer" }
      }[mode];
      if (spec) {
        var submit = function () {
          need(function () {
            if (mode === "reject") { setMode(scope, "reject-confirm", scope + ":confirm"); }
            else { send(spec.kind, { text: str(ui.drafts[draftKey]).trim() }); }
          });
        };
        put(zone, h("div", { class: "action-form" }, textField({
          fk: scope + ":ta", draft: draftKey, label: spec.label, max: TEXT_LIMIT[spec.kind], error: ui.errors[scope],
          submit: submit, cancel: cancel,
          buttons: [
            h("button", { type: "button", class: "btn btn-sm " + spec.cls, "data-fk": scope + ":submit", disabled: locked, on: { click: submit } }, busy ? "Sending…" : spec.btn),
            h("button", { type: "button", class: "btn btn-sm btn-ghost", on: { click: cancel } }, "Cancel")
          ]
        }), offlineHint()));
      }
    }

    if (!mode && ui.errors[scope]) { put(zone, h("p", { class: "form-error", text: ui.errors[scope] })); }
    if (inflight.length) {
      var c = inflight[0];
      put(zone, h("p", { class: "cmd-status" }, h("span", { class: "spinner", "aria-hidden": "true" }),
        h("strong", { text: cmdLabel(c.kind) }),
        c.status === "queued" ? " queued — waiting for the laptop" : " delivered — the laptop is on it"));
      if (!mode) { var hint = offlineHint(); if (hint) { put(zone, hint); } }
    } else {
      var last = lastResult(v.slug);
      if (last) {
        put(zone, h("p", { class: "cmd-status" },
          statusChip(last.status, cmdLabel(last.kind) + " · " + str(last.status)),
          str(last.message)));
      }
    }
    return zone;
  }

  // The laptop's answer to the latest finished action on this venture, shown
  // until an HQ update generated after it arrives (which reflects the result).
  function lastResult(slug) {
    var d = doc();
    var docAt = tms(d && d.generated_at) || 0;
    var last = commandsSorted().filter(function (x) { return x.slug === slug && ACTION_KINDS[x.kind] && isFinal(x); })[0];
    return last && (tms(last.finished_at) || 0) >= docAt - 5000 ? last : null;
  }

  function actionsSig(slug, ctx) {
    var scope = ctx + ":" + slug;
    var last = lastResult(slug);
    return [ui.modes[scope] || "", ui.errors[scope] || "", !!ui.busy[scope], last ? last.id : "",
      commandsSorted().filter(function (c) { return c.slug === slug; }).map(function (c) { return c.id + c.status; }).join(",")];
  }

  // ------------------------------------------------------------------ inbox

  function inboxItems() {
    return activeVentures().filter(needsFounder)
      .sort(function (a, b) {
        var ga = a.gate ? 0 : 1;
        var gb = b.gate ? 0 : 1;
        if (ga !== gb) { return ga - gb; }
        return (Number(b.priority) || 0) - (Number(a.priority) || 0);
      });
  }

  function stageLabel(id) {
    var s = STAGE_BY_ID[id];
    return s ? s.label : str(id).replace(/_/g, " ").toLowerCase();
  }

  var KIND_CHIP = dict({ founder: "chip-kind-founder", internal: "chip-kind-internal", product: "chip-kind-product" });

  function kindChip(kind) {
    if (!kind) { return null; }
    var k = str(kind);
    var cls = KIND_CHIP[k] || "";
    return h("span", { class: "chip " + cls, text: k });
  }

  function focusArtifact(v, key, open) {
    if (!v.focus || !v.focus.text) { return null; }
    var det = h("details", { class: "artifact" },
      h("summary", null, icon("note"), h("span", null, "Read ", h("span", { class: "mono", text: str(v.focus.name) || "artifact" })), h("span", { class: "chev", "aria-hidden": "true" })));
    detailsKeep(det, key, open);
    det.appendChild(renderDoc(v.focus.text));
    return det;
  }

  function linkRow(v, opts) {
    opts = opts || {};
    var links = [];
    if (v.gate === "design" || opts.design) {
      var dl = extLink(v.design_url, "Open design canvas", "link-design");
      if (dl) { links.push(dl); }
    }
    if (v.block_url) { links.push(extLink(v.block_url, "Open issue")); }
    if (v.current_pr && v.current_pr.url) { links.push(extLink(v.current_pr.url, "PR #" + str(v.current_pr.number))); }
    if (opts.all) {
      links.push(extLink(v.url, "Live product"));
      links.push(extLink(v.repo_url, "Repository"));
    }
    links = links.filter(Boolean);
    return links.length ? h("div", { class: "row-links" }, links) : null;
  }

  function askCard(v, ctx) {
    var gate = v.gate ? (GATES[v.gate] || { kicker: "Gate · " + v.gate, ask: "The crew needs your decision to continue." }) : null;
    var stage = STAGE_BY_ID[v.stage];
    var who = v.gate ? "founder" : (stage && stage.role) || "doctor";
    var decided = v.gate_decision && v.gate_decision !== "pending";
    var card = h("article", { class: "ask" + (v.gate ? "" : " ask-blocked"), "aria-label": (gate ? gate.kicker : "Blocked") + ": " + (v.title || v.slug) });
    var headText = h("div", null,
      h("p", { class: "ask-kicker", text: gate ? gate.kicker : "Blocked · needs an answer" }),
      ctx === "inbox"
        ? h("button", { type: "button", class: "linklike ask-title", "data-fk": "inbox:" + v.slug + ":open", text: v.title || v.slug,
          on: { click: function () { openVenture(v.slug); } } })
        : null,
      h("div", { class: "meta-row" }, v.gate ? null : h("span", { class: "chip", text: stageLabel(v.stage) }), kindChip(v.kind),
        decided ? h("span", { class: "chip chip-status-done", text: "You chose: " + v.gate_decision + " · next tick applies it" }) : null,
        v.gate && v.blocked ? h("span", { class: "chip chip-blocked", text: "also blocked" }) : null));
    put(card, h("div", { class: "ask-head" }, avatar(who, "md", { awake: true }), headText));
    if (gate) { put(card, h("p", { class: "ask-q", text: gate.ask })); }
    if (v.blocked) {
      put(card, h("p", { class: "ask-q ask-q-blocked" }, inline(v.block_question || "The crew is stuck and did not say why. Open the venture for the latest events.")));
    }
    put(card, linkRow(v));
    if (ctx === "inbox") { put(card, focusArtifact(v, "inbox:" + v.slug + ":focus", false)); }
    put(card, ventureActions(v, ctx));
    return card;
  }

  function renderInbox() {
    var items = S.hq ? inboxItems() : [];
    var countEl = $("inbox-count");
    countEl.hidden = !items.length;
    countEl.textContent = String(items.length);
    countEl.setAttribute("aria-label", items.length + (items.length === 1 ? " item needs you" : " items need you"));
    document.title = (items.length ? "(" + items.length + ") " : "") + "HQ · Agent startup";
    var sig = JSON.stringify([S.loaded, !!S.hq, bridgeOnline(), ui.authLost, items.map(function (v) {
      return [v.slug, v.title, v.stage, v.kind, v.gate, v.gate_decision, v.blocked, v.block_question, v.block_url, v.design_url,
        v.current_pr, v.focus && v.focus.name, v.focus && str(v.focus.text).length, actionsSig(v.slug, "inbox")];
    })]);
    patch($("inbox-body"), "inbox", sig, function () {
      if (!S.loaded) { return h("p", { class: "loading-line", text: "Loading the founder's inbox…" }); }
      if (!S.hq) {
        return h("div", { class: "all-clear" }, avatar("founder", "lg", { awake: true }),
          h("div", null, h("h3", { text: "Inbox waits for the laptop" }), h("p", { text: "Gates and blocked ventures show up once the laptop pushes its first HQ update." })));
      }
      if (!items.length) {
        return h("div", { class: "all-clear" }, avatar("founder", "lg", { awake: true }),
          h("div", null, h("h3", { text: "All clear" }), h("p", { text: "No gate waits on you and nothing is blocked. The crew will ping this inbox when they need a decision." })));
      }
      return h("ul", { class: "inbox-list" }, items.map(function (v) { return h("li", null, askCard(v, "inbox")); }));
    });
  }

  // ------------------------------------------------------------------ crew

  function renderCrew() {
    var roles = crewRoles();
    var rs = runs();
    var stale = liveStale();
    var working = dict();
    rs.forEach(function (r) { working[r.role] = true; });
    var idle = roles.filter(function (r) { return !working[r.role]; });
    $("crew-meta").textContent = rs.length ? rs.length + " working · " + idle.length + " resting" : "Everyone's resting";
    var sig = JSON.stringify([stale, roles.map(function (r) { return [r.role, r.model, r.engine]; }),
      rs.map(function (r) { return [r.run_id, r.role, r.slug, r.leg, r.engine, r.model, r.started_at, titleOf(r.slug)]; })]);
    patch($("crew-body"), "crew", sig, function () {
      var grid = h("div", { class: "crew-grid" });
      rs.forEach(function (r) {
        var title = titleOf(r.slug);
        var tile = h("button", {
          type: "button", class: "crew-tile is-working" + (stale ? " is-stale" : ""), "data-fk": "crew:run:" + r.run_id,
          "aria-label": roleName(r.role) + " is working on " + title + ". Open the live log.",
          on: { click: function () { openLog(r.run_id); } }
        },
          avatar(r.role, "lg"),
          h("span", { class: "crew-name" }, roleName(r.role), h("span", { class: "typing", "aria-hidden": "true" }, h("i"), h("i"), h("i"))),
          h("span", { class: "crew-venture", text: title }),
          h("span", { class: "crew-meta" },
            h("span", { class: "elapsed", "data-since": str(r.started_at), text: isFinite(tms(r.started_at)) ? dur((now() - tms(r.started_at)) / 1000) : "" }),
            r.model ? h("span", { class: "chip mono", title: str(r.engine) + " · " + str(r.model), text: shortModel(r.model) }) : null,
            r.leg ? h("span", { class: "chip" + (r.leg === "fallback" ? " chip-leg-fallback" : ""), text: str(r.leg) }) : null),
          stale ? h("span", { class: "crew-stale-note", text: "signal lost · last seen " + ago(liveAge()) })
            : h("span", { class: "crew-cta" }, icon("log"), "Live log"));
        roleClass(tile, r.role);
        grid.appendChild(tile);
      });
      idle.forEach(function (r) {
        var k = roleKey(r.role);
        var tile = h("div", { class: "crew-tile", role: "group", "aria-label": roleName(r.role) + " is resting" },
          avatar(r.role, "lg"),
          h("span", { class: "crew-name", text: roleName(r.role) }),
          h("span", { class: "crew-tag", text: ART[k].tag }),
          r.model ? h("span", { class: "chip mono crew-model", title: str(r.engine) + " · " + str(r.model), text: shortModel(r.model) }) : null);
        roleClass(tile, r.role);
        grid.appendChild(tile);
      });
      return grid;
    });
  }

  // ------------------------------------------------------------------ pipeline

  function meter(v) {
    var t = Number(v.ticks);
    var m = Number(v.max_ticks);
    if (!isFinite(t) || !isFinite(m) || m <= 0 || v.ticks === null) { return null; }
    var p = Math.max(0, Math.min(1, t / m));
    var bar = h("span", { class: "meter" + (p >= 1 ? " is-full" : p >= .75 ? " is-high" : ""), role: "img", "aria-label": t + " of " + m + " ticks used" }, h("i"));
    bar.style.setProperty("--p", String(p));
    return [bar, h("span", { class: "meter-label", "aria-hidden": "true", text: t + "/" + m })];
  }

  function ventureCard(v) {
    var live = runsFor(v.slug);
    var card = h("button", {
      type: "button", class: "vcard" + (v.gate ? " is-gate" : "") + (v.blocked ? " is-blocked" : "") + (live.length ? " is-live" : ""),
      "data-card": v.slug, "data-fk": "card:" + v.slug,
      "aria-label": (v.title || v.slug) + ", " + stageLabel(v.stage) + (v.gate ? ", waiting on you" : "") + (v.blocked ? ", blocked" : "") +
        (live.length ? ", " + live.map(function (r) { return roleName(r.role); }).join(" and ") + " working" : ""),
      on: { click: function () { openVenture(v.slug); } }
    },
      h("span", { class: "vcard-top" }, kindChip(v.kind),
        v.gate ? h("span", { class: "chip chip-gate", text: "Your call" }) : null,
        v.blocked ? h("span", { class: "chip chip-blocked", text: "Blocked" }) : null),
      h("span", { class: "vcard-title", text: v.title || v.slug }),
      h("span", { class: "vcard-foot" }, meter(v),
        live.length ? h("span", { class: "live-heads" }, live.map(function (r) { return avatar(r.role, "sm", { working: true }); })) : null));
    if (live.length) {
      roleClass(card, live[0].role);
      card.style.setProperty("--live-role", "var(--role)");
    }
    return card;
  }

  function stageHeadIcon(s) {
    if (s.gate) { return avatar("founder", "sm", { awake: true }); }
    if (s.wait) { return h("span", { class: "hourglass", "aria-hidden": "true" }, icon("hourglass")); }
    return avatar(s.role, "sm");
  }

  var LEFT_SHOWN = 6;

  function lastEventAt(v) {
    var e = arr(v.events)[0];
    return tms(e && e.ts) || tms(v.created) || 0;
  }

  // Open the archive filtered to the given ventures and bring it into view.
  function showInArchive(slugs) {
    ui.archiveSlugs = slugs.slice();
    ui.sigs.archive = null;
    var det = $("archive-details");
    det.open = true;
    renderArchive();
    det.scrollIntoView({ block: "start", behavior: reducedMotion() ? "auto" : "smooth" });
    var clear = $("archive-clear");
    if (clear) { clear.focus({ preventScroll: true }); }
  }

  function renderPipeline() {
    var vs = activeVentures();
    var byStage = dict();
    var left = [];
    var other = [];
    vs.forEach(function (v) {
      if (STAGE_BY_ID[v.stage]) { (byStage[v.stage] = byStage[v.stage] || []).push(v); }
      else if (TERMINAL_STAGES.indexOf(v.stage) >= 0) { left.push(v); }
      else { other.push(v); }
    });
    left.sort(function (a, b) { return lastEventAt(b) - lastEventAt(a); });
    var onBoard = vs.length - left.length - other.length;
    $("pipeline-meta").textContent = onBoard ? onBoard + " in flight" + (doc() && doc().wip_limit_total ? " · WIP limit " + doc().wip_limit_total : "") : "";
    var sig = JSON.stringify([S.loaded, vs.map(function (v) {
      return [v.slug, v.title, v.stage, v.kind, v.ticks, v.max_ticks, !!v.gate, !!v.blocked, lastEventAt(v)];
    }), runs().map(function (r) { return r.slug + ":" + r.role; })]);

    var body = $("pipeline-body");
    if (ui.sigs.pipeline === sig) { return; }
    // Card positions before the swap drive the move animation. Measure only
    // when something changed: this forces a layout on every card.
    var before = dict();
    Array.prototype.forEach.call(body.querySelectorAll("[data-card]"), function (el) {
      before[el.getAttribute("data-card")] = el.getBoundingClientRect();
    });
    var changed = patch(body, "pipeline", sig, function () {
      if (!S.loaded) { return h("p", { class: "loading-line", text: "Loading the pipeline…" }); }
      if (!onBoard && !left.length && !other.length) {
        return h("div", { class: "pipeline-empty" },
          h("div", { class: "floor-sleepers" }, ["inventor", "screener", "analyst"].map(function (r) { return avatar(r, "md"); })),
          h("h3", { text: "The floor is quiet" }),
          h("p", { text: "No venture is in flight. The next tick opens a new idea — or suggest a subject in the idea inbox and the inventor starts there." }));
      }
      var board = h("div", { class: "board" });
      PHASES.forEach(function (name, pi) {
        var stages = STAGES.filter(function (s) { return s.phase === pi; });
        var count = stages.reduce(function (n, s) { return n + (byStage[s.id] || []).length; }, 0);
        var phase = h("section", { class: "phase", "aria-label": name + " phase, " + count + " ventures" },
          h("header", { class: "phase-head" }, h("h3", { text: name }), h("span", { class: "phase-n", text: String(count) })),
          h("div", { class: "phase-strip", "aria-hidden": "true" }, stages.map(function (s) {
            var n = (byStage[s.id] || []).length;
            return h("span", { class: "chip" + (n ? " has-items" : "") + (s.gate ? " chip-gate-stage" : ""), text: s.label + (n ? " · " + n : "") });
          })));
        stages.forEach(function (s) {
          var list = byStage[s.id] || [];
          var owner = s.gate ? "Your checkpoint" : s.wait ? "Waiting on GitHub" : roleName(s.role) + (s.also ? " + " + roleName(s.also) : "");
          var block = h("div", { class: "stage" + (s.gate ? " stage-gate" : "") + (s.wait ? " stage-wait" : "") + (list.length ? "" : " is-empty") },
            h("div", { class: "stage-head" }, stageHeadIcon(s),
              h("span", { class: "stage-label" }, s.label, h("span", { class: "stage-owner", text: owner })),
              list.length ? h("span", { class: "stage-count", text: String(list.length) }) : null),
            list.length ? h("ul", { class: "cards" }, list.map(function (v) { return h("li", null, ventureCard(v)); })) : null);
          phase.appendChild(block);
        });
        board.appendChild(phase);
      });
      var frag = document.createDocumentFragment();
      put(frag, board);
      function offButton(v) {
        return h("button", { type: "button", class: "btn btn-quiet btn-sm", "data-fk": "off:" + v.slug, on: { click: function () { openVenture(v.slug); } } },
          v.title || v.slug, h("span", { class: "chip", text: stageLabel(v.stage) }));
      }
      if (left.length) {
        var hidden = left.length - LEFT_SHOWN;
        put(frag, h("div", { class: "offboard" }, h("span", { text: "Recently left the board:" }),
          left.slice(0, hidden > 0 ? LEFT_SHOWN : left.length).map(offButton),
          hidden > 0 ? h("button", { type: "button", class: "btn btn-ghost btn-sm offboard-more", "data-fk": "off:more",
            "aria-label": "Show all " + left.length + " ventures that left the board in the archive",
            on: { click: function () { showInArchive(left.map(function (v) { return v.slug; })); } } },
          "+" + hidden + " more", icon("arrow")) : null));
      }
      if (other.length) {
        put(frag, h("div", { class: "offboard" }, h("span", { text: "Off the board:" }), other.map(offButton)));
      }
      return frag;
    });
    if (!changed || !S.loaded) { return; }
    var first = ui.cardStage === null;
    var prevStage = ui.cardStage || dict();
    ui.cardStage = dict();
    vs.forEach(function (v) { ui.cardStage[v.slug] = v.stage; });
    if (first) { return; }
    var calm = reducedMotion();
    Array.prototype.forEach.call(body.querySelectorAll("[data-card]"), function (el) {
      var slug = el.getAttribute("data-card");
      var was = before[slug];
      if (!prevStage[slug]) { el.classList.add("just-added"); return; }
      if (prevStage[slug] !== ui.cardStage[slug]) {
        el.classList.add("just-moved");
        if (was && !calm && el.animate) {
          var now2 = el.getBoundingClientRect();
          var dx = was.left - now2.left;
          var dy = was.top - now2.top;
          if (Math.abs(dx) + Math.abs(dy) > 2) {
            el.animate([{ transform: "translate(" + dx + "px," + dy + "px)" }, { transform: "none" }],
              { duration: 560, easing: "cubic-bezier(.2,.8,.2,1)" });
          }
        }
      }
    });
  }

  // ------------------------------------------------------------------ activity

  var AGENT_EVENTS = dict({ role_start: 1, role_done: 1, role_failed: 1, failover: 1, session_resume: 1 });
  var EVENT_GLYPH = dict({
    role_start: "play", role_done: "check", role_failed: "x", failover: "swap", blocked: "pause", killed: "ban",
    gate: "flag", created: "sparkle", session_resume: "resume", stage_change: "arrow",
    cmd_queued: "send", cmd_done: "check", cmd_failed: "x"
  });

  function roleOfDetail(detail) {
    var m = /^([a-z][a-z0-9_-]*)(?: via |:)/i.exec(str(detail));
    return m && knownRole(m[1].toLowerCase()) ? m[1].toLowerCase() : null;
  }

  function feedItems() {
    var d = doc();
    var acts = arr(d && d.activity).filter(function (a) { return a && a.event; });
    var tokens = {};
    acts.forEach(function (a) {
      if (a.event === "role_tokens") {
        var m = /(\d+)s\s+([\d.]+[kMG]?)\s+tokens/.exec(str(a.detail));
        if (m) { tokens[a.slug + "|" + roleOfDetail(a.detail) + "|" + a.ts] = dur(Number(m[1])) + " · " + m[2] + " tokens"; }
      }
    });
    var items = acts.filter(function (a) { return a.event !== "role_tokens"; }).map(function (a) {
      var role = roleOfDetail(a.detail);
      var via = /via\s+(\S+)(?:\s+\((primary|fallback)\))?/.exec(str(a.detail));
      return {
        key: "a|" + a.ts + "|" + a.event + "|" + a.slug + "|" + str(a.detail).slice(0, 60),
        ts: a.ts, event: str(a.event), slug: a.slug, title: a.title || a.slug, role: role, detail: str(a.detail),
        model: via ? via[1] : null, leg: via ? via[2] : null, tokens: tokens[a.slug + "|" + role + "|" + a.ts] || null,
        group: AGENT_EVENTS[a.event] ? "agents" : "ventures"
      };
    });
    commandsSorted().forEach(function (c) {
      var fin = isFinal(c);
      items.push({
        key: "c|" + c.id + "|" + c.status, ts: (fin && c.finished_at) || c.created_at, event: fin ? "cmd_" + c.status : "cmd_queued",
        slug: c.slug, title: c.slug ? titleOf(c.slug) : "", role: "founder", cmd: c, group: "you",
        detail: fin ? str(c.message) : (c.text ? "“" + str(c.text).slice(0, 180) + "”" : "")
      });
    });
    items.sort(function (a, b) { return (tms(b.ts) || 0) - (tms(a.ts) || 0); });
    return items;
  }

  function feedLine(it) {
    var who = it.role && it.role !== "founder" ? h("strong", { class: "feed-who", text: roleName(it.role) }) : null;
    var venture = it.slug ? h("button", { type: "button", class: "linklike", "data-fk": "feed:" + it.key, text: it.title || it.slug,
      on: { click: function () { openVenture(it.slug); } } }) : null;
    var parts;
    switch (it.event) {
      case "role_start": parts = [who, " started on ", venture]; break;
      case "role_done": parts = [who, " finished ", venture]; break;
      case "role_failed": parts = [who, " failed on ", venture]; break;
      case "failover": parts = [who || "An agent", " switched to its fallback on ", venture]; break;
      case "session_resume": parts = [who, " picked up where it left off on ", venture]; break;
      case "blocked": parts = [venture, " is blocked"]; break;
      case "killed": parts = [venture, " was killed"]; break;
      case "gate": parts = [venture, " is waiting on you"]; break;
      case "created": parts = ["New venture ", venture]; break;
      case "stage_change": parts = [venture, " moved stage"]; break;
      case "cmd_queued":
      case "cmd_done":
      case "cmd_failed":
        parts = [h("strong", { text: "You" }), " · " + cmdLabel(it.cmd.kind), venture ? " · " : null, venture,
          " ", statusChip(it.cmd.status)];
        break;
      default: parts = [who, who ? " · " : null, h("strong", { text: it.event.replace(/_/g, " ") }), venture ? " · " : null, venture];
    }
    return h("p", { class: "feed-line" }, parts);
  }

  function feedIcon(it) {
    var wrap = h("span", { class: "feed-icon", "aria-hidden": "true" });
    var known = !!EVENT_GLYPH[it.event];
    if (it.role) {
      wrap.appendChild(avatar(it.role, "md", { awake: it.event === "role_start" || it.role === "founder" }));
    } else if (known) {
      wrap.appendChild(h("span", { class: "ev-disc" }, icon(EVENT_GLYPH[it.event])));
      return wrap;
    } else {
      wrap.appendChild(h("span", { class: "neutral-dot" }));
      return wrap;
    }
    if (known) { wrap.appendChild(h("span", { class: "feed-badge" }, icon(EVENT_GLYPH[it.event]))); }
    return wrap;
  }

  function renderActivity() {
    var all = feedItems();
    var filter = ui.feedFilter;
    var items = all.filter(function (it) { return filter === "all" || it.group === filter; }).slice(0, 80);
    $("activity-meta").textContent = all.length ? all.length + " events" : "";
    var sig = JSON.stringify([S.loaded, filter, items.map(function (i) { return i.key; })]);
    var firstRender = ui.feedSeen === null;
    var seen = ui.feedSeen || {};
    patch($("activity-body"), "activity", sig, function () {
      var frag = document.createDocumentFragment();
      var filters = h("div", { class: "feed-filters", role: "group", "aria-label": "Filter activity" });
      [["all", "All"], ["agents", "Agents"], ["ventures", "Ventures"], ["you", "You"]].forEach(function (f) {
        filters.appendChild(h("button", { type: "button", class: "filter-chip", "aria-pressed": filter === f[0] ? "true" : "false", "data-fk": "feed-filter:" + f[0],
          on: { click: function () { ui.feedFilter = f[0]; renderActivity(); } } }, f[1]));
      });
      put(frag, filters);
      if (!S.loaded) { put(frag, h("p", { class: "loading-line", text: "Loading activity…" })); return frag; }
      if (!items.length) {
        put(frag, h("p", { class: "empty-line", text: filter === "all" ? "Nothing has happened yet. Events stream in here as the crew works." : "No events of this kind yet." }));
        return frag;
      }
      var ul = h("ul", { class: "feed", "data-keep-scroll": "feed", "aria-label": "Recent activity, newest first" });
      items.forEach(function (it) {
        var chips = [];
        if (it.model) { chips.push(h("span", { class: "chip mono", text: shortModel(it.model) })); }
        if (it.leg === "fallback") { chips.push(h("span", { class: "chip chip-leg-fallback", text: "fallback" })); }
        if (it.tokens) { chips.push(h("span", { class: "chip mono", text: it.tokens })); }
        var detail = it.model ? "" : it.detail;
        var li = h("li", { class: "feed-item ev-" + it.event.replace(/[^a-z0-9_]/gi, "") + (!firstRender && !seen[it.key] ? " is-new" : "") },
          feedIcon(it),
          h("div", null, feedLine(it), detail ? h("p", { class: "feed-detail", title: detail.length > 200 ? detail.slice(0, 600) : null, text: detail }) : null,
            chips.length ? h("div", { class: "feed-chips" }, chips) : null),
          timeEl(it.ts));
        if (it.role) { roleClass(li, it.role); }
        ul.appendChild(li);
      });
      put(frag, ul);
      return frag;
    });
    ui.feedSeen = {};
    all.forEach(function (it) { ui.feedSeen[it.key] = true; });
  }

  // ------------------------------------------------------------------ ideas

  function renderIdeas() {
    var d = doc();
    var sugg = arr(d && d.suggestions).filter(function (s) { return s && s.text; });
    // A suggest command the laptop already filed (its result is not back yet)
    // is listed once, as the suggestion it became.
    var pending = pendingFor(null, { suggest: 1 }).filter(function (c) {
      var at = tms(c.created_at) || 0;
      return !sugg.some(function (s) { return str(s.text).trim() === str(c.text).trim() && (tms(s.ts) || 0) >= at - 5000; });
    });
    var queued = sugg.filter(function (s) { return s.status === "queued"; })
      .sort(function (a, b) { return (tms(a.ts) || 0) - (tms(b.ts) || 0); });
    var sig = JSON.stringify([S.loaded, bridgeOnline(), ui.errors.suggest || "", !!ui.busy.suggest, ui.authLost,
      sugg.map(function (s) { return [s.id, s.status, s.slug, s.text, s.ts, s.slug ? titleOf(s.slug) : ""]; }),
      pending.map(function (c) { return c.id + c.status; })]);
    patch($("ideas-body"), "ideas", sig, function () {
      var frag = document.createDocumentFragment();
      put(frag, h("p", { class: "ideas-lede", text: "Suggest a subject for a future venture. The next new venture researches the oldest queued subject first." }));
      var submit = function () {
        var text = str(ui.drafts.suggest).trim();
        if (!text) { ui.errors.suggest = "Write a subject first."; announce(ui.errors.suggest); renderIdeas(); focusSoon("idea:ta"); return; }
        sendCommand("suggest", { kind: "suggest", text: text }, function () { delete ui.drafts.suggest; });
      };
      put(frag, h("div", { class: "idea-form" }, textField({
        fk: "idea:ta", draft: "suggest", label: "Subject", max: TEXT_LIMIT.suggest, rows: 3, error: ui.errors.suggest,
        placeholder: "e.g. Tools for tabletop game masters running long campaigns", submit: submit,
        buttons: [h("button", { type: "button", class: "btn btn-primary btn-sm", "data-fk": "idea:submit", disabled: !!ui.busy.suggest || ui.authLost, on: { click: submit } },
          icon("send"), ui.busy.suggest ? "Sending…" : "Queue subject")]
      }), offlineHint()));
      var list = h("ul", { class: "idea-list", "aria-label": "Suggested subjects" });
      pending.forEach(function (c) {
        list.appendChild(h("li", { class: "idea is-queued" }, h("span", { class: "idea-text", text: str(c.text) }),
          statusChip(c.status, c.status === "queued" ? "sending to laptop" : "laptop is filing it"),
          h("span", { class: "idea-meta" }, timeEl(c.created_at))));
      });
      sugg.forEach(function (s) {
        var pos = queued.indexOf(s);
        var meta = [timeEl(s.ts)];
        if (s.status === "picked" && s.slug) {
          meta.push(h("span", null, "→ ", h("button", { type: "button", class: "linklike", "data-fk": "idea:open:" + s.id, text: titleOf(s.slug),
            on: { click: function () { openVenture(s.slug); } } })));
        } else if (pos === 0) {
          meta.push(h("strong", { text: "next up" }));
        } else if (pos > 0) {
          meta.push(h("span", { text: "#" + (pos + 1) + " in line" }));
        }
        list.appendChild(h("li", { class: "idea" + (s.status === "queued" ? " is-queued" : "") },
          h("span", { class: "idea-text", text: s.text }),
          h("span", { class: "chip " + (s.status === "picked" ? "chip-picked" : "chip-queued"), text: str(s.status) }),
          h("span", { class: "idea-meta" }, meta)));
      });
      if (list.children.length) { put(frag, list); }
      else if (S.loaded) { put(frag, h("p", { class: "empty-line", text: "No subjects queued. The inventor picks its own until you suggest one." })); }
      return frag;
    });
  }

  // ------------------------------------------------------------------ archive

  var archiveBuilt = false;

  function archiveRows() {
    return S.ventures.filter(function (v) { return v.status === "abandoned" || v.status === "deployed"; })
      .sort(function (a, b) {
        if ((a.status === "deployed") !== (b.status === "deployed")) { return a.status === "deployed" ? -1 : 1; }
        return (tms(b.created) || 0) - (tms(a.created) || 0);
      });
  }

  function renderArchive() {
    var rows = archiveRows();
    var dep = rows.filter(function (v) { return v.status === "deployed"; }).length;
    $("archive-meta").textContent = S.loaded ? (rows.length - dep) + " abandoned · " + dep + " deployed" : "";
    var det = $("archive-details");
    if (!det.open) { return; }
    var body = $("archive-body");
    if (!archiveBuilt) {
      archiveBuilt = true;
      var input = h("input", { class: "input", id: "archive-q", type: "search", "data-fk": "archive:q", placeholder: "Filter by title, reason, field…", autocomplete: "off" });
      input.value = ui.archiveQuery;
      input.addEventListener("input", function () { ui.archiveQuery = input.value; renderArchive(); });
      var clear = h("button", { type: "button", class: "filter-chip archive-clear", id: "archive-clear", "aria-pressed": "true", hidden: true,
        on: { click: function () { ui.archiveSlugs = null; renderArchive(); input.focus(); } } },
        "Left the board in the last 72 h", icon("x"), h("span", { class: "sr-only", text: " — show the whole archive" }));
      body.replaceChildren(
        h("div", { class: "archive-tools" }, h("label", { class: "sr-only", for: "archive-q", text: "Filter the archive" }), input, clear,
          h("span", { class: "form-hint", id: "archive-count", "aria-live": "polite" })),
        h("div", { id: "archive-list-host" }));
    }
    var q = ui.archiveQuery.trim().toLowerCase();
    var only = ui.archiveSlugs;
    $("archive-clear").hidden = !only;
    var shown = rows.filter(function (v) {
      if (only && only.indexOf(v.slug) < 0) { return false; }
      if (!q) { return true; }
      return [v.title, v.slug, v.abandoned_reason, v.field, v.abandoned_at_stage, v.stage].some(function (x) { return str(x).toLowerCase().indexOf(q) >= 0; });
    });
    if (only) {
      // Most recent first, the same order as the pipeline's "recently left" row.
      shown.sort(function (a, b) { return only.indexOf(a.slug) - only.indexOf(b.slug); });
    }
    $("archive-count").textContent = q || only ? shown.length + " of " + rows.length + " shown" : "";
    var sig = JSON.stringify([q, only, shown.map(function (v) { return [v.slug, v.status, v.title, v.abandoned_reason, v.abandoned_at_stage, v.url]; })]);
    patch($("archive-list-host"), "archive", sig, function () {
      if (!shown.length) { return h("p", { class: "empty-line", text: q || only ? "Nothing matches that filter." : "The archive is empty." }); }
      return h("ul", { class: "archive-list" }, shown.map(function (v) {
        var deployed = v.status === "deployed";
        return h("li", null, h("div", { class: "arow" + (deployed ? " is-deployed" : "") },
          h("div", { class: "meta-row" },
            deployed ? h("span", { class: "chip chip-pass", text: "deployed" }) : h("span", { class: "chip " + (v.stage === "ARCHIVED" ? "chip-status-delivered" : "chip-fail"), text: (v.stage === "ARCHIVED" ? "archived at " : "killed at ") + stageLabel(v.abandoned_at_stage || v.stage) }),
            v.field ? h("span", { class: "chip", text: str(v.field) }) : null, kindChip(v.kind)),
          h("button", { type: "button", class: "linklike arow-title", "data-fk": "arc:" + v.slug, text: v.title || v.slug, on: { click: function () { openVenture(v.slug); } } }),
          deployed ? h("div", { class: "row-links" }, extLink(v.url, str(v.url).replace(/^https?:\/\//, "") || "Live product"))
            : h("p", { class: "arow-reason" + (v.abandoned_reason ? "" : " is-missing"), text: v.abandoned_reason || "reason not recorded" })));
      }));
    });
  }

  // ------------------------------------------------------------------ drawer

  function openVenture(slug, fromLog) {
    if (!slug) { return; }
    if (!ui.drawer) { ui.returnFk = document.activeElement && document.activeElement.getAttribute("data-fk"); }
    ui.drawerHistory = fromLog ? [] : ui.drawerHistory;
    ui.drawer = { type: "venture", slug: slug };
    showDrawer("#v=" + encodeURIComponent(slug));
  }

  function openLog(runId) {
    var r = runById(runId);
    if (!r) { return; }
    if (!ui.drawer) { ui.returnFk = document.activeElement && document.activeElement.getAttribute("data-fk"); ui.drawerHistory = []; }
    else if (ui.drawer.type === "venture") { ui.drawerHistory = [ui.drawer]; }
    ui.drawer = { type: "log", runId: runId, slug: r.slug, role: r.role };
    ui.lastRun = r;
    ui.logStick = true;
    showDrawer("#log=" + encodeURIComponent(runId));
  }

  function plainUrl() { return location.pathname + location.search; }
  function drawerEntry() { return !!(history.state && history.state.hqDrawer); }

  // Opening the drawer adds one history entry, so the phone's Back button (or
  // a swipe back) closes it instead of leaving HQ. Moving inside the open
  // drawer replaces that entry.
  function showDrawer(hash) {
    var drawer = $("drawer");
    var opening = drawer.hidden;
    drawer.hidden = false;
    $("scrim").hidden = false;
    document.body.classList.add("is-locked");
    ["main", "topbar", "banners"].forEach(function (id) { var el = $(id); if (el) { el.inert = true; } });
    ui.sigs.drawer = null;
    try {
      if (opening && !drawerEntry()) { history.pushState({ hqDrawer: true }, "", plainUrl() + hash); }
      else { history.replaceState({ hqDrawer: true }, "", plainUrl() + hash); }
    } catch (e) { /* ignore */ }
    renderDrawer();
    $("drawer-body").scrollTop = 0;
    $("drawer-title").focus({ preventScroll: true });
  }

  function closeDrawer(fromHistory) {
    var drawer = $("drawer");
    if (drawer.hidden) { return; }
    drawer.hidden = true;
    $("scrim").hidden = true;
    document.body.classList.remove("is-locked");
    ["main", "topbar", "banners"].forEach(function (id) { var el = $(id); if (el) { el.inert = false; } });
    ui.drawer = null;
    ui.drawerHistory = [];
    ui.sigs.drawer = null;
    try {
      // Closed from the UI: pop the entry the drawer pushed (the popstate that
      // follows finds the drawer closed and only tidies the URL).
      if (fromHistory !== true && drawerEntry()) { history.back(); }
      else { history.replaceState(null, "", plainUrl()); }
    } catch (e) { /* ignore */ }
    var back = ui.returnFk ? document.querySelector('[data-fk="' + cssEsc(ui.returnFk) + '"]') : null;
    ui.returnFk = null;
    if (back && !back.disabled) { back.focus(); } else { $("main").focus({ preventScroll: true }); }
  }

  function trapFocus(e) {
    var drawer = $("drawer");
    if (drawer.hidden || e.key !== "Tab") { return; }
    var nodes = Array.prototype.filter.call(drawer.querySelectorAll('a[href], button:not([disabled]), textarea:not([disabled]), input:not([disabled]), summary, [tabindex]:not([tabindex="-1"])'),
      function (n) { return n.offsetParent !== null || n === document.activeElement; });
    if (!nodes.length) { return; }
    var first = nodes[0];
    var last = nodes[nodes.length - 1];
    if (e.shiftKey && (document.activeElement === first || document.activeElement === $("drawer-title"))) { e.preventDefault(); last.focus(); }
    else if (!e.shiftKey && document.activeElement === last) { e.preventDefault(); first.focus(); }
  }

  function stepper(v, pub) {
    var terminal = !STAGE_BY_ID[v.stage];
    var stopAt = terminal && pub && pub.abandoned_at_stage ? STAGE_BY_ID[pub.abandoned_at_stage] : null;
    var cur = STAGE_BY_ID[v.stage] || stopAt;
    var idx = cur ? cur.index : -1;
    var wrap = h("div", { class: "stepper", role: "group", "aria-label": "Pipeline progress" });
    PHASES.forEach(function (name, pi) {
      var nameId = "step-phase-" + pi;
      var col = h("div", { class: "step-phase", role: "list", "aria-labelledby": nameId }, h("span", { class: "step-phase-name", id: nameId, text: name }));
      STAGES.filter(function (s) { return s.phase === pi; }).forEach(function (s) {
        var state = s.index < idx ? "done" : s.index === idx ? (stopAt ? "stop" : "now") : "next";
        var dot = h("span", { class: "step-dot" }, state === "done" ? icon("check") : state === "stop" ? icon("x") : null);
        col.appendChild(h("div", { class: "step is-" + state + (s.gate ? " is-gate" : ""), role: "listitem",
          "aria-current": state === "now" ? "step" : null },
          dot, h("span", { class: "step-text", text: s.label }),
          h("span", { class: "sr-only", text: state === "done" ? " (done)" : state === "now" ? " (current)" : state === "stop" ? " (stopped here)" : "" })));
      });
      wrap.appendChild(col);
    });
    return wrap;
  }

  function dsec(title, kids) {
    var content = Array.isArray(kids) ? kids.filter(Boolean) : kids;
    if (!content || (Array.isArray(content) && !content.length)) { return null; }
    return h("section", { class: "dsec" }, h("h3", { text: title }), content);
  }

  function verdictChip(vd) {
    var v = str(vd);
    var cls = v === "pass" ? "chip-pass" : v === "fail" ? "chip-fail" : v === "blocked" ? "chip-blocked-verdict" : "";
    return v ? h("span", { class: "chip " + cls, text: v }) : null;
  }

  function fmtTokens(n) {
    n = Number(n) || 0;
    return n >= 1e6 ? (n / 1e6).toFixed(1) + "M" : n >= 1e3 ? (n / 1e3).toFixed(1) + "k" : String(n);
  }

  function noteForm(slug) {
    var scope = "note:" + slug;
    var draft = scope;
    var busy = !!ui.busy[scope];
    var submit = function () {
      var text = str(ui.drafts[draft]).trim();
      if (!text) { ui.errors[scope] = "Write the note first."; announce(ui.errors[scope]); renderDrawer(); focusSoon(scope + ":ta"); return; }
      sendCommand(scope, { kind: "note", slug: slug, text: text }, function () { delete ui.drafts[draft]; });
    };
    return h("div", { class: "action-form" }, textField({
      fk: scope + ":ta", draft: draft, label: "Note for the next worker on this venture", max: TEXT_LIMIT.note, error: ui.errors[scope],
      submit: submit, placeholder: "e.g. Keep the French copy informal.",
      buttons: [h("button", { type: "button", class: "btn btn-sm btn-primary", "data-fk": scope + ":submit", disabled: busy || ui.authLost, on: { click: submit } },
        icon("note"), busy ? "Sending…" : "Add note")]
    }), h("p", { class: "form-hint", text: "Tip: Ctrl/⌘ + Enter sends." }), offlineHint());
  }

  // One list of founder notes for a venture, each note once. The same text can
  // arrive three ways: a note command still in the queue, the snapshot's
  // queued_notes (not yet read by a worker) and the HQ doc's last notes.
  function founderNotes(slug, v) {
    var norm = function (t) { return str(t).trim(); };
    var queued = dict();
    arr(S.status && S.status.queued_notes && S.status.queued_notes[slug]).forEach(function (t) {
      queued[norm(t)] = (queued[norm(t)] || 0) + 1;
    });
    var onDisk = dict(); // text -> newest note time
    var fromDoc = arr(v.notes).filter(function (n) { return n && norm(n.text); }).map(function (n) {
      var t = norm(n.text);
      onDisk[t] = Math.max(onDisk[t] || 0, tms(n.ts) || 0);
      var isQueued = queued[t] > 0;
      if (isQueued) { queued[t] -= 1; }
      return { text: str(n.text), ts: n.ts, queued: isQueued };
    });
    var out = [];
    pendingFor(slug, { note: 1 }).forEach(function (c) {
      var t = norm(c.text);
      // The laptop already wrote it (a note no older than the command, or the
      // same instruction still queued); its result is just not back yet.
      if ((t in onDisk && onDisk[t] >= (tms(c.created_at) || 0) - 5000) || queued[t] > 0) { return; }
      out.push({ text: str(c.text), ts: null, cmd: c });
    });
    // Queued notes older than the doc's last five still wait for a worker.
    Object.keys(queued).forEach(function (t) {
      for (var i = 0; i < queued[t]; i++) { out.push({ text: t, ts: null, queued: true }); }
    });
    return out.concat(fromDoc);
  }

  function ventureDrawer(slug) {
    var hv = hqBySlug(slug);
    var pub = pubBySlug(slug);
    var v = hv || (pub ? fromPublic(pub) : null);
    $("drawer-back").hidden = true;
    if (!v) {
      $("drawer-kicker").textContent = "Venture";
      $("drawer-title").textContent = slug;
      return h("p", { class: "empty-line", text: "This venture is not in the current data. It may have been archived; try the archive list." });
    }
    $("drawer-kicker").textContent = [stageLabel(v.stage), v.kind].filter(Boolean).join(" · ");
    $("drawer-title").textContent = v.title || v.slug;
    var frag = document.createDocumentFragment();
    var asks = needsFounder(v);
    var metaBits = [h("span", { class: "chip", text: stageLabel(v.stage) }), kindChip(v.kind), v.field ? h("span", { class: "chip", text: str(v.field) }) : null,
      v.priority ? h("span", { class: "chip", text: "priority " + v.priority }) : null, meter(v),
      v.created ? h("span", { class: "chip" }, "opened ", timeEl(v.created, "")) : null,
      pub && pub.tokens ? h("span", { class: "chip mono", text: str(pub.tokens.runs) + " runs · " + fmtTokens(pub.tokens.fresh) + " fresh tokens" }) : null];
    put(frag, h("section", { class: "dsec" }, h("div", { class: "meta-row" }, metaBits),
      linkRow(asks ? { url: v.url || (pub && pub.url), repo_url: v.repo_url || (pub && pub.repo_url) }
        : { design_url: v.design_url || (pub && pub.design_url), current_pr: v.current_pr, url: v.url || (pub && pub.url), repo_url: v.repo_url || (pub && pub.repo_url) },
      { all: true, design: !asks })));
    if (v.suggestion && v.suggestion.text) {
      put(frag, h("p", { class: "note is-queued", text: "Opened from your suggestion: “" + str(v.suggestion.text) + "”" }));
    }
    if (asks) { put(frag, dsec("Needs you", [askCard(v, "drawer")])); }
    var live = runsFor(slug);
    put(frag, dsec("Working on it now", live.length ? [h("div", { class: "drawer-live" }, live.map(function (r) {
      var b = h("button", { type: "button", class: "live-btn", "data-fk": "dlive:" + r.run_id, on: { click: function () { openLog(r.run_id); } } },
        avatar(r.role, "sm", { working: true }), roleName(r.role) + " · live log", h("span", { class: "elapsed", "data-since": str(r.started_at) }));
      roleClass(b, r.role);
      return b;
    }))] : null));
    put(frag, dsec("Progress", [stepper(v, pub)]));
    var about = [];
    if (pub && pub.description) { about.push(h("p", { class: "prose", text: pub.description })); }
    if (pub && pub.monetization && v.kind !== "internal") { about.push(h("p", { class: "prose" }, h("strong", { text: "Monetization: " }), pub.monetization)); }
    if (pub && pub.status === "abandoned") {
      about.push(h("p", { class: "prose" }, h("strong", { text: "Abandoned" + (pub.abandoned_at_stage ? " at " + stageLabel(pub.abandoned_at_stage) : "") + ": " }),
        pub.abandoned_reason || "reason not recorded"));
    }
    put(frag, dsec("About", about));
    if (v.focus && v.focus.text) {
      put(frag, dsec(v.gate ? "The artifact to review" : "Current artifact", [focusArtifact(v, "drawer:" + slug + ":focus", true)]));
    }
    put(frag, dsec("Artifacts", arr(v.artifacts).length ? [h("ul", { class: "list-plain" }, arr(v.artifacts).map(function (a) {
      return h("li", { class: "art-row" }, avatar(a.author || "unknown", "xs"), h("span", { class: "mono", text: str(a.name) }), verdictChip(a.verdict));
    }))] : null));
    var notes = founderNotes(slug, v).map(function (n) {
      return h("li", { class: "note" + (n.queued || n.cmd ? " is-queued" : "") }, n.text,
        h("span", { class: "note-meta" },
          n.ts ? timeEl(n.ts, "") : null,
          n.ts && (n.queued || n.cmd) ? " · " : null,
          n.cmd ? "in the command queue · " + str(n.cmd.status) : n.queued ? "queued for the next worker" : null));
    });
    put(frag, h("section", { class: "dsec" }, h("h3", { text: "Founder notes" }),
      notes.length ? h("ul", { class: "list-plain" }, notes) : null,
      hv ? noteForm(slug) : h("p", { class: "form-hint", text: "Notes can be added once the laptop reports this venture." })));
    var cmds = commandsSorted().filter(function (c) { return c.slug === slug; });
    put(frag, dsec("Your commands", cmds.length ? [h("ul", { class: "list-plain" }, cmds.slice(0, 12).map(function (c) {
      return h("li", { class: "art-row cmd-row" }, h("strong", { text: cmdLabel(c.kind) }), c.gate ? h("span", { class: "chip", text: str(c.gate) }) : null,
        h("span", { class: "feed-detail", text: str(c.message || c.text) }), timeEl(c.finished_at || c.created_at), statusChip(c.status));
    }))] : null));
    put(frag, dsec("Recent events", arr(v.events).length ? [h("ol", { class: "timeline" }, arr(v.events).map(function (e) {
      var role = roleOfDetail(e.detail);
      var li = h("li", { class: "tl-item" }, h("strong", { text: str(e.event).replace(/_/g, " ") }), timeEl(e.ts), h("span", { class: "tl-detail", text: str(e.detail) }));
      if (role) { roleClass(li, role); }
      return li;
    }))] : null));
    return frag;
  }

  function logDrawer() {
    var d = ui.drawer;
    var r = runById(d.runId);
    if (r) { ui.lastRun = r; }
    var run = r || ui.lastRun;
    $("drawer-back").hidden = !ui.drawerHistory.length;
    $("drawer-kicker").textContent = titleOf(d.slug);
    $("drawer-title").textContent = roleName(d.role) + " · live log";
    var frag = document.createDocumentFragment();
    var meta = h("div", { class: "log-meta" },
      run && run.model ? h("span", { class: "chip mono", text: str(run.engine) + " · " + shortModel(run.model) }) : null,
      run && run.leg ? h("span", { class: "chip" + (run.leg === "fallback" ? " chip-leg-fallback" : ""), text: str(run.leg) + " leg" }) : null,
      r ? h("span", { class: "chip" }, "running ", h("span", { class: "elapsed", "data-since": str(run.started_at) })) : h("span", { class: "chip chip-status-done", text: "finished" }),
      run && run.log_bytes ? h("span", { class: "chip mono", text: fmtTokens(run.log_bytes) + "B of events" }) : null);
    var head = h("div", { class: "log-head" + (r ? " is-working" : "") }, avatar(d.role, "lg"),
      h("div", null, h("button", { type: "button", class: "linklike", "data-fk": "log:venture", text: titleOf(d.slug), on: { click: function () { openVenture(d.slug, true); } } }), meta));
    roleClass(head, d.role);
    put(frag, head);
    if (!r) { put(frag, h("p", { class: "log-ended", role: "status", text: "This run has ended (or the live feed stopped). Showing its last steps." })); }
    // Typed steps (messages, thoughts, tool calls), drawn by livelog.js. The role
    // class colours the agent's own messages in its crew colour.
    var host = h("div", { class: "log-steps", id: "log-steps" });
    roleClass(host, d.role);
    ui.log = window.LiveLog ? window.LiveLog.mount(host, {
      who: roleName(d.role), label: roleName(d.role) + " steps", stick: ui.logStick,
      onStick: function (atBottom) { ui.logStick = atBottom; }
    }) : null;
    if (ui.log) { ui.log.update(run, { working: !!r && !liveStale(), ended: !r }); }
    else { put(host, h("pre", { class: "log-fallback", text: (run && run.log_tail) || "Waiting for output…" })); }
    put(frag, host);
    return frag;
  }

  function updateLogText() {
    if (!ui.log || !ui.drawer || ui.drawer.type !== "log") { return; }
    var r = runById(ui.drawer.runId);
    if (!r) { return; }
    ui.log.update(r, { working: !liveStale() });
  }

  function renderDrawer() {
    if (!ui.drawer) { return; }
    var d = ui.drawer;
    var body = $("drawer-body");
    $("drawer").classList.toggle("is-log", d.type === "log");
    if (d.type !== "log") { ui.log = null; }
    if (d.type === "venture") {
      var hv = hqBySlug(d.slug);
      var pub = pubBySlug(d.slug);
      var sig = JSON.stringify(["v", d.slug, hv, pub, S.status && S.status.queued_notes && S.status.queued_notes[d.slug],
        runsFor(d.slug).map(function (r) { return r.run_id; }), actionsSig(d.slug, "drawer"), bridgeOnline(), ui.errors["note:" + d.slug] || "",
        !!ui.busy["note:" + d.slug], ui.authLost]);
      patch(body, "drawer", sig, function () { return ventureDrawer(d.slug); });
    } else {
      var alive = !!runById(d.runId);
      var sig2 = JSON.stringify(["l", d.runId, alive, titleOf(d.slug)]);
      if (!patch(body, "drawer", sig2, logDrawer)) { updateLogText(); }
    }
  }

  // ------------------------------------------------------------------ per-second clock

  function tickClock() {
    watchSocket();
    if (document.hidden) { return; }
    guarded("clock", tickTimes);
    if (S.loaded) {
      guarded("top", renderTop);
      guarded("banners", renderBanners);
      if (liveStale() !== ui.lastStale) { ui.lastStale = liveStale(); guarded("crew", renderCrew); }
    }
  }

  function tickTimes() {
    Array.prototype.forEach.call(document.querySelectorAll("[data-rel]"), function (el) {
      var t = rel(el.getAttribute("data-rel"));
      if (el.textContent !== t) { el.textContent = t; }
    });
    Array.prototype.forEach.call(document.querySelectorAll("[data-since]"), function (el) {
      var t = tms(el.getAttribute("data-since"));
      if (isFinite(t)) { el.textContent = dur((now() - t) / 1000); }
    });
  }

  // ------------------------------------------------------------------ boot: control room

  function decorateStatic() {
    var glyphs = { inbox: "flag", crew: "users", pipeline: "columns", activity: "pulse", ideas: "bulb", archive: "archive" };
    Object.keys(glyphs).forEach(function (id) {
      var sec = $(id);
      var slot = sec && sec.querySelector(".h-glyph");
      if (slot) { slot.appendChild(icon(glyphs[id])); }
    });
    $("drawer-close").appendChild(icon("x"));
    $("drawer-back").appendChild(icon("back"));
    $("tick-btn-icon").appendChild(icon("bolt"));
  }

  function initHQ() {
    decorateStatic();
    $("tick-btn").addEventListener("click", function () {
      sendCommand("tick", { kind: "tick" });
    });
    $("logout-btn").addEventListener("click", function () {
      fetch("/hq/logout", { method: "POST", credentials: "same-origin" })
        .catch(function () { return null; })
        .then(function () { location.href = "/hq"; });
    });
    $("drawer-close").addEventListener("click", function () { closeDrawer(); });
    $("scrim").addEventListener("click", function () { closeDrawer(); });
    window.addEventListener("popstate", function () {
      if (ui.drawer) { closeDrawer(true); return; }
      // Our own history.back() after a close, or Forward onto an old drawer
      // entry: show the plain page.
      if (location.hash || drawerEntry()) {
        try { history.replaceState(null, "", plainUrl()); } catch (e) { /* ignore */ }
      }
    });
    $("drawer-back").addEventListener("click", function () {
      var prev = ui.drawerHistory.pop();
      if (prev) { ui.drawer = prev; ui.sigs.drawer = null; showDrawer("#v=" + encodeURIComponent(prev.slug)); }
    });
    document.addEventListener("keydown", function (e) {
      if (e.key === "Escape" && ui.drawer) { e.preventDefault(); closeDrawer(); }
      trapFocus(e);
    });
    $("archive-details").addEventListener("toggle", function () { renderArchive(); });
    window.addEventListener("online", function () { backoff = 1000; connect(); });
    document.addEventListener("visibilitychange", function () {
      if (document.visibilityState !== "visible") { hiddenAt = Date.now(); return; }
      if (!ws) {
        backoff = 1000;
        window.clearTimeout(reconnectTimer);
        reconnectTimer = null;
        connect();
        pollOnce();
      } else if (hiddenAt && Date.now() - hiddenAt > 30000) {
        // Back from a sleeping phone or a background tab: the socket may be
        // half-dead, so fetch a fresh state now rather than trusting it.
        pollOnce();
        watchSocket();
      }
      hiddenAt = 0;
      if (renderDeferred) { scheduleRender(); }
    });

    var deepLink = /^#(v|log)=([^&]+)$/.exec(location.hash || "");
    // A reload keeps the old history entry; start from the plain page so the
    // deep-linked drawer pushes exactly one entry of its own.
    if (location.hash || drawerEntry()) {
      try { history.replaceState(null, "", plainUrl()); } catch (e) { /* ignore */ }
    }
    renderAll();
    pollOnce().then(function () {
      if (deepLink && !ui.drawer) {
        var val = "";
        try { val = decodeURIComponent(deepLink[2]); } catch (e) { return; }
        if (deepLink[1] === "v") { openVenture(val); } else if (runById(val)) { openLog(val); }
      }
    });
    connect();
    window.setInterval(tickClock, 1000);
  }

  // ------------------------------------------------------------------ boot: auth pages

  function parade(awakeCount) {
    var host = $("auth-parade");
    if (!host) { return; }
    FALLBACK_ROLES.forEach(function (r, i) {
      host.appendChild(avatar(r, "md", { awake: i < (awakeCount || 0) }));
    });
  }

  function initLoginInfo() {
    parade(0);
  }

  function initLoginLink() {
    parade(0);
    var hash = location.hash || "";
    var m = /(?:^#|&)t=([^&]*)/.exec(hash);
    var token = "";
    if (m) { try { token = decodeURIComponent(m[1]); } catch (e) { token = ""; } }
    if (hash) {
      try { history.replaceState(null, "", location.pathname + location.search); } catch (e) { /* ignore */ }
    }
    function show(which, detail) {
      ["checking", "ok", "failed"].forEach(function (s) { $("state-" + s).hidden = s !== which; });
      if (detail) { $("state-failed-detail").textContent = detail; }
      if (which === "ok") {
        Array.prototype.forEach.call(document.querySelectorAll("#auth-parade .av"), function (a) { a.classList.add("is-awake"); });
      }
    }
    if (!token) { show("failed"); return; }
    fetch("/hq/login", {
      method: "POST",
      credentials: "same-origin",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ token: token })
    }).then(function (res) {
      if (res.status === 204 || res.ok) {
        show("ok");
        location.replace("/hq");
      } else if (res.status === 401) {
        show("failed");
      } else {
        show("failed", "HQ answered " + res.status + ". Wait a minute, then send /web again.");
      }
    }).catch(function () {
      show("failed", "Could not reach HQ. Check your connection, then open a fresh link.");
    });
  }

  var page = document.body ? document.body.getAttribute("data-page") : null;
  if (page === "hq") { initHQ(); }
  else if (page === "login") { initLoginInfo(); }
  else if (page === "login-link") { initLoginLink(); }
})();
