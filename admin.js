(function () {
  "use strict";

  var REFRESH_MS = 5000;
  // The chart is a 24h view that changes slowly; polling it as often as the live
  // counters would refetch 96 buckets every 5s for nothing.
  var HISTORY_REFRESH_MS = 30000;
  var autoTimer = null;
  var autoOn = false;
  var config = null;

  function esc(v) {
    return String(v == null ? "" : v).replace(/[&<>"']/g, function (c) {
      return { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c];
    });
  }

  function num(v) { return (v || 0).toLocaleString("en-US"); }
  function pct(v) { return ((v || 0) * 100).toFixed(1) + "%"; }
  function ms(v) {
    if (!v) return "0 ms";
    if (v < 1000) return v + " ms";
    return (v / 1000).toFixed(2) + " s";
  }
  function uptime(s) {
    s = s || 0;
    var d = Math.floor(s / 86400), h = Math.floor((s % 86400) / 3600),
        m = Math.floor((s % 3600) / 60), sec = s % 60;
    if (d) return d + " 天 " + h + " 小时";
    if (h) return h + " 小时 " + m + " 分";
    if (m) return m + " 分 " + sec + " 秒";
    return sec + " 秒";
  }

  // ---- history chart ----
  //
  // Everything between here and mountHistory is pure: it takes the API payload
  // and returns strings and numbers. The page's chart is hand-rolled SVG, so the
  // geometry is arithmetic that can be checked without a browser (admin_js_test).

  var HISTORY_RANGES = [
    { value: "1h", label: "1 小时" },
    { value: "6h", label: "6 小时" },
    { value: "24h", label: "24 小时" }
  ];

  // Four ways of reading the same buckets. The first two answer "how much", the
  // last two answer "how healthy" — which is what the chart is here for.
  var HISTORY_METRICS = [
    { value: "tpm", label: "Token 速率", unit: "token/分", stacked: true },
    { value: "rpm", label: "请求速率", unit: "请求/分", stacked: true },
    { value: "failure", label: "失败率", unit: "%", stacked: false },
    { value: "latency", label: "平均延迟", unit: "ms", stacked: false }
  ];

  // Eight hues that stay apart on a white panel and under the muted grid. The
  // assignment is by series index, so a model keeps its colour across refreshes.
  var SERIES_COLORS = [
    "#2563eb", "#dc2626", "#16a34a", "#d97706",
    "#7c3aed", "#0891b2", "#db2777", "#65a30d"
  ];

  function metricByValue(value) {
    for (var i = 0; i < HISTORY_METRICS.length; i++) {
      if (HISTORY_METRICS[i].value === value) return HISTORY_METRICS[i];
    }
    return HISTORY_METRICS[0];
  }

  function colorForSeries(index) {
    return SERIES_COLORS[index % SERIES_COLORS.length];
  }

  /** Token 数缩写：1234 → "1.2K"，12345678 → "12.3M"。 */
  function formatCount(value) {
    if (!isFinite(value) || value <= 0) return "0";
    if (value >= 1e6) return (value / 1e6).toFixed(1) + "M";
    if (value >= 1e3) return (value / 1e3).toFixed(1) + "K";
    return String(Math.round(value));
  }

  /** 纵轴与提示里的数值：按口径选单位，小值保留一位小数。 */
  function formatMetric(value, metric) {
    if (!isFinite(value) || value <= 0) return metric === "failure" ? "0%" : "0";
    if (metric === "failure") return (value * 100).toFixed(value < 0.1 ? 2 : 1) + "%";
    if (metric === "latency") return value >= 1000 ? (value / 1000).toFixed(2) + " s" : Math.round(value) + " ms";
    if (value >= 1000) return formatCount(value);
    return value.toFixed(1);
  }

  function pad2(n) { return String(n).padStart(2, "0"); }

  /** 横轴刻度：桶越粗，标签越粗。 */
  function formatBucketLabel(ts, bucketSeconds) {
    var d = new Date(ts * 1000);
    if (bucketSeconds >= 86400) return pad2(d.getMonth() + 1) + "-" + pad2(d.getDate());
    if (bucketSeconds >= 3600) return pad2(d.getMonth() + 1) + "-" + pad2(d.getDate()) + " " + pad2(d.getHours()) + ":00";
    return pad2(d.getHours()) + ":" + pad2(d.getMinutes());
  }

  /** 提示里的完整时间。 */
  function formatFullTime(ts) {
    var d = new Date(ts * 1000);
    return d.getFullYear() + "-" + pad2(d.getMonth() + 1) + "-" + pad2(d.getDate()) +
      " " + pad2(d.getHours()) + ":" + pad2(d.getMinutes());
  }

  /**
   * 把一个桶的原始计数折算成所选口径的数值。
   *
   * 后端给的是桶内总量，这里按桶长归一化到「每分钟」，否则切到 24 小时视图时
   * 曲线会凭空涨 15 倍——那是桶变宽了，不是流量变了。
   */
  function metricValue(point, metric, bucketSeconds) {
    var perMinute = bucketSeconds > 0 ? 60 / bucketSeconds : 1;
    if (metric === "tpm") return (point.tokens || 0) * perMinute;
    if (metric === "rpm") return (point.requests || 0) * perMinute;
    if (metric === "failure") return point.requests > 0 ? (point.failures || 0) / point.requests : 0;
    return point.requests > 0 ? (point.latency_ms || 0) / point.requests : 0;
  }

  /**
   * 把 API 载荷整理成画图用的行。
   *
   * 返回的每一行是 `{ts, values: {模型: 数值}}`，与后端的时间轴一一对应；模型
   * 顺序就是颜色顺序，所以同一份数据每次画出来颜色都一样。
   */
  function buildChartRows(snapshot, metric, hidden) {
    var rows = [];
    if (!snapshot || !snapshot.timestamps) return rows;
    var bucket = snapshot.bucket_seconds || 60;
    var series = snapshot.models || [];
    for (var i = 0; i < snapshot.timestamps.length; i++) {
      var row = { ts: snapshot.timestamps[i], values: {} };
      for (var j = 0; j < series.length; j++) {
        var name = series[j].model;
        if (hidden && hidden[name]) continue;
        var point = (series[j].points || [])[i] || {};
        row.values[name] = metricValue(point, metric, bucket);
      }
      rows.push(row);
    }
    return rows;
  }

  /** 行内数值的最大值；堆叠口径下是各模型之和，线口径下是单条线的峰值。 */
  function chartMax(rows, names, stacked) {
    var max = 0;
    for (var i = 0; i < rows.length; i++) {
      var total = 0;
      for (var j = 0; j < names.length; j++) {
        var v = rows[i].values[names[j]] || 0;
        if (stacked) total += v;
        else if (v > total) total = v;
      }
      if (total > max) max = total;
    }
    return max;
  }

  /** 取一个好看的纵轴上限：1/2/2.5/5/10 × 10^n，刻度落在整数上。 */
  function niceCeil(value) {
    if (!isFinite(value) || value <= 0) return 1;
    var exp = Math.floor(Math.log10(value));
    var base = Math.pow(10, exp);
    var frac = value / base;
    var step = frac <= 1 ? 1 : frac <= 2 ? 2 : frac <= 2.5 ? 2.5 : frac <= 5 ? 5 : 10;
    return step * base;
  }

  /** 相邻横轴标签之间至少留出的像素，短于它就会叠在一起。 */
  var MIN_TICK_SPACING_PX = 64;

  /**
   * 横轴刻度索引：步长吸附到「能整除点数的约数」上，让刻度落在等距的时刻上。
   *
   * 不直接按 `Math.ceil(n / target)` 取步长：7、13 这种与点数无关的值会让最后
   * 一个刻度落不到时间轴末尾，标签看起来像随手挑的。取约数保证刻度等距。
   *
   * 末尾那个刻度只在离前一个够远时才补上：72 个点、步长 12 时 60 与 71 只隔
   * 13px，补上去就是两个叠在一起的时刻。
   */
  function pickTickIndices(count, target, plotWidthPx) {
    if (count <= 0) return [];
    target = Math.max(2, Math.min(target || 6, count));
    var stride = Math.max(1, Math.ceil(count / target));
    while (stride < count && count % stride !== 0) stride++;
    var out = [];
    for (var i = 0; i < count; i += stride) out.push(i);
    var last = out[out.length - 1];
    if (last < count - 1) {
      // 末尾刻度与前一个的像素距离必须容得下两个标签（各约 34px 宽）。
      // 没量到宽度时不补：宁可少一个刻度，也不赌它会重叠。
      var pxPerStep = plotWidthPx > 0 ? plotWidthPx / (count - 1) : 0;
      if ((count - 1 - last) * pxPerStep >= MIN_TICK_SPACING_PX) out.push(count - 1);
    }
    return out;
  }

  /** SVG path 的数值保留两位小数，省掉一半体积，肉眼无差别。 */
  function n2(v) { return Math.round(v * 100) / 100; }

  /**
   * 生成一条折线或堆叠带。
   *
   * `baseline` 是每个点的下沿：折线口径传 0，堆叠口径传「它下面那些模型的和」，
   * 这样同一条函数既能画线也能画带。
   */
  function buildAreaPath(rows, name, baseline, scale) {
    if (!rows.length) return "";
    var top = [], bottom = [];
    for (var i = 0; i < rows.length; i++) {
      var x = n2(scale.x(i));
      var v = (rows[i].values[name] || 0);
      var y0 = scale.y(baseline[i]);
      var y1 = scale.y(baseline[i] + v);
      top.push(x + " " + n2(y1));
      bottom.push(x + " " + n2(y0));
    }
    bottom.reverse();
    return "M" + top.join("L") + "L" + bottom.join("L") + "Z";
  }

  /** 一条不带面积的折线。 */
  function buildLinePath(rows, name, scale) {
    if (!rows.length) return "";
    var parts = [];
    for (var i = 0; i < rows.length; i++) {
      parts.push(n2(scale.x(i)) + " " + n2(scale.y(rows[i].values[name] || 0)));
    }
    return "M" + parts.join("L");
  }

  /** 堆叠口径下每个模型的基线（它下面所有可见模型之和）。 */
  function buildBaselines(rows, names) {
    var base = [];
    for (var i = 0; i < rows.length; i++) base.push(0);
    var out = [];
    for (var j = 0; j < names.length; j++) {
      out.push(base.slice());
      for (var k = 0; k < rows.length; k++) {
        base[k] += rows[k].values[names[j]] || 0;
      }
    }
    return out;
  }

  /**
   * 区间内每个模型的稳定性小结，按失败率降序。
   *
   * 这是「评估上游稳定性」的落点：图上看得见形状，但「谁在失败、失败了多少、
   * 最慢的一次多久」要在表里读。没有请求的模型不进表。
   */
  function summarizeModels(snapshot) {
    var out = [];
    if (!snapshot || !snapshot.models) return out;
    for (var i = 0; i < snapshot.models.length; i++) {
      var series = snapshot.models[i];
      var acc = { model: series.model, requests: 0, failures: 0, tokens: 0, latency: 0, maxLatency: 0 };
      var points = series.points || [];
      for (var j = 0; j < points.length; j++) {
        acc.requests += points[j].requests || 0;
        acc.failures += points[j].failures || 0;
        acc.tokens += points[j].tokens || 0;
        acc.latency += points[j].latency_ms || 0;
        if ((points[j].max_latency_ms || 0) > acc.maxLatency) acc.maxLatency = points[j].max_latency_ms;
      }
      if (acc.requests === 0) continue;
      acc.failureRate = acc.failures / acc.requests;
      acc.avgLatency = Math.round(acc.latency / acc.requests);
      out.push(acc);
    }
    out.sort(function (a, b) {
      if (a.failureRate !== b.failureRate) return b.failureRate - a.failureRate;
      return b.requests - a.requests;
    });
    return out;
  }

  // Stats errors and config-save results get separate regions: a save warning
  // (port change needs a restart, unknown keys dropped) must survive the stats
  // refresh that follows the save, and the periodic refresh after that.
  function banner(id, kind, text, dismissible) {
    var el = document.getElementById(id);
    el.className = "banner show " + kind;
    el.textContent = text;
    if (dismissible) {
      var btn = document.createElement("button");
      btn.className = "close";
      btn.type = "button";
      btn.title = "关闭";
      btn.textContent = "×";
      btn.addEventListener("click", function () { hideBanner(id); });
      el.insertBefore(btn, el.firstChild);
    }
  }
  function hideBanner(id) {
    document.getElementById(id).className = "banner";
  }

  async function api(path, opts) {
    var res = await fetch(path, opts);
    var text = await res.text();
    var data = null;
    try { data = text ? JSON.parse(text) : null; } catch (e) { data = null; }
    if (!res.ok) {
      var msg = (data && data.error) ? data.error : (text || res.statusText);
      throw new Error(msg);
    }
    return data;
  }

  // Chart state deliberately lives outside refresh(): the 5s stats poll must not
  // reset the range or metric the operator just picked, and the history request
  // is separate so a slow chart never delays the live numbers.
  var history = { range: "1h", metric: "tpm", hidden: {}, data: null, error: "" };

  function renderHistoryChips() {
    return HISTORY_RANGES.map(function (r) {
      return '<button type="button" class="chip' + (history.range === r.value ? " on" : "") +
        '" data-range="' + r.value + '">' + esc(r.label) + "</button>";
    }).join("") + '<span class="sep"></span>' + HISTORY_METRICS.map(function (m) {
      return '<button type="button" class="chip' + (history.metric === m.value ? " on" : "") +
        '" data-metric="' + m.value + '">' + esc(m.label) + "</button>";
    }).join("");
  }

  /**
   * 画整张历史速率图。
   *
   * 两条口径的形状不同：token / 请求是「量」，用堆叠带看总量与构成；失败率与
   * 延迟是「率」，堆叠没有意义（0.1% + 0.2% 不是 0.3% 的失败率），改用叠加折线。
   */
  function renderHistoryChart() {
    var host = document.getElementById("history-chart");
    var snap = history.data;
    if (!snap || !snap.timestamps || !snap.timestamps.length) {
      host.innerHTML = '<div class="empty">还没有请求经过代理，暂无速率数据。</div>';
      return;
    }

    var metric = metricByValue(history.metric);
    var names = [];
    var series = snap.models || [];
    for (var i = 0; i < series.length; i++) {
      if (!history.hidden[series[i].model]) names.push(series[i].model);
    }

    var rows = buildChartRows(snap, history.metric, history.hidden);
    var max = chartMax(rows, names, metric.stacked);
    if (max <= 0) {
      host.innerHTML = '<div class="empty">所选区间内没有流量。</div>';
      return;
    }
    // 失败率的纵轴固定 0-100%，否则一次失败就把曲线顶满，看不出「本来是 0」。
    var yMax = history.metric === "failure" ? 1 : niceCeil(max);

    // PAD_L leaves room for the y labels; PAD_B for the x labels, which sit below
    // the plot rather than inside it.
    var W = 1000, H = 240, PAD_L = 64, PAD_R = 16, PAD_T = 10, PAD_B = 28;
    var plotW = W - PAD_L - PAD_R, plotH = H - PAD_T - PAD_B;
    var scale = {
      x: function (i) { return PAD_L + (rows.length <= 1 ? plotW / 2 : (i / (rows.length - 1)) * plotW); },
      y: function (v) { return PAD_T + plotH - (Math.max(0, Math.min(v, yMax)) / yMax) * plotH; }
    };

    // preserveAspectRatio="none" is deliberate: the panel's width is fluid but the
    // chart's height is fixed, so the drawing must stretch horizontally rather
    // than keep a 1000x240 ratio. Strokes use vector-effect to stay 1px.
    var parts = ['<svg class="chart" viewBox="0 0 ' + W + " " + H + '" preserveAspectRatio="none" role="img" aria-label="历史速率图">'];
    for (var g = 0; g <= 4; g++) {
      var gv = (yMax / 4) * g;
      var gy = n2(scale.y(gv));
      parts.push('<line class="grid" x1="' + PAD_L + '" y1="' + gy + '" x2="' + (W - PAD_R) + '" y2="' + gy + '"></line>');
      // The zero label is skipped: it would sit on the axis line, right where the
      // first x tick already is.
      if (g > 0) {
        parts.push('<text class="axis" x="' + (PAD_L - 8) + '" y="' + gy +
          '" text-anchor="end" dominant-baseline="middle">' + esc(formatMetric(gv, history.metric)) + "</text>");
      }
    }

    // 横轴刻度自己抽样：点是等距的，但标签宽度不等，按容器宽度定目标个数，
    // 否则窄屏上 96 个标签会糊成一团。
    var target = Math.max(3, Math.min(15, Math.round(host.clientWidth / 90) || 6));
    var ticks = pickTickIndices(rows.length, target, plotW);
    ticks.forEach(function (idx, k) {
      var x = n2(scale.x(idx));
      // The end labels are pinned inside the viewport: centred, they would hang
      // past the edge of the panel and get clipped.
      var anchor = k === 0 ? "start" : (k === ticks.length - 1 ? "end" : "middle");
      parts.push('<text class="axis" x="' + x + '" y="' + (H - 8) + '" text-anchor="' + anchor + '">' +
        esc(formatBucketLabel(rows[idx].ts, snap.bucket_seconds)) + "</text>");
    });

    var baselines = metric.stacked ? buildBaselines(rows, names) : null;
    for (var j = 0; j < names.length; j++) {
      var color = colorForSeries(j);
      var d = metric.stacked
        ? buildAreaPath(rows, names[j], baselines[j], scale)
        : buildLinePath(rows, names[j], scale);
      var fill = metric.stacked ? ' fill="' + color + '" fill-opacity="0.35"' : ' fill="none"';
      parts.push('<path d="' + d + '" stroke="' + color + '" stroke-width="1.5"' + fill + "></path>");
    }

    // 提示用的竖线：鼠标移到哪个桶就标哪个，位置由 JS 在 mousemove 里改。
    parts.push('<line class="cursor" id="history-cursor" x1="0" y1="' + PAD_T + '" x2="0" y2="' + (PAD_T + plotH) + '" style="display:none"></line>');
    // 透明覆盖层：捕获得整块绘图区，避免每个数据点都挂一个命中区。
    parts.push('<rect id="history-hit" x="' + PAD_L + '" y="' + PAD_T + '" width="' + plotW + '" height="' + plotH + '" fill="transparent"></rect>');
    parts.push("</svg>");

    parts.push('<div class="chart-legend">' + names.map(function (name, idx) {
      return '<span class="chip on" data-show="' + esc(name) + '" title="点击隐藏">' +
        '<span class="dot" style="background:' + colorForSeries(idx) + '"></span>' + esc(name) + "</span>";
    }).join("") + (snap.models_excluded
      ? '<span class="hint">另有 ' + snap.models_excluded + " 个模型已合并为 " + esc(otherLabel()) + "</span>"
      : "") + "</div>");

    // 隐藏掉的模型留成一个可点回来的灰 chip，否则关掉之后就再也找不到了。
    var hiddenNames = (snap.models || []).filter(function (m) {
      return history.hidden[m.model];
    }).map(function (m) {
      return '<span class="chip" data-show="' + esc(m.model) + '" title="点击显示">' + esc(m.model) + "</span>";
    }).join("");
    if (hiddenNames) {
      parts.push('<div class="chart-legend">' + hiddenNames + "</div>");
    }

    if (history.error) parts.push('<div class="hint bad">' + esc(history.error) + "</div>");

    host.innerHTML = parts.join("");
    wireHistoryChart(host, snap, names, rows, scale, yMax, metric);
  }

  function otherLabel() { return "(other)"; }

  /** 鼠标交互：竖线跟随最近的桶，并弹出该桶的明细。 */
  function wireHistoryChart(host, snap, names, rows, scale, yMax, metric) {
    var svg = host.querySelector("svg.chart");
    var cursor = host.querySelector("#history-cursor");
    var tooltip = document.getElementById("history-tip");
    if (!svg || !cursor || !tooltip) return;

    function pick(clientX) {
      var box = svg.getBoundingClientRect();
      if (!box.width) return -1;
      // viewBox 是 1000 宽但 CSS 宽度随窗口变，先把客户端坐标折算回 viewBox。
      var vbX = ((clientX - box.left) / box.width) * 1000;
      var ratio = (vbX - scale.x(0)) / Math.max(1, scale.x(rows.length - 1) - scale.x(0));
      var idx = Math.round(ratio * (rows.length - 1));
      return Math.max(0, Math.min(rows.length - 1, idx));
    }

    function show(index) {
      var row = rows[index];
      if (!row) return;
      cursor.setAttribute("x1", scale.x(index));
      cursor.setAttribute("x2", scale.x(index));
      cursor.style.display = "";

      var lines = names.map(function (name) {
        return { name: name, value: row.values[name] || 0 };
      }).sort(function (a, b) { return b.value - a.value; });
      var total = lines.reduce(function (sum, l) { return sum + l.value; }, 0);

      var html = '<div class="tip-time">' + esc(formatFullTime(row.ts)) + " · " +
        esc(formatBucketLabel(row.ts, snap.bucket_seconds)) + "</div>";
      html += lines.slice(0, 8).map(function (l) {
        return '<div class="tip-row"><span>' + esc(l.name) + '</span><span class="mono">' +
          esc(formatMetric(l.value, history.metric)) + "</span></div>";
      }).join("");
      if (metric.stacked) {
        html += '<div class="tip-row tip-total"><span>合计</span><span class="mono">' +
          esc(formatMetric(total, history.metric)) + "</span></div>";
      }
      html += '<div class="tip-unit">口径：' + esc(metric.label) + "（" + esc(metric.unit) + "）</div>";
      tooltip.innerHTML = html;
      tooltip.classList.add("show");

      var box = host.getBoundingClientRect();
      var left = box.left + (scale.x(index) / 1000) * box.width + 12;
      // 贴右侧时翻到左边，避免提示被窗口裁掉。
      if (left + tooltip.offsetWidth > window.innerWidth - 8) {
        left = box.left + (scale.x(index) / 1000) * box.width - tooltip.offsetWidth - 12;
      }
      tooltip.style.left = Math.max(8, left) + "px";
      tooltip.style.top = (box.top + 8) + "px";
    }

    var hit = host.querySelector("#history-hit");
    if (hit) {
      hit.addEventListener("mousemove", function (ev) { show(pick(ev.clientX)); });
      hit.addEventListener("mouseleave", function () {
        cursor.style.display = "none";
        tooltip.classList.remove("show");
      });
    }
  }

  /** 区间内每个模型的稳定性小结，按失败率降序。图上看得见形状，表里读得清数字。 */
  function renderHistorySummary() {
    var host = document.getElementById("history-summary");
    var rows = summarizeModels(history.data);
    if (!rows.length) {
      host.innerHTML = '<div class="empty">所选区间内没有请求。</div>';
      return;
    }
    var body = rows.map(function (r) {
      var cls = r.failureRate > 0 ? (r.failureRate >= 0.1 ? "bad" : "warn") : "ok";
      return "<tr>" +
        "<td>" + esc(r.model) + "</td>" +
        '<td class="num">' + num(r.requests) + "</td>" +
        '<td class="num">' + num(r.failures) + "</td>" +
        '<td class="num ' + cls + '">' + pct(r.failureRate) + "</td>" +
        '<td class="num">' + num(r.tokens) + "</td>" +
        '<td class="num">' + ms(r.avgLatency) + "</td>" +
        '<td class="num">' + ms(r.maxLatency) + "</td>" +
        "</tr>";
    }).join("");
    host.innerHTML = '<table><thead><tr><th>模型</th>' +
      '<th class="num">请求</th><th class="num">失败</th><th class="num">失败率</th>' +
      '<th class="num">token</th><th class="num">平均延迟</th><th class="num">最大延迟</th>' +
      "</tr></thead><tbody>" + body + "</tbody></table>";
  }

  function renderHistory() {
    document.getElementById("history-controls").innerHTML = renderHistoryChips();
    renderHistoryChart();
    renderHistorySummary();
    var meta = document.getElementById("history-meta");
    var snap = history.data;
    if (snap && snap.timestamps && snap.timestamps.length) {
      var to = snap.timestamps[snap.timestamps.length - 1] + snap.bucket_seconds;
      meta.textContent = "区间 " + formatFullTime(snap.timestamps[0]) + " → " + formatFullTime(to) +
        " · 每桶 " + snap.bucket_seconds + " 秒 · 历史保留 24 小时，重启清零";
    } else {
      meta.textContent = "历史保留 24 小时，重启清零";
    }
  }

  function loadHistory() {
    var q = "/admin/api/stats/history?range=" + encodeURIComponent(history.range);
    return api(q).then(function (data) {
      history.data = data;
      history.error = "";
      renderHistory();
    }).catch(function (err) {
      history.error = "读取历史速率失败：" + err.message;
      renderHistory();
    });
  }

  function toggleHidden(name) {
    if (history.hidden[name]) delete history.hidden[name];
    else history.hidden[name] = true;
    renderHistory();
  }

  // ---- stats ----

  function sparkline(values) {
    var w = 120, h = 26, n = values.length || 1;
    var max = 0;
    for (var i = 0; i < values.length; i++) if (values[i] > max) max = values[i];
    var bw = w / n;
    var parts = ['<svg class="spark" width="' + w + '" height="' + h + '" viewBox="0 0 ' + w + ' ' + h + '" role="img" aria-label="最近 30 分钟 token 趋势">'];
    for (var j = 0; j < values.length; j++) {
      var v = values[j];
      var bh = max > 0 ? Math.max(v > 0 ? 1 : 0, Math.round((v / max) * (h - 2))) : 0;
      var x = (j * bw).toFixed(2);
      parts.push('<rect x="' + x + '" y="' + (h - bh) + '" width="' + Math.max(1, bw - 1).toFixed(2) +
                 '" height="' + bh + '" fill="var(--bar)"></rect>');
    }
    parts.push("</svg>");
    return parts.join("");
  }

  function renderTotals(stats) {
    var t = stats.totals || {};
    var cards = [
      ["总请求", num(t.requests), ""],
      ["失败请求", num(t.failures), t.failures ? "bad" : ""],
      ["TPM（最近 60 秒 token）", num(t.tpm), ""],
      ["RPM（最近 60 秒请求）", num(t.rpm), ""]
    ];
    document.getElementById("totals").innerHTML = cards.map(function (c) {
      return '<div class="card"><div class="label">' + esc(c[0]) + '</div><div class="value ' + c[2] + '">' + esc(c[1]) + "</div></div>";
    }).join("");
  }

  function renderModels(stats) {
    var models = stats.models || [];
    var host = document.getElementById("models");
    if (!models.length) {
      host.innerHTML = '<div class="empty">还没有请求经过代理。</div>';
      return;
    }
    var rows = models.map(function (m) {
      var aliases = Object.keys(m.aliases || {}).map(function (a) { return a + " ×" + m.aliases[a]; }).join("、");
      var rate = m.failure_rate || 0;
      var rateCls = rate > 0 ? (rate >= 0.1 ? "bad" : "warn") : "ok";
      return "<tr>" +
        "<td>" + esc(m.model) + (m.estimated ? ' <span class="tag" title="部分请求上游未返回 usage，token 数为按文本长度估算">估算</span>' : "") + "</td>" +
        "<td>" + esc(m.upstream || "anthropic") + "</td>" +
        '<td class="muted">' + (aliases ? esc(aliases) : "—") + "</td>" +
        '<td class="num">' + num(m.requests) + "</td>" +
        '<td class="num">' + num(m.successes) + " / " + num(m.failures) + "</td>" +
        '<td class="num ' + rateCls + '">' + pct(rate) + "</td>" +
        '<td class="num">' + num(m.tpm) + "</td>" +
        '<td class="num">' + num(m.rpm) + "</td>" +
        '<td class="num">' + num(m.input_tokens) + " / " + num(m.output_tokens) + "</td>" +
        '<td class="num">' + ms(m.avg_latency_ms) + " / " + ms(m.max_latency_ms) + "</td>" +
        "<td>" + sparkline(m.sparkline || []) + "</td>" +
        "</tr>";
    }).join("");
    host.innerHTML = '<table><thead><tr>' +
      "<th>模型</th><th>网关</th><th>别名</th>" +
      '<th class="num">请求</th><th class="num">成功 / 失败</th><th class="num">失败率</th>' +
      '<th class="num">TPM</th><th class="num">RPM</th>' +
      '<th class="num">输入 / 输出 token</th><th class="num">平均 / 最大延迟</th>' +
      "<th>最近 30 分钟</th>" +
      "</tr></thead><tbody>" + rows + "</tbody></table>";
  }

  function renderFailures(stats) {
    var models = stats.models || [];
    var host = document.getElementById("failures");

    var counts = {};
    var events = [];
    models.forEach(function (m) {
      Object.keys(m.error_counts || {}).forEach(function (k) {
        counts[k] = (counts[k] || 0) + m.error_counts[k];
      });
      (m.recent_errors || []).forEach(function (e) {
        events.push({ model: m.model, e: e });
      });
    });
    events.sort(function (a, b) { return new Date(b.e.time) - new Date(a.e.time); });

    if (!events.length) {
      host.innerHTML = '<div class="empty">没有失败记录。</div>';
      return;
    }

    var summary = Object.keys(counts).sort(function (a, b) { return counts[b] - counts[a]; })
      .map(function (k) { return '<span class="tag">' + esc(k) + " ×" + num(counts[k]) + "</span>"; }).join(" ");

    var rows = events.slice(0, 100).map(function (item) {
      var e = item.e;
      return "<tr>" +
        "<td>" + esc(new Date(e.time).toLocaleString("zh-CN")) + "</td>" +
        "<td>" + esc(item.model) + "</td>" +
        '<td class="muted">' + esc(e.alias || "—") + "</td>" +
        '<td><span class="tag">' + esc(e.category) + "</span></td>" +
        '<td class="num">' + (e.status ? esc(e.status) : "—") + "</td>" +
        '<td class="muted">' + esc(e.message || "—") + "</td>" +
        "</tr>";
    }).join("");

    host.innerHTML =
      '<div style="padding:12px 16px; border-bottom:1px solid var(--border)">' + summary + "</div>" +
      '<table><thead><tr><th>时间</th><th>模型</th><th>别名</th><th>分类</th><th class="num">状态码</th><th>消息</th></tr></thead><tbody>' +
      rows + "</tbody></table>";
  }

  async function refresh() {
    try {
      var stats = await api("/admin/api/stats");
      hideBanner("banner");
      renderTotals(stats);
      renderModels(stats);
      renderFailures(stats);
      document.getElementById("head-meta").textContent =
        "版本 " + stats.version + " · 运行 " + uptime(stats.uptime_seconds) +
        " · 更新于 " + new Date(stats.generated_at).toLocaleTimeString("zh-CN");
    } catch (err) {
      banner("banner", "err", "读取统计失败：" + err.message);
    }
  }

  function setAuto(on) {
    if (autoTimer) { clearInterval(autoTimer); autoTimer = null; }
    if (on) autoTimer = setInterval(refresh, REFRESH_MS);
    autoOn = on;
    document.getElementById("btn-auto").textContent = on ? "暂停自动刷新" : "开启自动刷新";
    document.getElementById("refresh-legend").textContent = on ? "每 5 秒自动刷新" : "自动刷新已暂停";
  }

  // ---- config ----

  function routeRow(entry) {
    var row = document.createElement("div");
    row.className = "route-row";
    row.innerHTML =
      '<div><label>别名</label><input type="text" class="r-alias"></div>' +
      '<div><label>上游模型名</label><input type="text" class="r-model"></div>' +
      '<div><label>网关</label><select class="r-upstream">' +
        '<option value="">Anthropic（默认）</option>' +
        '<option value="anthropic">anthropic（显式）</option>' +
        '<option value="openai">openai（协议翻译）</option>' +
      "</select></div>" +
      '<div class="chk"><input type="checkbox" class="r-image"><label>supports_image</label></div>' +
      '<div><button class="danger r-del">删除</button></div>';
    row.querySelector(".r-alias").value = entry.alias || "";
    row.querySelector(".r-model").value = entry.model || "";
    row.querySelector(".r-upstream").value = entry.upstream || "";
    row.querySelector(".r-image").checked = !!entry.supports_image;
    row.querySelector(".r-del").addEventListener("click", function () { row.remove(); });
    return row;
  }

  function renderConfig(cfg) {
    config = cfg;
    document.getElementById("cfg-port").value = cfg.proxy.port;
    document.getElementById("cfg-vlm-model").value = cfg.proxy.vlm_model;
    document.getElementById("cfg-vlm-max-tokens").value = cfg.proxy.vlm_max_tokens;
    document.getElementById("cfg-anthropic-url").value = cfg.upstream.anthropic_url;
    document.getElementById("cfg-openai-url").value = cfg.upstream.openai_url;
    document.getElementById("cfg-default-upstream").value = cfg.upstream.default_upstream || "";
    document.getElementById("cfg-header-timeout").value = cfg.upstream.header_timeout_seconds;
    document.getElementById("cfg-body-idle").value = cfg.upstream.body_idle_seconds;
    document.getElementById("cfg-max-retries").value = cfg.upstream.max_retries;
    document.getElementById("cfg-sophnet").value = "";
    document.getElementById("cfg-key-clear").checked = false;

    var hint = cfg.keys.sophnet_from_env
      ? "已通过环境变量 SOPHNET_API_KEY 配置（优先级高于此处）"
      : (cfg.keys.sophnet_set ? "已配置（不回显明文）" : "未配置");
    document.getElementById("cfg-key-hint").textContent = hint;

    document.getElementById("cfg-admin-token").value = "";
    document.getElementById("cfg-admin-clear").checked = false;
    document.getElementById("cfg-admin-hint").textContent = cfg.admin.token_from_env
      ? "已通过环境变量 LLM_PROXY_ADMIN_TOKEN 配置（优先级高于此处，在此修改不会生效）"
      : (cfg.admin.token_set ? "已配置（不回显明文）。修改后需用新口令重新登录。" : "未配置：管理页面当前已关闭");

    var host = document.getElementById("routes");
    host.innerHTML = "";
    (cfg.routing || []).forEach(function (e) { host.appendChild(routeRow(e)); });

    var eff = cfg.effective_routing || {};
    var names = Object.keys(eff).sort();
    document.getElementById("effective-routes").innerHTML = names.length
      ? names.map(function (alias) {
          var e = eff[alias];
          return '<span class="tag" title="' + (e.declared ? "文件中已声明" : "未声明，使用内置兜底") + '">' +
                 esc(alias) + " → " + esc(e.model) + "（" + esc(e.upstream || "anthropic") + "）" +
                 (e.declared ? "" : " · 兜底") + "</span>";
        }).join(" ")
      : '<span class="muted">—</span>';
  }

  function collectPayload() {
    var routing = [];
    document.querySelectorAll("#routes .route-row").forEach(function (row) {
      var alias = row.querySelector(".r-alias").value.trim();
      var model = row.querySelector(".r-model").value.trim();
      if (!alias && !model) return;
      routing.push({
        alias: alias,
        model: model,
        upstream: row.querySelector(".r-upstream").value,
        supports_image: row.querySelector(".r-image").checked
      });
    });

    var keyAction = "keep";
    var keyValue = "";
    if (document.getElementById("cfg-key-clear").checked) {
      keyAction = "clear";
    } else if (document.getElementById("cfg-sophnet").value !== "") {
      keyAction = "set";
      keyValue = document.getElementById("cfg-sophnet").value;
    }

    var tokenAction = "keep";
    var tokenValue = "";
    if (document.getElementById("cfg-admin-clear").checked) {
      tokenAction = "clear";
    } else if (document.getElementById("cfg-admin-token").value !== "") {
      tokenAction = "set";
      tokenValue = document.getElementById("cfg-admin-token").value;
    }

    return {
      proxy: {
        port: parseInt(document.getElementById("cfg-port").value, 10) || 0,
        vlm_model: document.getElementById("cfg-vlm-model").value.trim(),
        vlm_max_tokens: parseInt(document.getElementById("cfg-vlm-max-tokens").value, 10) || 0
      },
      upstream: {
        anthropic_url: document.getElementById("cfg-anthropic-url").value.trim(),
        openai_url: document.getElementById("cfg-openai-url").value.trim(),
        default_upstream: document.getElementById("cfg-default-upstream").value,
        header_timeout_seconds: parseInt(document.getElementById("cfg-header-timeout").value, 10) || 0,
        body_idle_seconds: parseInt(document.getElementById("cfg-body-idle").value, 10) || 0,
        max_retries: parseInt(document.getElementById("cfg-max-retries").value, 10) || 0
      },
      keys: { sophnet_action: keyAction, sophnet: keyValue },
      admin: { token_action: tokenAction, token: tokenValue },
      routing: routing
    };
  }

  async function loadConfig() {
    try {
      renderConfig(await api("/admin/api/config"));
    } catch (err) {
      banner("save-banner", "err", "读取配置失败：" + err.message, true);
    }
  }

  async function saveConfig() {
    var btn = document.getElementById("btn-save");
    btn.disabled = true;
    document.getElementById("save-hint").textContent = "保存中…";
    try {
      var res = await api("/admin/api/config", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(collectPayload())
      });
      renderConfig(res.config);
      var warns = res.warnings || [];
      if (res.reauth_required) {
        // The old password no longer authenticates, so stop polling and reload
        // to let the browser prompt for the new one.
        setAuto(false);
        banner("save-banner", "warnbox", "配置已保存。" + (warns.length ? "\n· " + warns.join("\n· ") : "") +
          "\n管理口令已更新，页面即将刷新，请用新口令登录。", true);
        setTimeout(function () { location.reload(); }, 2500);
      } else if (warns.length) {
        banner("save-banner", "warnbox", "已保存并生效。\n· " + warns.join("\n· "), true);
      } else {
        banner("save-banner", "ok", "已保存并生效。", true);
      }
      document.getElementById("save-hint").textContent = "已保存 " + new Date().toLocaleTimeString("zh-CN");
      if (!res.reauth_required) refresh();
    } catch (err) {
      banner("save-banner", "err", "保存失败：" + err.message, true);
      document.getElementById("save-hint").textContent = "";
    } finally {
      btn.disabled = false;
    }
  }

  function initAdmin() {
  document.getElementById("btn-refresh").addEventListener("click", refresh);
  document.getElementById("btn-auto").addEventListener("click", function () { setAuto(!autoTimer); });
  document.getElementById("btn-reset").addEventListener("click", async function () {
    if (!confirm("清零所有调用统计？此操作不可撤销。")) return;
    try {
      await api("/admin/api/stats/reset", { method: "POST" });
      banner("banner", "ok", "统计已清零。");
      refresh();
    } catch (err) {
      banner("banner", "err", "清零失败：" + err.message);
    }
  });
  document.getElementById("btn-add-route").addEventListener("click", function () {
    document.getElementById("routes").appendChild(routeRow({}));
  });
  document.getElementById("btn-save").addEventListener("click", saveConfig);
  document.getElementById("btn-reload").addEventListener("click", function () {
    hideBanner("save-banner");
    document.getElementById("save-hint").textContent = "";
    loadConfig();
  });
  document.getElementById("cfg-key-clear").addEventListener("change", function () {
    if (this.checked) document.getElementById("cfg-sophnet").value = "";
  });
  document.getElementById("cfg-admin-clear").addEventListener("change", function () {
    if (this.checked) document.getElementById("cfg-admin-token").value = "";
  });

  // Chips are re-rendered on every load, so the handler lives on the container.
  document.getElementById("history-controls").addEventListener("click", function (ev) {
    var chip = ev.target.closest ? ev.target.closest("button.chip") : null;
    if (!chip) return;
    if (chip.dataset.range) {
      history.range = chip.dataset.range;
      renderHistory();
      loadHistory();
      return;
    }
    if (chip.dataset.metric) {
      history.metric = chip.dataset.metric;
      renderHistory();
    }
  });
  document.getElementById("history-chart").addEventListener("click", function (ev) {
    var chip = ev.target.closest ? ev.target.closest("[data-show]") : null;
    if (chip) toggleHidden(chip.dataset.show);
  });

  setAuto(true);
  loadConfig();
  refresh();
  loadHistory();

    setInterval(function () {
      if (autoOn) loadHistory();
    }, HISTORY_REFRESH_MS);
  }

  // Node has no DOM: this file doubles as the module the chart's pure helpers are
  // tested through (admin_js_test.go), so export them instead of booting the page.
  if (typeof document === "undefined") {
    module.exports = {
      esc: esc,
      formatCount: formatCount,
      formatMetric: formatMetric,
      formatBucketLabel: formatBucketLabel,
      formatFullTime: formatFullTime,
      metricValue: metricValue,
      metricByValue: metricByValue,
      buildChartRows: buildChartRows,
      chartMax: chartMax,
      niceCeil: niceCeil,
      pickTickIndices: pickTickIndices,
      buildAreaPath: buildAreaPath,
      buildLinePath: buildLinePath,
      buildBaselines: buildBaselines,
      summarizeModels: summarizeModels,
      colorForSeries: colorForSeries,
      historyRanges: HISTORY_RANGES,
      historyMetrics: HISTORY_METRICS
    };
    return;
  }

  initAdmin();
})();
