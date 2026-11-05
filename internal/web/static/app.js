// SolarGuard Frontend JS: uPlot integration and HTMX setup

document.addEventListener("DOMContentLoaded", function () {
  // 1. Configure HTMX CSRF header from meta tag
  const csrfMeta = document.querySelector('meta[name="csrf-token"]');
  if (csrfMeta && window.htmx) {
    document.body.addEventListener('htmx:configRequest', function (evt) {
      evt.detail.headers['HX-CSRF-Token'] = csrfMeta.getAttribute('content');
    });
  }

  // Filter dropdowns submit on change. The CSP forbids inline handlers.
  document.addEventListener("change", function (evt) {
    if (evt.target.matches("select[data-autosubmit]") && evt.target.form) {
      evt.target.form.submit();
    }
  });

  // 2. Initialize uPlot chart if on plant page
  const chartEl = document.getElementById("plant-chart");
  if (chartEl && window.uPlot) {
    initPlantChart(chartEl);
  }

  // 3. Forecast chart of a report (section 5). Data is the report's own
  // stored snapshot, served in a non-executing JSON block.
  const reportChartEl = document.getElementById("report-forecast-chart");
  if (reportChartEl && window.uPlot) {
    initReportForecastChart(reportChartEl);
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

            ctx.fillStyle = "#c94343";
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

  const textColor = "#637282";
  const gridColor = "#dce4ea";

  const opts = {
    width: container.clientWidth || 800,
    height: 340,
    plugins: [notOkPlugin],
    bands: [{ series: [4, 3], fill: "rgba(22, 134, 92, 0.10)" }],
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
        stroke: "#1e67b1",
        width: 2,
        points: { show: true, size: 5 },
        spanGaps: false,
      },
      {
        label: "Esperado (kWh)",
        stroke: "#16865c",
        width: 2,
        dash: [4, 4],
        spanGaps: false,
      },
      {
        label: "Limite inferior (90%)",
        stroke: "rgba(22, 134, 92, 0.2)",
        width: 1,
      },
      {
        label: "Limite superior (90%)",
        stroke: "rgba(22, 134, 92, 0.2)",
        width: 1,
      },
      {
        label: "Previsão (kWh)",
        stroke: "#e58b20",
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
      qualStatus = `<span class="badge badge-critical" >${escapeHtml(nok.reason_label || "leitura inválida")}</span>`;
    }

    tr.innerHTML = `
      <td>${escapeHtml(formatDatePt(d))}</td>
      <td class="num">${medOK}</td>
      <td class="num">${exp}</td>
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
      <td class="num">-</td>
      <td class="num">-</td>
      <td><span class="badge badge-info">Previsão: ${val} kWh</span></td>
    `;
    tbody.appendChild(tr);
  });
}

// Forecast of a report: expected kWh per day with the 90% band when the
// report has it. Reads #report-forecast-data, never fetches anything.
function initReportForecastChart(container) {
  const dataEl = document.getElementById("report-forecast-data");
  if (!dataEl) return;

  let fc;
  try {
    fc = JSON.parse(dataEl.textContent);
  } catch (e) {
    return;
  }
  if (!fc || !Array.isArray(fc.dates) || fc.dates.length === 0 || !Array.isArray(fc.expected)) return;

  const xVals = fc.dates.map((d) => {
    const p = d.split("-").map(Number);
    return Math.floor(Date.UTC(p[0], p[1] - 1, p[2]) / 1000);
  });
  const num = (v) => (typeof v === "number" ? v : null);
  const expected = fc.expected.map(num);
  const lower = (fc.lower || []).map(num);
  const upper = (fc.upper || []).map(num);
  const hasBand = !!fc.has_band && lower.length === xVals.length && upper.length === xVals.length;

  const textColor = "#637282";
  const gridColor = "#dce4ea";
  const lineColor = "#e58b20";
  const fmt = (v) => (v == null ? "-" : v.toLocaleString("pt-BR", { minimumFractionDigits: 2, maximumFractionDigits: 2 }) + " kWh");
  const fmtDay = (ts) => {
    const d = new Date(ts * 1000);
    return String(d.getUTCDate()).padStart(2, "0") + "/" + String(d.getUTCMonth() + 1).padStart(2, "0");
  };
  const heightFor = () => (window.innerWidth < 600 ? 260 : 320);

  const data = [xVals, expected];
  const series = [
    { label: "Data", value: (u, ts) => (ts == null ? "-" : fmtDay(ts)) },
    {
      label: "Previsão (kWh/dia)",
      stroke: lineColor,
      width: 2,
      dash: [6, 3],
      points: { show: true, size: 7, fill: lineColor, stroke: "#fff", width: 1 },
      value: (u, v) => fmt(v),
    },
  ];
  const opts = {
    width: container.clientWidth || 700,
    height: heightFor(),
    scales: {
      x: {
        time: true,
        range: (u, min, max) => (min === max ? [min - 86400, max + 86400] : [min, max]),
      },
      // Zero baseline, so the size of the values is not exaggerated.
      y: { range: (u, min, max) => [0, Math.max(max * 1.1, 1)] },
    },
    axes: [
      {
        stroke: textColor,
        grid: { stroke: gridColor, width: 1 },
        ticks: { stroke: gridColor },
        // One tick per day, thinned out when the plot is narrow.
        splits: (u) => {
          const maxLabels = Math.max(2, Math.floor((u.bbox.width / (window.devicePixelRatio || 1)) / 56));
          const step = Math.ceil(xVals.length / maxLabels);
          return xVals.filter((v, i) => i % step === 0);
        },
        values: (u, splits) => splits.map(fmtDay),
        space: 50,
      },
      {
        stroke: textColor,
        grid: { stroke: gridColor, width: 1 },
        ticks: { stroke: gridColor },
        label: "kWh por dia",
        labelSize: 18,
        size: 64,
        values: (u, vals) => vals.map((v) => v.toLocaleString("pt-BR")),
      },
    ],
    series: series,
  };

  if (hasBand) {
    data.push(lower, upper);
    series.push(
      { label: "Limite inferior (90%)", stroke: "rgba(229, 139, 32, 0.35)", width: 1, value: (u, v) => fmt(v) },
      { label: "Limite superior (90%)", stroke: "rgba(229, 139, 32, 0.35)", width: 1, value: (u, v) => fmt(v) }
    );
    opts.bands = [{ series: [3, 2], fill: "rgba(229, 139, 32, 0.16)" }];
  }

  container.innerHTML = "";
  const plot = new uPlot(opts, data, container);
  window.addEventListener("resize", () => {
    plot.setSize({ width: container.clientWidth, height: heightFor() });
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
