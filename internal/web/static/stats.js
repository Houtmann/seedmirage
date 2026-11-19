// Page /stats : graphiques uPlot (upload par heure, vitesse globale en direct). Aucun script inline (CSP stricte).
(function () {
  "use strict";
  var root = document.getElementById("stats");
  if (!root || !window.uPlot) {
    return;
  }
  var WINDOW = 600; // secondes de l'historique de vitesse
  var HEIGHT = 220;
  // Unités, séparateur décimal et libellés rendus par le serveur (data-*) dans la langue de la page.
  var UNITS = (root.getAttribute("data-units") || "B,KiB,MiB,GiB,TiB").split(",");
  var DECIMAL = root.getAttribute("data-decimal") || ".";
  var LABEL_UPLOAD = root.getAttribute("data-label-upload") || "Upload";
  var LABEL_SPEED = root.getAttribute("data-label-speed") || "Speed";
  var charts = { hours: null, speed: null };
  var last = null; // dernières données reçues
  var range = "24h";

  function fmtBytes(v) {
    if (v == null) {
      return "";
    }
    var units = UNITS;
    var i = 0;
    var x = Math.abs(v);
    while (x >= 1024 && i < units.length - 1) {
      x /= 1024;
      i++;
    }
    var s = x >= 100 || i === 0 ? Math.round(x).toString() : x.toFixed(1).replace(".", DECIMAL);
    return s + " " + units[i];
  }

  // Couleurs : variables daisyUI du thème courant (clair ou sombre), résolues par le navigateur.
  function color(name, fallback) {
    var probe = document.createElement("span");
    probe.style.color = "var(" + name + ", " + fallback + ")";
    probe.style.display = "none";
    document.body.appendChild(probe);
    var c = getComputedStyle(probe).color;
    document.body.removeChild(probe);
    return c || fallback;
  }

  function palette() {
    return {
      text: color("--color-base-content", "#888"),
      grid: color("--color-base-300", "#ddd"),
      line: color("--color-primary", "#3b82f6"),
    };
  }

  function width(el) {
    return Math.max(el.clientWidth, 200);
  }

  function axes(p, unit) {
    var common = { stroke: p.text, grid: { stroke: p.grid, width: 1 }, ticks: { stroke: p.grid, width: 1 }, font: "11px sans-serif" };
    return [
      Object.assign({}, common, {
        space: 90,
        // Dates et heures dans la langue de la page (<html lang>), pas dans celle d'uPlot.
        values: function (u, splits, axisIdx, foundSpace, foundIncr) {
          var opts = { hour: "2-digit", minute: "2-digit" };
          if (foundIncr >= 86400) {
            opts = { day: "2-digit", month: "2-digit" };
          } else if (foundIncr < 60) {
            opts.second = "2-digit";
          }
          var f;
          try {
            f = new Intl.DateTimeFormat(document.documentElement.lang || undefined, opts);
          } catch (e) {
            return splits.map(String);
          }
          return splits.map(function (t) {
            return f.format(new Date(t * 1000));
          });
        },
      }),
      Object.assign({}, common, {
        size: 76,
        values: function (u, vals) {
          return vals.map(function (v) {
            return fmtBytes(v) + unit;
          });
        },
      }),
    ];
  }

  function build(key, el, data, bars, unit) {
    if (charts[key]) {
      charts[key].destroy();
      charts[key] = null;
    }
    var p = palette();
    var s = {
      label: unit ? LABEL_SPEED : LABEL_UPLOAD,
      stroke: p.line,
      width: 2,
      fill: bars ? p.line : undefined,
      value: function (u, v) {
        return v == null ? "—" : fmtBytes(v) + unit;
      },
    };
    if (bars) {
      s.paths = window.uPlot.paths.bars({ size: [0.8, 64] });
      s.width = 0;
    }
    charts[key] = new window.uPlot(
      {
        width: width(el),
        height: HEIGHT,
        legend: { show: false },
        cursor: { drag: { x: false, y: false } },
        scales: { x: { time: true }, y: { range: function (u, min, max) { return [0, Math.max(max, 1) * 1.1]; } } },
        axes: axes(p, unit),
        series: [{}, s],
      },
      data,
      el
    );
  }

  function render() {
    if (!last) {
      return;
    }
    var h = last.hours;
    var total = 0;
    for (var i = 0; i < h.v.length; i++) {
      total += h.v[i];
    }
    document.getElementById("hours-empty").classList.toggle("hidden", total > 0);
    build("hours", document.getElementById("chart-hours"), [h.t, h.v], true, "");
    renderSpeed();
  }

  function renderSpeed() {
    var sp = last.speed;
    var now = Math.floor(Date.now() / 1000);
    var t = sp.t.slice();
    var v = sp.v.slice();
    if (t.length === 0) {
      t = [now - WINDOW, now];
      v = [null, null];
    }
    build("speed", document.getElementById("chart-speed"), [t, v], false, "/s");
  }

  function load() {
    fetch("/stats/data.json?range=" + encodeURIComponent(range), { credentials: "same-origin", headers: { Accept: "application/json" } })
      .then(function (r) {
        if (!r.ok) {
          throw new Error("HTTP " + r.status);
        }
        return r.json();
      })
      .then(function (d) {
        document.getElementById("stats-error").classList.add("hidden");
        last = d;
        render();
      })
      .catch(function () {
        document.getElementById("stats-error").classList.remove("hidden");
      });
  }

  // Point de vitesse envoyé par SSE (« secondes,octets/s »), au plus un toutes les 5 s.
  document.addEventListener("htmx:sseMessage", function (ev) {
    var m = ev.detail;
    if (!last || !m || m.type !== "speed.point" || typeof m.data !== "string") {
      return;
    }
    var parts = m.data.split(",");
    var t = Number(parts[0]);
    var v = Number(parts[1]);
    if (!isFinite(t) || !isFinite(v)) {
      return;
    }
    var sp = last.speed;
    if (sp.t.length && sp.t[sp.t.length - 1] >= t) {
      return;
    }
    sp.t.push(t);
    sp.v.push(v);
    while (sp.t.length && sp.t[0] < t - WINDOW) {
      sp.t.shift();
      sp.v.shift();
    }
    if (charts.speed) {
      charts.speed.setData([sp.t, sp.v]);
    }
  });

  root.querySelectorAll("[data-range]").forEach(function (btn) {
    btn.addEventListener("click", function () {
      range = btn.getAttribute("data-range");
      root.querySelectorAll("[data-range]").forEach(function (b) {
        var on = b === btn;
        b.classList.toggle("btn-primary", on);
        b.setAttribute("aria-pressed", on ? "true" : "false");
      });
      load();
    });
  });

  // Largeur : suit le conteneur ; thème : couleurs relues au changement.
  var rt;
  window.addEventListener("resize", function () {
    clearTimeout(rt);
    rt = setTimeout(function () {
      if (charts.hours) {
        charts.hours.setSize({ width: width(document.getElementById("chart-hours")), height: HEIGHT });
      }
      if (charts.speed) {
        charts.speed.setSize({ width: width(document.getElementById("chart-speed")), height: HEIGHT });
      }
    }, 100);
  });
  new MutationObserver(render).observe(document.documentElement, { attributes: true, attributeFilter: ["data-theme"] });
  if (window.matchMedia) {
    var mq = window.matchMedia("(prefers-color-scheme: dark)");
    if (mq.addEventListener) {
      mq.addEventListener("change", render);
    }
  }
  load();
})();
