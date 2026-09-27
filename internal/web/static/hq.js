(function () {
  "use strict";
  var page = document.body.getAttribute("data-page");

  if (page === "login") {
    var status = document.getElementById("hq-login-status");
    var match = /(?:^#|&)t=([A-Za-z0-9_-]+)/.exec(location.hash);
    history.replaceState(null, "", location.pathname);
    if (!match) {
      status.textContent = "This link has no token. Send /web to the Telegram bot for a new one.";
      return;
    }
    fetch("/hq/login", {
      method: "POST", credentials: "same-origin",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ token: match[1] })
    }).then(function (response) {
      if (response.status === 204) {
        location.replace("/hq");
      } else {
        status.textContent = "This link expired or was already used. Send /web to the Telegram bot for a new one.";
      }
    });
    return;
  }

  if (page !== "hq") {
    return;
  }
  var connection = document.getElementById("hq-connection");
  var output = document.getElementById("hq-state");
  var state = null;
  function render() {
    output.textContent = JSON.stringify(state, null, 2);
  }
  function connect() {
    var socket = new WebSocket((location.protocol === "https:" ? "wss://" : "ws://") + location.host + "/hq/ws");
    socket.onopen = function () { connection.textContent = "live"; };
    socket.onclose = function (event) {
      if (event.code === 4001) { location.reload(); return; }
      connection.textContent = "reconnecting…";
      setTimeout(connect, 3000);
    };
    socket.onmessage = function (event) {
      var message = JSON.parse(event.data);
      if (message.type === "state") { state = message; }
      else if (state && message.type === "live") { state.live = message.live; }
      else if (state && message.type === "ventures") { state.ventures = message.ventures; state.status = message.status; }
      else if (state && message.type === "hq") { state.hq = message.hq; }
      else if (state && message.type === "bridge") { state.bridge = message.bridge; }
      else if (state && message.type === "command") {
        state.commands = [message.command].concat(state.commands.filter(function (c) { return c.id !== message.command.id; }));
      }
      render();
    };
  }
  connect();
})();
