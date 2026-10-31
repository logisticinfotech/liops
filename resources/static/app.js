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
  light: { axis: "#71717a", grid: "#e4e4e7", series: ["#18181b", "#0284c7", "#d97706", "#059669", "#dc2626"] },
  dark: { axis: "#a1a1aa", grid: "#27272a", series: ["#f4f4f5", "#38bdf8", "#fbbf24", "#34d399", "#f87171"] },
};

function formatValue(unit, v) {
  if (v == null) return "—";
  if (unit === "bytes") {
    const units = ["B", "KB", "MB", "GB", "TB"];
    let i = 0;
    while (Math.abs(v) >= 1024 && i < units.length - 1) { v /= 1024; i++; }
    return `${v.toFixed(v < 10 && i > 0 ? 1 : 0)} ${units[i]}`;
  }
  if (unit === "percent") return `${v.toFixed(0)}%`;
  if (unit === "ms") return v >= 1000 ? `${(v / 1000).toFixed(1)} s` : `${v.toFixed(0)} ms`;
  return `${+v.toFixed(2)}`;
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
    scales: { x: { time: true } },
    axes: [axis, { ...axis, size: 70, values: (u, vals) => vals.map((v) => formatValue(spec.unit, v)) }],
    series: [{}, ...spec.series.map((s, i) => ({
      label: s.label,
      stroke: colors.series[i % colors.series.length],
      fill: spec.series.length === 1 ? colors.series[0] + "1a" : undefined,
      width: 2,
      spanGaps: true,
      points: { show: true, size: 5 },
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
