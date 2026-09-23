/* Live agent telemetry for the dashboard.
 *
 * The laptop pushes the running-agent set to /ingest/live while a tick runs;
 * this script polls /live.json and animates the result. Everything degrades
 * gracefully: with JavaScript off the page still shows the full stage flow,
 * only the "running now" layer is lost. */
(function () {
  "use strict";

  var POLL_MS = 3000;
  var STALE_AFTER = 60; // seconds without a push before we stop trusting it

  var live = null;
  var fetchedAt = 0; // local ms when the last good feed arrived
  var ageAtFetch = 0; // server-reported age at that moment
  var selectedRunId = null;
  var tickHandle = null;

  var els = {};

  function $(id) {
    return document.getElementById(id);
  }

  function init() {
    els.section = $("running-now");
    els.list = $("run-list");
    els.idle = $("live-idle");
    els.meta = $("live-meta");
    els.drawer = $("log-drawer");
    els.title = $("log-title");
    els.sub = $("log-sub");
    els.body = $("log-body");
    els.close = $("log-close");
    if (!els.section || !els.drawer) {
      return;
    }

    els.close.addEventListener("click", function () {
      closeDrawer();
    });
    document.addEventListener("keydown", function (event) {
      if (event.key === "Escape" && isDrawerOpen()) {
        closeDrawer();
      }
    });

    els.list.addEventListener("click", function (event) {
      var button = event.target.closest(".run-button");
      if (button) {
        openRun(button.getAttribute("data-run-id"));
      }
    });

    document.addEventListener("click", function (event) {
      var step = event.target.closest(".step.is-running");
      if (step) {
        var run = runForStep(step);
        if (run) {
          openRun(run.run_id);
        }
      }
    });
    document.addEventListener("keydown", function (event) {
      if (event.key !== "Enter" && event.key !== " ") {
        return;
      }
      var step = event.target.closest(".step.is-running");
      if (step) {
        var run = runForStep(step);
        if (run) {
          event.preventDefault();
          openRun(run.run_id);
        }
      }
    });

    tickHandle = window.setInterval(tick, 1000);
    poll();
    window.setInterval(poll, POLL_MS);
  }

  function poll() {
    fetch("/live.json", { cache: "no-store" })
      .then(function (response) {
        if (!response.ok) {
          throw new Error("live feed " + response.status);
        }
        return response.json();
      })
      .then(function (state) {
        live = state;
        fetchedAt = Date.now();
        ageAtFetch = state.age_seconds || 0;
        render();
      })
      .catch(function () {
        // Keep the last good state; currentAge() keeps decaying it locally.
      });
  }

  // The server age is frozen at fetch time. If polling then fails, advance it
  // locally so a stale feed cannot keep looking live forever.
  function currentAge() {
    if (!live) {
      return 0;
    }
    var drift = fetchedAt ? Math.max(0, Math.floor((Date.now() - fetchedAt) / 1000)) : 0;
    return ageAtFetch + drift;
  }

  function isStale() {
    return !live || currentAge() > STALE_AFTER;
  }

  function runs() {
    return (live && Array.isArray(live.runs)) ? live.runs : [];
  }

  function tick() {
    if (!live) {
      return;
    }
    var stale = isStale();
    Array.prototype.forEach.call(
      els.list.querySelectorAll(".run-elapsed"),
      function (node) {
        node.textContent = elapsedLabel(node.getAttribute("data-started"));
      }
    );
    if (stale) {
      els.meta.classList.add("is-stale");
    }
    if (selectedRunId) {
      var run = findRun(selectedRunId);
      if (run) {
        els.sub.textContent = runSubtitle(run);
      }
    }
  }

  function render() {
    var stale = isStale();
    var list = runs();

    els.section.hidden = false;
    els.section.classList.toggle("is-stale", stale);
    els.meta.classList.toggle("is-stale", stale);
    els.meta.textContent = metaLabel();

    // Reset every live marker before repainting the truth.
    Array.prototype.forEach.call(
      document.querySelectorAll(".step.is-running, .flow.has-live"),
      function (node) {
        node.classList.remove("is-running");
        node.classList.remove("has-live");
        if (node.tagName === "LI") {
          node.removeAttribute("role");
          node.removeAttribute("tabindex");
          node.removeAttribute("data-running");
        }
      }
    );

    els.list.textContent = "";
    if (list.length === 0) {
      els.idle.hidden = false;
      els.list.hidden = true;
    } else {
      els.idle.hidden = true;
      els.list.hidden = false;
      list.forEach(function (run) {
        els.list.appendChild(runRow(run, stale));
        markStep(run);
      });
      refreshSelected(list);
    }
  }

  function runRow(run, stale) {
    var li = document.createElement("li");
    li.className = "run" + (stale ? " is-stale" : "");
    if (run.run_id === selectedRunId) {
      li.classList.add("is-selected");
    }

    var button = document.createElement("button");
    button.type = "button";
    button.className = "run-button";
    button.setAttribute("data-run-id", run.run_id);

    var dot = document.createElement("span");
    dot.className = "run-dot";
    if (!stale) {
      dot.classList.add("is-live");
    }
    dot.setAttribute("aria-hidden", "true");

    var main = document.createElement("span");
    main.className = "run-main";
    var venture = document.createElement("span");
    venture.className = "run-venture";
    venture.textContent = ventureTitle(run.slug);
    var role = document.createElement("span");
    role.className = "run-role";
    role.textContent = run.role;
    main.appendChild(venture);
    main.appendChild(role);

    var meta = document.createElement("span");
    meta.className = "run-meta";
    var engine = document.createElement("span");
    engine.className = "run-engine";
    engine.textContent = engineLabel(run);
    var elapsed = document.createElement("span");
    elapsed.className = "run-elapsed";
    elapsed.setAttribute("data-started", run.started_at || "");
    elapsed.textContent = elapsedLabel(run.started_at);
    meta.appendChild(engine);
    meta.appendChild(elapsed);

    var cue = document.createElement("span");
    cue.className = "run-cue";
    cue.textContent = "view output";

    button.appendChild(dot);
    button.appendChild(main);
    button.appendChild(meta);
    button.appendChild(cue);
    li.appendChild(button);
    return li;
  }

  function markStep(run) {
    var article = document.querySelector(
      'article.venture[data-slug="' + cssEscape(run.slug) + '"]'
    );
    if (!article) {
      return;
    }
    var flow = article.querySelector(".flow");
    var step = article.querySelector(
      '.step[data-role="' + cssEscape(run.role) + '"]'
    );
    if (flow) {
      flow.classList.add("has-live");
    }
    if (step && !isStale()) {
      step.classList.add("is-running");
      step.setAttribute("data-running", "true");
      step.setAttribute("role", "button");
      step.setAttribute("tabindex", "0");
    }
  }

  function findRun(id) {
    var found = null;
    runs().forEach(function (run) {
      if (run.run_id === id) {
        found = run;
      }
    });
    return found;
  }

  function runForStep(step) {
    var article = step.closest("article.venture");
    if (!article) {
      return null;
    }
    var slug = article.getAttribute("data-slug");
    var role = step.getAttribute("data-role");
    var found = null;
    runs().forEach(function (run) {
      if (run.slug === slug && run.role === role) {
        found = run;
      }
    });
    return found;
  }

  function refreshSelected(list) {
    if (!selectedRunId) {
      return;
    }
    var run = findRun(selectedRunId);
    if (!run) {
      closeDrawer();
      return;
    }
    els.body.textContent = run.log_tail || "Waiting for output…";
    els.sub.textContent = runSubtitle(run);
    scrollLogToEnd();
    // Repaint the selection ring after the list was rebuilt.
    Array.prototype.forEach.call(
      els.list.querySelectorAll(".run"),
      function (li) {
        var button = li.querySelector(".run-button");
        if (button && button.getAttribute("data-run-id") === selectedRunId) {
          li.classList.add("is-selected");
        }
      }
    );
  }

  function openRun(id) {
    var run = findRun(id);
    if (!run) {
      return;
    }
    selectedRunId = id;
    els.drawer.setAttribute("aria-hidden", "false");
    els.drawer.classList.add("is-open");
    els.title.textContent = ventureTitle(run.slug) + " · " + run.role;
    els.sub.textContent = runSubtitle(run);
    els.body.textContent = run.log_tail || "Waiting for output…";
    scrollLogToEnd();
    Array.prototype.forEach.call(
      els.list.querySelectorAll(".run"),
      function (li) {
        var button = li.querySelector(".run-button");
        li.classList.toggle(
          "is-selected",
          !!button && button.getAttribute("data-run-id") === id
        );
      }
    );
  }

  function closeDrawer() {
    selectedRunId = null;
    els.drawer.classList.remove("is-open");
    els.drawer.setAttribute("aria-hidden", "true");
    Array.prototype.forEach.call(els.list.querySelectorAll(".run"), function (li) {
      li.classList.remove("is-selected");
    });
  }

  function isDrawerOpen() {
    return els.drawer.getAttribute("aria-hidden") !== "true";
  }

  function scrollLogToEnd() {
    els.body.scrollTop = els.body.scrollHeight;
  }

  function ventureTitle(slug) {
    var title = document.querySelector(
      'article.venture[data-slug="' + cssEscape(slug) + '"] .title'
    );
    return title && title.textContent ? title.textContent : slug;
  }

  function engineLabel(run) {
    var parts = [];
    if (run.engine) {
      parts.push(run.engine);
    }
    if (run.model) {
      parts.push(run.model);
    }
    if (run.leg) {
      parts.push(run.leg);
    }
    return parts.join(" · ");
  }

  function runSubtitle(run) {
    var label = engineLabel(run);
    if (isStale()) {
      return label + " · feed stale, last seen " + currentAge() + "s ago";
    }
    return label + " · live";
  }

  function metaLabel() {
    if (!live) {
      return "";
    }
    var cycle = typeof live.cycle === "number" ? "cycle " + live.cycle : "";
    if (isStale()) {
      return (cycle ? cycle + " · " : "") + "feed stale (" + currentAge() + "s)";
    }
    if (live.tick_running) {
      return (cycle ? cycle + " · " : "") + "tick running · updated " + currentAge() + "s ago";
    }
    return (cycle ? cycle + " · " : "") + "tick idle · updated " + currentAge() + "s ago";
  }

  function elapsedLabel(started) {
    if (!started) {
      return "";
    }
    var startedAt = Date.parse(started);
    if (isNaN(startedAt)) {
      return "";
    }
    var seconds = Math.max(0, Math.floor((Date.now() - startedAt) / 1000));
    return formatDuration(seconds);
  }

  function formatDuration(seconds) {
    if (seconds < 60) {
      return seconds + "s";
    }
    var minutes = Math.floor(seconds / 60);
    var rest = seconds % 60;
    if (minutes < 60) {
      return minutes + "m " + rest + "s";
    }
    var hours = Math.floor(minutes / 60);
    return hours + "h " + (minutes % 60) + "m";
  }

  // CSS.escape is not available in every browser this page may be viewed in.
  function cssEscape(value) {
    if (window.CSS && typeof window.CSS.escape === "function") {
      return window.CSS.escape(value);
    }
    return String(value).replace(/["\\]/g, "\\$&");
  }

  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", init);
  } else {
    init();
  }
})();
