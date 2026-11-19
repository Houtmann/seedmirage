// Tableau de bord : lignes cliquables, compte à rebours des annonces, toasts auto-effacés (aucun script inline : CSP stricte).
(function () {
  "use strict";
  document.addEventListener("click", function (ev) {
    if (!(ev.target instanceof Element) || ev.target.closest("a, button")) {
      return;
    }
    var row = ev.target.closest("tr[data-href]");
    if (row) {
      window.location.href = row.getAttribute("data-href");
    }
  });
  // Textes rendus par le serveur dans la langue de la page : <body data-t-in="in %s" data-t-imminent="imminent">.
  function text(name, fallback) {
    var v = document.body ? document.body.getAttribute(name) : null;
    return v || fallback;
  }
  function fmt(sec) {
    if (sec <= 0) {
      return text("data-t-imminent", "imminent");
    }
    var d;
    if (sec < 60) {
      d = sec + " s";
    } else if (sec < 3600) {
      d = Math.floor(sec / 60) + " min";
    } else {
      d = Math.floor(sec / 3600) + " h " + ("0" + Math.floor((sec % 3600) / 60)).slice(-2);
    }
    return text("data-t-in", "in %s").replace("%s", d);
  }
  setInterval(function () {
    var now = Date.now() / 1000;
    document.querySelectorAll("time[data-next]").forEach(function (el) {
      el.textContent = fmt(Math.ceil(Number(el.getAttribute("data-next")) - now));
    });
  }, 1000);
  document.addEventListener("DOMContentLoaded", function () {
    var box = document.getElementById("toasts");
    if (!box) {
      return;
    }
    new MutationObserver(function (muts) {
      muts.forEach(function (m) {
        m.addedNodes.forEach(function (n) {
          if (n instanceof Element) {
            while (box.children.length > 5) {
              box.removeChild(box.firstElementChild);
            }
            setTimeout(function () {
              n.remove();
            }, 8000);
          }
        });
      });
    }).observe(box, { childList: true });
  });
})();
