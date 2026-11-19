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
  function fmt(sec) {
    if (sec <= 0) {
      return "imminent";
    }
    if (sec < 60) {
      return "dans " + sec + " s";
    }
    if (sec < 3600) {
      return "dans " + Math.floor(sec / 60) + " min";
    }
    return "dans " + Math.floor(sec / 3600) + " h " + ("0" + Math.floor((sec % 3600) / 60)).slice(-2);
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
