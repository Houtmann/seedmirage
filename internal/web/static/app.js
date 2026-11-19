// JS minimal de seedmirage : bascule de thème clair/sombre (aucun script inline : CSP stricte).
(function () {
  "use strict";
  var KEY = "sm-theme";
  var root = document.documentElement;
  try {
    var saved = localStorage.getItem(KEY);
    if (saved === "light" || saved === "dark") {
      root.setAttribute("data-theme", saved);
    }
  } catch (e) {
    /* stockage indisponible : on garde le thème du système */
  }
  // Planning horaire (réglages) : la valeur affichée suit le curseur, et un
  // préréglage remplit les 24 curseurs. Écouteurs délégués : le formulaire est
  // remplacé par htmx après chaque enregistrement.
  document.addEventListener("input", function (ev) {
    var el = ev.target;
    if (el && el.matches && el.matches("input[data-hour]")) {
      var out = el.parentNode.querySelector("output");
      if (out) {
        out.textContent = el.value;
      }
    }
  });
  document.addEventListener("click", function (ev) {
    var btn = ev.target && ev.target.closest ? ev.target.closest("[data-hourly-preset]") : null;
    if (!btn) {
      return;
    }
    var values = btn.getAttribute("data-hourly-preset").split(",");
    var sliders = btn.closest("div.flex-col").querySelectorAll("input[data-hour]");
    sliders.forEach(function (s, i) {
      if (i < values.length) {
        s.value = values[i];
        var out = s.parentNode.querySelector("output");
        if (out) {
          out.textContent = values[i];
        }
      }
    });
  });
  document.addEventListener("DOMContentLoaded", function () {
    var btn = document.querySelector("[data-theme-toggle]");
    if (!btn) {
      return;
    }
    btn.addEventListener("click", function () {
      var cur = root.getAttribute("data-theme");
      if (!cur) {
        cur = window.matchMedia("(prefers-color-scheme: dark)").matches ? "dark" : "light";
      }
      var next = cur === "dark" ? "light" : "dark";
      root.setAttribute("data-theme", next);
      try {
        localStorage.setItem(KEY, next);
      } catch (e) {
        /* ignoré */
      }
    });
  });
})();
