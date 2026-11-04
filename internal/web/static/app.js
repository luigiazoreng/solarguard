// SolarGuard Frontend JS: uPlot integration and HTMX setup

document.addEventListener("DOMContentLoaded", function () {
  // 1. Configure HTMX CSRF header from meta tag
  const csrfMeta = document.querySelector('meta[name="csrf-token"]');
  if (csrfMeta && window.htmx) {
    document.body.addEventListener('htmx:configRequest', function (evt) {
      evt.detail.headers['HX-CSRF-Token'] = csrfMeta.getAttribute('content');
    });
  }

  // 2. Initialize uPlot chart if on plant page
  const chartEl = document.getElementById("plant-chart");
  if (chartEl && window.uPlot) {
    initPlantChart(chartEl);
  }
});

function initPlantChart(container) {
  const seriesUrl = container.getAttribute("data-series-url");
  if (!seriesUrl) return;

  fetch(seriesUrl)
    .then((r) => {
      if (!r.ok) throw new Error("Falha ao carregar dados da série");
      return r.json();
    })
    .then((data) => {
      renderPlot(container, data);
      populateFallbackTable(data);
    })
    .catch((err) => {
      container.innerHTML = `<div class="empty-state"><p>Erro ao carregar gráfico: ${escapeHtml(err.message)}</p></div>`;
    });
}

function renderPlot(container, data) {
  container.innerHTML = "";

  const allDates = [...data.dates, ...(data.forecast_dates || [])];
  if (allDates.length === 0) {
    container.innerHTML = `<div class="empty-state"><p>Nenhum dado disponível para o período selecionado.</p></div>`;
    return;
  }

  // Convert dates YYYY-MM-DD to unix epoch seconds (UTC midnight)
  const xVals = allDates.map((d) => {
    const parts = d.split("-").map(Number);
    return Math.floor(Date.UTC(parts[0], parts[1] - 1, parts[2]) / 1000);
  });

  const numHist = data.dates.length;
  const numFore = (data.forecast_dates || []).length;

  // Series 1: Measured OK (null for forecast days and not-ok days)
  const measuredOK = [...(data.measured_ok || []), ...Array(numFore).fill(null)];

  // Series 2: Expected (past observed)
  const expected = [...(data.expected || []), ...Array(numFore).fill(null)];

  // Series 3: Lower Band
  const bandLower = [...(data.band_lower || []), ...Array(numFore).fill(null)];

  // Series 4: Upper Band
  const bandUpper = [...(data.band_upper || []), ...Array(numFore).fill(null)];

  // Series 5: Forecast (future)
  const forecast = [...Array(numHist).fill(null), ...(data.forecast_values || [])];

  const plotData = [xVals, measuredOK, expected, bandLower, bandUpper, forecast];

  // Map of not-ok readings by timestamp
  const notOkMap = {};
  (data.measured_not_ok || []).forEach((nok) => {
    const parts = nok.date.split("-").map(Number);
    const ts = Math.floor(Date.UTC(parts[0], parts[1] - 1, parts[2]) / 1000);
    notOkMap[ts] = nok;
  });

  // Custom plugin to draw markers for not-ok days
  const notOkPlugin = {
    hooks: {
      draw: (u) => {
        const ctx = u.ctx;
        ctx.save();
        xVals.forEach((ts, idx) => {
          if (notOkMap[ts]) {
            const cx = u.valToPos(ts, "x", true);
            const cy = u.bbox.top + u.bbox.height - 10;

            ctx.fillStyle = "#dc2626";
            ctx.beginPath();
            ctx.arc(cx, cy, 5, 0, 2 * Math.PI);
            ctx.fill();

            // Exclamation point
            ctx.fillStyle = "#ffffff";
            ctx.font = "bold 9px sans-serif";
            ctx.textAlign = "center";
            ctx.textBaseline = "middle";
            ctx.fillText("!", cx, cy);
          }
        });
        ctx.restore();
      },
    },
  };

  const isDark = window.matchMedia && window.matchMedia("(prefers-color-scheme: dark)").matches;
  const textColor = isDark ? "#94a3b8" : "#64748b";
  const gridColor = isDark ? "#1e293b" : "#e2e8f0";

  const opts = {
    width: container.clientWidth || 800,
    height: 340,
    plugins: [notOkPlugin],
    scales: {
      x: { time: true },
      y: { auto: true },
    },
    axes: [
      {
        scale: "x",
        stroke: textColor,
        grid: { stroke: gridColor, width: 1 },
        space: 70,
        gap: 8,
        splits: (u, axisIdx, min, max) => {
          const width = u.bbox.width || 800;
          const space = 70;
          const maxLabels = Math.max(2, Math.floor(width / space));
          const inRange = xVals.filter((v) => v >= min && v <= max);
          if (inRange.length <= maxLabels) return inRange;
          const step = Math.ceil(inRange.length / maxLabels);
          const s = [];
          for (let i = 0; i < inRange.length; i += step) {
            s.push(inRange[i]);
          }
          return s;
        },
        values: (u, splits) => splits.map((ts) => {
          const d = new Date(ts * 1000);
          const day = String(d.getUTCDate()).padStart(2, "0");
          const month = String(d.getUTCMonth() + 1).padStart(2, "0");
          return `${day}/${month}`;
        }),
      },
      {
        scale: "y",
        stroke: textColor,
        grid: { stroke: gridColor, width: 1 },
        values: (u, vals) => vals.map((v) => v.toLocaleString("pt-BR")),
      },
    ],
    series: [
      { label: "Data" },
      {
        label: "Medido (kWh)",
        stroke: "#2563eb",
        width: 2,
        points: { show: true, size: 5 },
        spanGaps: false,
      },
      {
        label: "Esperado (kWh)",
        stroke: "#16a34a",
        width: 2,
        dash: [4, 4],
        spanGaps: false,
      },
      {
        label: "Banda Inf (90%)",
        stroke: "rgba(22, 163, 74, 0.2)",
        width: 1,
      },
      {
        label: "Banda Sup (90%)",
        stroke: "rgba(22, 163, 74, 0.2)",
        width: 1,
        fill: "rgba(22, 163, 74, 0.08)",
        band: true,
      },
      {
        label: "Previsão (kWh)",
        stroke: "#d97706",
        width: 2,
        dash: [6, 3],
        points: { show: true, size: 4 },
        spanGaps: false,
      },
    ],
  };

  const plot = new uPlot(opts, plotData, container);

  window.addEventListener("resize", () => {
    plot.setSize({ width: container.clientWidth, height: 340 });
  });
}

function populateFallbackTable(data) {
  const tbody = document.getElementById("fallback-table-body");
  if (!tbody) return;

  tbody.innerHTML = "";

  const notOkMap = {};
  (data.measured_not_ok || []).forEach((nok) => {
    notOkMap[nok.date] = nok;
  });

  (data.dates || []).forEach((d, idx) => {
    const tr = document.createElement("tr");

    const medOK = data.measured_ok && data.measured_ok[idx] !== null && data.measured_ok[idx] !== undefined
      ? Number(data.measured_ok[idx]).toLocaleString("pt-BR", { minimumFractionDigits: 2, maximumFractionDigits: 2 })
      : "-";

    const exp = data.expected && data.expected[idx] !== null && data.expected[idx] !== undefined
      ? Number(data.expected[idx]).toLocaleString("pt-BR", { minimumFractionDigits: 2, maximumFractionDigits: 2 })
      : "-";

    let qualStatus = `<span class="badge badge-success">OK</span>`;
    if (notOkMap[d]) {
      const nok = notOkMap[d];
      qualStatus = `<span class="badge badge-warning" title="${escapeHtml(nok.reason)}">${escapeHtml(nok.reason || "não ok")}</span>`;
    }

    tr.innerHTML = `
      <td>${escapeHtml(formatDatePt(d))}</td>
      <td>${medOK}</td>
      <td>${exp}</td>
      <td>${qualStatus}</td>
    `;
    tbody.appendChild(tr);
  });

  (data.forecast_dates || []).forEach((d, idx) => {
    const tr = document.createElement("tr");
    const val = data.forecast_values && data.forecast_values[idx] !== null && data.forecast_values[idx] !== undefined
      ? Number(data.forecast_values[idx]).toLocaleString("pt-BR", { minimumFractionDigits: 2, maximumFractionDigits: 2 })
      : "-";

    tr.innerHTML = `
      <td>${escapeHtml(formatDatePt(d))}</td>
      <td>-</td>
      <td>-</td>
      <td><span class="badge badge-info">Previsão: ${val} kWh</span></td>
    `;
    tbody.appendChild(tr);
  });
}

function formatDatePt(d) {
  if (!d || d.length < 10) return d;
  const p = d.split("-");
  return `${p[2]}/${p[1]}/${p[0]}`;
}

function escapeHtml(str) {
  if (!str) return "";
  return String(str)
    .replace(/&/g, "&amp;")
    .replace(/</g, "&lt;")
    .replace(/>/g, "&gt;")
    .replace(/"/g, "&quot;")
    .replace(/'/g, "&#039;");
}
