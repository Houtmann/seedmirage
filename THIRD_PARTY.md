# Tiers

| Composant | Version | Licence | Usage |
|---|---|---|---|
| htmx | 2.0.11 | BSD-2-Clause | Vendored : `internal/web/static/htmx.min.js` (npm `htmx.org@2.0.11`, `dist/htmx.min.js`) |
| htmx extension SSE | 2.2.4 | BSD-0-Clause | Vendored : `internal/web/static/sse.js` (npm `htmx-ext-sse@2.2.4`, `dist/sse.min.js`) |
| uPlot | 1.6.32 | MIT | Vendored : `internal/web/static/uplot.iife.min.js` et `uplot.min.css` (npm `uplot@1.6.32`, `dist/uPlot.iife.min.js` et `dist/uPlot.min.css`, https://registry.npmjs.org/uplot/-/uplot-1.6.32.tgz) |
| Tailwind CSS (standalone) | 4.3.3 | MIT | Outillage CSS (`tools/fetch-tailwind.sh`, sha256 dans `tools/tailwind.sha256`), non distribué dans le binaire (seul le CSS généré l'est) |
| daisyUI | 5.7.47 | MIT | Outillage CSS (`tools/daisyui/`, copie du bundle `.mjs`), non distribué (seul le CSS généré l'est) |

## Sommes sha256 des fichiers vendored

```
d6fdc75f204e6bdefa99b69bf1e6d4ac69b8a364f77929f45c13476b4000f717  internal/web/static/htmx.min.js
98a46496de0c3605fbffdce9167ba427bdd9553184f83f149c261891a92c0136  internal/web/static/sse.js
19c8d4c6ad88929a79f4ae49d6f7161566dfd0ba3d15cc495e974f787eb78f1f  internal/web/static/uplot.iife.min.js
df630c6a8d6f8eeaff264b50f73ce5b114f646ffd9a0bb74f049b0a00135fa04  internal/web/static/uplot.min.css
```

Les sommes de l'exécutable Tailwind (par OS/arch) sont dans `tools/tailwind.sha256`.
