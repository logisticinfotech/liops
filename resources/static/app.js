// SPDX-License-Identifier: Apache-2.0
// Small progressive enhancements. The UI works without JavaScript, except the charts.
document.addEventListener("submit", (e) => {
  const msg = e.submitter?.dataset.confirm || e.target.dataset.confirm;
  if (msg && !window.confirm(msg)) e.preventDefault();
});

document.addEventListener("click", (e) => {
  const copy = e.target.closest("[data-copy]");
  if (copy) {
    navigator.clipboard?.writeText(copy.dataset.copy).then(() => {
      const old = copy.textContent;
      copy.textContent = "Copied";
      setTimeout(() => (copy.textContent = old), 1500);
    });
  }
  if (e.target.closest("[data-back]")) history.back();
  // The sidebar is a slide-in menu on small screens.
  if (e.target.closest("[data-nav-toggle]") || e.target.closest("[data-nav-close]")) {
    document.getElementById("sidebar")?.classList.toggle("open");
    document.querySelector(".backdrop")?.classList.toggle("open");
  }
  // Theme: follow the system → light → dark → follow the system.
  if (e.target.closest("[data-theme-toggle]")) {
    let choice = null;
    try { choice = localStorage.getItem("theme"); } catch (err) {}
    const next = choice === null ? "light" : choice === "light" ? "dark" : null;
    try { next ? localStorage.setItem("theme", next) : localStorage.removeItem("theme"); } catch (err) {}
    applyTheme();
  }
});

function applyTheme() {
  let choice = null;
  try { choice = localStorage.getItem("theme"); } catch (e) {}
  const dark = choice === "dark" || (choice !== "light" && matchMedia("(prefers-color-scheme: dark)").matches);
  document.documentElement.classList.toggle("dark", dark);
  const label = choice === null ? "Theme: follows the system" : `Theme: ${choice}`;
  document.querySelectorAll("[data-theme-toggle]").forEach((b) => { b.title = label; b.setAttribute("aria-label", label); });
}
matchMedia("(prefers-color-scheme: dark)").addEventListener("change", applyTheme);
document.addEventListener("DOMContentLoaded", applyTheme);

// Charts: <div class="chart" data-chart='{"unit":"bytes","x":[unix…],"series":[{"label":…,"values":[…]}]}'>.
const palette = {
  light: { axis: "#71717a", grid: "#e4e4e7", marker: "rgba(113, 113, 122, 0.12)", series: ["#18181b", "#0284c7", "#d97706", "#059669", "#dc2626"] },
  dark: { axis: "#a1a1aa", grid: "#27272a", marker: "rgba(161, 161, 170, 0.11)", series: ["#f4f4f5", "#38bdf8", "#fbbf24", "#34d399", "#f87171"] },
};

function formatValue(unit, v) {
  if (v == null) return "—";
  if (unit === "bytes") {
    const units = ["B", "KB", "MB", "GB", "TB"];
    let i = 0;
    // Divide at 1000, not 1024, so no label needs four digits ("1000 MB" → "1.0 GB").
    while (Math.abs(v) >= 1000 && i < units.length - 1) { v /= 1024; i++; }
    return `${v.toFixed(v < 10 && i > 0 ? 1 : 0)} ${units[i]}`;
  }
  if (unit === "bytes/s") return `${formatValue("bytes", v)}/s`;
  if (unit === "percent") return `${v.toFixed(0)}%`;
  if (unit === "ms") return v >= 1000 ? `${(v / 1000).toFixed(1)} s` : `${v.toFixed(0)} ms`;
  return `${+v.toFixed(2)}`;
}

// Every value liops charts is zero or more, so the y-axis starts at 0 (a 37→40% change
// shouldn't fill the chart); percentages always show 0–100.
function yRange(unit) {
  return (u, min, max) => [0, unit === "percent" ? 100 : max > 0 ? max * 1.1 : 1];
}

// Byte axes step in powers of 1024, to match how formatValue prints them.
const byteIncrs = [0, 1, 2, 3, 4].flatMap((k) => [1, 2, 5, 10, 25, 50, 100, 250, 500].map((n) => n * 1024 ** k));

// Bands where liops's own jobs ran, behind the lines; labelled when few are in view.
function drawMarkers(u, markers, colors) {
  if (!markers) return;
  const { ctx, bbox } = u;
  const dpr = devicePixelRatio || 1;
  const shown = markers.filter((m) => m.end >= u.scales.x.min && m.start <= u.scales.x.max);
  ctx.save();
  ctx.beginPath();
  ctx.rect(bbox.left, bbox.top, bbox.width, bbox.height);
  ctx.clip();
  ctx.font = `${11 * dpr}px ui-sans-serif, system-ui, sans-serif`;
  for (const m of shown) {
    const x0 = u.valToPos(m.start, "x", true);
    const x1 = u.valToPos(m.end, "x", true);
    ctx.fillStyle = colors.marker;
    ctx.fillRect(x0, bbox.top, Math.max(2 * dpr, x1 - x0), bbox.height);
    if (shown.length <= 8) {
      ctx.fillStyle = colors.axis;
      // A job that started before the chart's first point is labelled at the left edge.
      ctx.fillText(m.label, Math.max(x0, bbox.left) + 4 * dpr, bbox.top + 14 * dpr);
    }
  }
  ctx.restore();
}

function drawChart(el) {
  if (!window.uPlot) return;
  const spec = JSON.parse(el.dataset.chart);
  const colors = palette[document.documentElement.classList.contains("dark") ? "dark" : "light"];
  const axis = { stroke: colors.axis, grid: { stroke: colors.grid, width: 1 }, ticks: { stroke: colors.grid, width: 1 } };
  const pad = getComputedStyle(el);
  const opts = {
    // The box's width inside its padding: the chart must never be wider than its box.
    width: Math.max(200, el.clientWidth - parseFloat(pad.paddingLeft) - parseFloat(pad.paddingRight)),
    height: +(el.dataset.height || 220),
    cursor: { points: { size: 6 } },
    hooks: { drawClear: [(u) => drawMarkers(u, spec.markers, colors)] },
    scales: { x: { time: true }, y: { range: yRange(spec.unit) } },
    axes: [axis, {
      ...axis, size: 70,
      incrs: spec.unit.startsWith("bytes") ? byteIncrs : undefined,
      values: (u, vals) => vals.map((v) => formatValue(spec.unit, v)),
    }],
    series: [{}, ...spec.series.map((s, i) => ({
      label: s.label,
      stroke: colors.series[i % colors.series.length],
      fill: spec.series.length === 1 ? colors.series[0] + "1a" : undefined,
      width: 2,
      spanGaps: true,
      points: { size: 5 }, // uPlot hides them when they'd crowd (a day of 15-second samples)
      value: (u, v) => formatValue(spec.unit, v),
    }))],
  };
  el.querySelector(".uplot")?.remove();
  el._plot?.destroy();
  el._plot = new uPlot(opts, [spec.x, ...spec.series.map((s) => s.values)], el);
}

function drawCharts() { document.querySelectorAll(".chart[data-chart]").forEach(drawChart); }
document.addEventListener("DOMContentLoaded", () => {
  drawCharts();
  new MutationObserver(drawCharts).observe(document.documentElement, { attributes: true, attributeFilter: ["class"] });
  let pending;
  addEventListener("resize", () => { clearTimeout(pending); pending = setTimeout(drawCharts, 150); });
});
