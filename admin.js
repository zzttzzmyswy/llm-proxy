(function () {
  "use strict";

  var REFRESH_MS = 5000;
  // The chart is a 24h view that changes slowly; polling it as often as the live
  // counters would refetch 96 buckets every 5s for nothing.
  var HISTORY_REFRESH_MS = 30000;
  var autoTimer = null;
  var autoOn = false;
  var config = null;
  // schemeView is the scheme currently loaded in the editor, which may or may not
  // be the running one.
  var schemeView = null;
  // routingEntries is the whole model table, including the rows on other pages, so
  // a save never drops what the operator cannot see.
  var routingEntries = [];

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
    { value: "cache", label: "缓存命中率", unit: "%", stacked: false },
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
    if (!isFinite(value) || value <= 0) return metric === "failure" || metric === "cache" ? "0%" : "0";
    if (metric === "failure" || metric === "cache") {
      return (value * 100).toFixed(value < 0.1 ? 2 : 1) + "%";
    }
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
    if (metric === "cache") return cacheHitRate(point);
    return point.requests > 0 ? (point.latency_ms || 0) / point.requests : 0;
  }

  /**
   * 缓存命中率 = 命中缓存的 prompt token / 全部 prompt token。
   *
   * 分母必须带上写缓存的那部分：只写不读的请求一个 token 都没命中，把它算成
   * 命中会让曲线虚高。也不能只除以未命中的部分，那样一个缓存良好的请求会算出
   * 超过 100%。桶里没有 prompt token 时返回 0，不做除零。
   *
   * 拿不到原始计数时回落到后端算好的 cache_hit_rate，这样页面在旧版本代理上
   * 也不会整块消失。
   */
  function cacheHitRate(point) {
    var read = point.cache_read || 0;
    var written = point.cache_creation || 0;
    var fresh = point.input || 0;
    var prompt = read + written + fresh;
    if (prompt <= 0) {
      return point.cache_hit_rate || 0;
    }
    return read / prompt;
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
      var acc = {
        model: series.model,
        requests: 0,
        failures: 0,
        tokens: 0,
        latency: 0,
        maxLatency: 0,
        cacheRead: series.cache_read || 0,
        cacheCreation: series.cache_creation || 0,
        cacheHitRate: series.cache_hit_rate || 0,
      };
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
    // 失败率与命中率的纵轴都固定 0-100%：自适应的话一次失败就把曲线顶满，而
    // 命中率永远在 0-1 之间，自适应只会把「80% 还是 90%」这点差别放大成噪声。
    var percentAxis = history.metric === "failure" || history.metric === "cache";
    var yMax = percentAxis ? 1 : niceCeil(max);

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
      if (history.metric === "cache") {
        // 命中率的分母是 prompt token，不是各模型命中率之和，所以这里另给一行
        // 区间合计，按 token 加权。
        var point = function (name) { return (snap.models.filter(function (m) {
          return m.model === name;
        })[0] || {}).points || []; };
        var read = 0, written = 0, fresh = 0;
        names.forEach(function (name) {
          var p = point(name)[index] || {};
          read += p.cache_read || 0;
          written += p.cache_creation || 0;
          fresh += p.input || 0;
        });
        var prompt = read + written + fresh;
        html += '<div class="tip-row tip-total"><span>命中 token</span><span class="mono">' +
          esc(formatCount(read)) + " / " + esc(formatCount(prompt)) + "</span></div>";
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
        '<td class="num" title="命中缓存 / 写缓存 / 未命中（prompt token）">' +
          pct(r.cacheHitRate) + " <span class=\"muted\">" +
          formatCount(r.cacheRead) + " / " + formatCount(r.cacheCreation) + "</span></td>" +
        '<td class="num">' + ms(r.avgLatency) + "</td>" +
        '<td class="num">' + ms(r.maxLatency) + "</td>" +
        "</tr>";
    }).join("");
    host.innerHTML = '<table><thead><tr><th>模型</th>' +
      '<th class="num">请求</th><th class="num">失败</th><th class="num">失败率</th>' +
      '<th class="num">token</th>' +
      '<th class="num" title="命中缓存的 prompt token 占比；括号内是命中 / 写入">缓存命中</th>' +
      '<th class="num">平均延迟</th><th class="num">最大延迟</th>' +
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

  // ---- pages ----
  //
  // Four views, one visible at a time. The hash carries the view so a link can
  // point at one, and a reload lands where the operator was.

  var PAGES = ["usage", "errors", "main", "schemes"];
  var currentPage = "usage";

  // normalisePage maps a hash to a known page, defaulting to the first. An unknown
  // or empty hash is not an error worth showing; it is just the default view.
  function normalisePage(hash) {
    var name = String(hash == null ? "" : hash).replace(/^#\/?/, "").trim();
    return PAGES.indexOf(name) >= 0 ? name : PAGES[0];
  }

  // showPage reveals one view and marks its tab. Everything else is hidden rather
  // than removed, so form state and the chart survive a switch.
  function showPage(name) {
    currentPage = normalisePage(name);
    var views = document.querySelectorAll("main .page");
    for (var i = 0; i < views.length; i++) {
      views[i].classList.toggle("is-active", views[i].dataset.page === currentPage);
    }
    var tabs = document.querySelectorAll("#nav-pages a");
    for (var j = 0; j < tabs.length; j++) {
      if (tabs[j].dataset.page === currentPage) tabs[j].setAttribute("aria-current", "page");
      else tabs[j].removeAttribute("aria-current");
    }
    // The chart measures its container, so it has to be laid out before it draws.
    if (currentPage === "usage") renderHistory();
    syncHeaderButtons();
  }

  // syncHeaderButtons hides the stats-only controls on the configuration pages and
  // the save controls elsewhere, so every button on screen does something.
  function syncHeaderButtons() {
    var statsPage = currentPage === "usage" || currentPage === "errors";
    ["btn-refresh", "btn-auto", "refresh-legend", "btn-reset"].forEach(function (id) {
      var el = document.getElementById(id);
      if (el) el.style.display = statsPage ? "" : "none";
    });
  }

  function initPages() {
    window.addEventListener("hashchange", function () { showPage(location.hash); });
    showPage(location.hash);
  }

  function renderModels(stats) {
    var models = stats.models || [];
    rowCounts.models = models.length;
    var host = document.getElementById("models");
    var pager = document.getElementById("models-pager");
    if (!models.length) {
      host.innerHTML = '<div class="empty">还没有请求经过代理。</div>';
      if (pager) pager.innerHTML = "";
      return;
    }
    var win = pageWindow(models.length, pagedState.models.page, pagedState.models.size);
    pagedState.models.page = win.page;
    if (pager) pager.innerHTML = pagerHTML("models", win, pagedState.models.size);
    var rows = models.slice(win.start, win.end).map(function (m) {
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
        '<td class="num" title="命中缓存 / 写缓存（prompt token）">' +
          pct(m.cache_hit_rate || 0) + " <span class=\"muted\">" +
          num(m.cache_read) + " / " + num(m.cache_creation) + "</span></td>" +
        '<td class="num">' + ms(m.avg_latency_ms) + " / " + ms(m.max_latency_ms) + "</td>" +
        "<td>" + sparkline(m.sparkline || []) + "</td>" +
        "</tr>";
    }).join("");
    host.innerHTML = '<table><thead><tr>' +
      "<th>模型</th><th>网关</th><th>别名</th>" +
      '<th class="num">请求</th><th class="num">成功 / 失败</th><th class="num">失败率</th>' +
      '<th class="num">TPM</th><th class="num">RPM</th>' +
      '<th class="num">输入 / 输出 token</th>' +
      '<th class="num" title="命中缓存的 prompt token 占比；括号内是命中 / 写入">缓存命中</th>' +
      '<th class="num">平均 / 最大延迟</th>' +
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
    rowCounts.failures = events.length;

    if (!events.length) {
      host.innerHTML = '<div class="empty">没有失败记录。</div>';
      var emptyPager = document.getElementById("failures-pager");
      if (emptyPager) emptyPager.innerHTML = "";
      return;
    }

    var win = pageWindow(events.length, pagedState.failures.page, pagedState.failures.size);
    pagedState.failures.page = win.page;
    var fPager = document.getElementById("failures-pager");
    if (fPager) fPager.innerHTML = pagerHTML("failures", win, pagedState.failures.size);

    var summary = Object.keys(counts).sort(function (a, b) { return counts[b] - counts[a]; })
      .map(function (k) { return '<span class="tag">' + esc(k) + " ×" + num(counts[k]) + "</span>"; }).join(" ");

    var rows = events.slice(win.start, win.end).map(function (item) {
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

  // lastStats is the most recent snapshot, so a pager press repaints from memory
  // instead of refetching the same numbers.
  var lastStats = null;

  async function refresh() {
    try {
      var stats = await api("/admin/api/stats");
      lastStats = stats;
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

  // ---- pagination ----
  //
  // The model, failure and routing lists all grow without bound (models to 128,
  // failures to one page per model, aliases to whatever an operator types), so
  // each is paged rather than scrolled. The pager is a pure function of the list
  // length and the page state, which is what lets the node tests exercise it
  // without a DOM.

  // pageCount is how many pages a list of n rows occupies. An empty list still has
  // one page, so the pager renders "(0)" rather than a page 0 of 0.
  function pageCount(total, size) {
    if (!(size > 0)) return 1;
    return Math.max(1, Math.ceil(total / size));
  }

  // pageWindow resolves the rows one page shows: the clamped page, its offset and
  // the slice bounds. Clamping matters because a list can shrink under the
  // operator (a model ages out of the retained window) while they sit on page 3.
  function pageWindow(total, page, size) {
    var pages = pageCount(total, size);
    var p = Math.min(Math.max(1, page), pages);
    var start = (p - 1) * size;
    var end = Math.min(total, start + size);
    return { page: p, pages: pages, start: start, end: end, total: total };
  }

  // pagerHTML renders the bar itself. Buttons carry a data-pager-action so one
  // delegated listener on the container can serve every list.
  function pagerHTML(id, win, size, sizes) {
    var opts = (sizes || [20, 50, 100]).map(function (s) {
      return '<option value="' + s + '"' + (s === size ? " selected" : "") + ">" + s + " 条/页</option>";
    }).join("");
    return '' +
      '<button data-pager-action="first" data-pager="' + esc(id) + '"' + (win.page <= 1 ? " disabled" : "") + ">« 首页</button>" +
      '<button data-pager-action="prev" data-pager="' + esc(id) + '"' + (win.page <= 1 ? " disabled" : "") + ">‹ 上一页</button>" +
      '<span class="pager-info">第 ' + win.page + " / " + win.pages + " 页 · 共 " + num(win.total) + " 条</span>" +
      '<button data-pager-action="next" data-pager="' + esc(id) + '"' + (win.page >= win.pages ? " disabled" : "") + ">下一页 ›</button>" +
      '<button data-pager-action="last" data-pager="' + esc(id) + '"' + (win.page >= win.pages ? " disabled" : "") + ">末页 »</button>" +
      '<span class="pager-size">每页<select data-pager-size="' + esc(id) + '">' + opts + "</select></span>";
  }

  // pagedState is the page + size one list is on. Kept apart from the data so a
  // stats refresh (every 5s) does not reset the operator's page.
  var pagedState = {
    models: { page: 1, size: 20 },
    failures: { page: 1, size: 20 },
    routing: { page: 1, size: 20 }
  };

  // applyPagerAction advances one list's state for a button press.
  function applyPagerAction(state, action, total) {
    var pages = pageCount(total, state.size);
    switch (action) {
      case "first": state.page = 1; break;
      case "prev": state.page = Math.max(1, state.page - 1); break;
      case "next": state.page = Math.min(pages, state.page + 1); break;
      case "last": state.page = pages; break;
    }
    return state;
  }

  // mountPager wires one container's bar. The render callback re-runs on every
  // interaction, so the caller only supplies how to draw its own rows.
  function mountPager(hostId, listId, render) {
    var host = document.getElementById(hostId);
    if (!host) return;
    host.addEventListener("click", function (ev) {
      var btn = ev.target.closest ? ev.target.closest("[data-pager-action]") : null;
      if (!btn) return;
      var state = pagedState[listId];
      applyPagerAction(state, btn.dataset.pagerAction, currentRowCount(listId));
      render();
    });
    host.addEventListener("change", function (ev) {
      var sel = ev.target.closest ? ev.target.closest("[data-pager-size]") : null;
      if (!sel) return;
      var state = pagedState[listId];
      state.size = parseInt(sel.value, 10) || state.size;
      state.page = 1;
      render();
    });
  }

  // rowCounts is what the pagers page over, refreshed each time the data changes.
  var rowCounts = { models: 0, failures: 0, routing: 0 };

  function currentRowCount(listId) { return rowCounts[listId] || 0; }

  // ---- config ----
  //
  // The form edits one scheme file: a main upstream, an optional image-describe
  // pass, an optional chat passthrough and a model table. There are no global
  // upstream settings any more, so every field below belongs to the active scheme
  // and a save writes exactly that one file.

  function intOr0(v) { return parseInt(v, 10) || 0; }

  // keyActionOf turns one credential field into the three-state choice the server
  // expects. A stored key is never sent to the page, so blank is the only way to
  // leave one alone; the clear box wins over a typed value, matching the UI, which
  // empties the field when the box is ticked.
  function keyActionOf(value, cleared) {
    if (cleared) return "clear";
    return value !== "" ? "set" : "keep";
  }

  // fillSelect rebuilds a <select> from a list of {value,label,title} entries.
  function fillSelect(sel, entries) {
    sel.innerHTML = entries.map(function (e) {
      return '<option value="' + esc(e.value) + '"' +
        (e.title ? ' title="' + esc(e.title) + '"' : "") + ">" + esc(e.label) + "</option>";
    }).join("");
  }

  // upstreamFields builds the markup for one upstream's controls. prefix keeps the
  // ids unique across the three upstreams on the page.
  function upstreamFields(prefix, showProtocol) {
    return '' +
      (showProtocol
        ? '<div><label>协议</label><select id="' + prefix + '-protocol">' +
            '<option value="anthropic">anthropic</option>' +
            '<option value="openai">openai</option>' +
          '</select></div>'
        : "") +
      '<div><label>地址</label><input type="text" id="' + prefix + '-url" placeholder="上游地址"></div>' +
      '<div><label>密钥环境变量</label><input type="text" id="' + prefix + '-key-env" placeholder="可选，优先于密钥"></div>' +
      '<div><label>密钥</label><input type="password" id="' + prefix + '-key" autocomplete="new-password" placeholder="留空则保持原密钥不变"></div>' +
      '<div class="chk"><input type="checkbox" id="' + prefix + '-key-clear"><label for="' + prefix + '-key-clear">清空密钥</label></div>' +
      '<div><label title="等待上游响应头的最长时间（秒）">响应头超时</label><input type="number" id="' + prefix + '-header-timeout" min="0"></div>' +
      '<div><label title="读取响应体允许的最长静默（秒）">响应体静默</label><input type="number" id="' + prefix + '-body-idle" min="0"></div>' +
      '<div><label title="瞬态错误后的额外重试次数">重试次数</label><input type="number" id="' + prefix + '-max-retries" min="0"></div>';
  }

  // readUpstream snapshots one upstream's controls into the payload the server
  // takes. Reading through the document keeps this usable for the three sections
  // that share the same field set.
  function readUpstream(prefix, withProtocol) {
    return {
      protocol: withProtocol ? document.getElementById(prefix + "-protocol").value : "",
      url: document.getElementById(prefix + "-url").value.trim(),
      key_action: keyActionOf(
        document.getElementById(prefix + "-key").value,
        document.getElementById(prefix + "-key-clear").checked
      ),
      key: document.getElementById(prefix + "-key").value,
      key_env: document.getElementById(prefix + "-key-env").value.trim(),
      header_timeout_seconds: intOr0(document.getElementById(prefix + "-header-timeout").value),
      body_idle_seconds: intOr0(document.getElementById(prefix + "-body-idle").value),
      max_retries: intOr0(document.getElementById(prefix + "-max-retries").value)
    };
  }

  // fillUpstream writes one upstream's state into the form. A key is never echoed
  // back, so the field is cleared and the hint says whether one is stored.
  function fillUpstream(prefix, up, withProtocol) {
    if (withProtocol) document.getElementById(prefix + "-protocol").value = up.protocol || "anthropic";
    document.getElementById(prefix + "-url").value = up.url || "";
    document.getElementById(prefix + "-key-env").value = up.key_env || "";
    document.getElementById(prefix + "-key").value = "";
    document.getElementById(prefix + "-key-clear").checked = false;
    document.getElementById(prefix + "-header-timeout").value = up.header_timeout_seconds || 0;
    document.getElementById(prefix + "-body-idle").value = up.body_idle_seconds || 0;
    document.getElementById(prefix + "-max-retries").value = up.max_retries || 0;
  }

  function keyHint(up) {
    if (up.key_from_env) return "密钥来自环境变量 " + up.key_env + "（优先于此处填写的值）";
    if (up.key_set) return "已配置密钥（不回显明文）";
    return "未配置密钥";
  }

  // schemeRoutePayload validates one row of the model table. An empty row is
  // dropped rather than saved as a broken entry the server would reject.
  function schemeRoutePayload(alias, model, supportsImage) {
    var a = String(alias == null ? "" : alias).trim();
    var m = String(model == null ? "" : model).trim();
    if (!a && !m) return null;
    return { alias: a, model: m, supports_image: !!supportsImage };
  }

  // routeRow renders one alias of the model table.
  function routeRow(entry) {
    var row = document.createElement("div");
    row.className = "route-row";
    row.innerHTML =
      '<div><label>别名</label><input type="text" class="r-alias" placeholder="Claude Code 用的模型名"></div>' +
      '<div><label>上游模型名</label><input type="text" class="r-model"></div>' +
      '<div class="chk"><input type="checkbox" class="r-image"><label>supports_image</label></div>' +
      '<div><button type="button" class="danger r-del">删除</button></div>';
    row.querySelector(".r-alias").value = entry.alias || "";
    row.querySelector(".r-model").value = entry.model || "";
    row.querySelector(".r-image").checked = !!entry.supports_image;
    row.querySelector(".r-del").addEventListener("click", function () { row.remove(); });
    return row;
  }

  // readRouting collects the model table the save should write.
  //
  // Only one page of rows is in the DOM, so the entries behind the other pages
  // have to come from the loaded table. The rows on screen replace their own slice;
  // everything else is carried over unchanged. Reading the DOM alone would silently
  // delete every alias the operator was not looking at.
  function readRouting() {
    var win = pageWindow(routingEntries.length, pagedState.routing.page, pagedState.routing.size);
    var edited = [];
    var bad = null;
    var rows = document.querySelectorAll("#routing .route-row");
    for (var i = 0; i < rows.length; i++) {
      var row = rows[i];
      var entry = schemeRoutePayload(
        row.querySelector(".r-alias").value,
        row.querySelector(".r-model").value,
        row.querySelector(".r-image").checked
      );
      if (!entry) continue;
      if (!entry.alias) bad = "有一个别名只填了模型名，请补上别名";
      if (!entry.model) bad = "别名 " + entry.alias + " 缺少上游模型名";
      edited.push(entry);
    }
    var out = routingEntries.slice(0, win.start).concat(edited, routingEntries.slice(win.end));
    return { entries: out, error: bad };
  }

  // mountUpstreamForms writes the three upstream field sets into their grids once
  // at boot. They are identical in shape, so building them from one helper keeps
  // the ids and field order from drifting apart.
  function mountUpstreamForms() {
    document.getElementById("up-main").innerHTML = upstreamFields("up-main", true);
    document.getElementById("up-vlm").innerHTML = upstreamFields("up-vlm", true);
    document.getElementById("up-chat").innerHTML = upstreamFields("up-chat", true);
  }

  // renderScheme fills the scheme editor from one scheme view. It is called for
  // whichever scheme the selector names, so editing an inactive scheme shows that
  // file's own values — not the running scheme's.
  function renderScheme(view) {
    schemeView = view;
    document.getElementById("cfg-scheme").value = view.name || "";
    document.getElementById("cfg-scheme-path").textContent = "方案文件：" + (view.path || "（未知）");
    document.getElementById("cfg-scheme-state").innerHTML = view.active
      ? '<span class="tag ok">生效中</span> <span class="muted">保存后立即生效</span>'
      : '<span class="tag">未生效</span> <span class="muted">保存只写文件，需在主配置里启用后才生效</span>';
    var actBtn = document.getElementById("btn-activate");
    actBtn.disabled = !!view.active;
    actBtn.textContent = view.active ? "已是当前方案" : "启用该方案";

    // Main upstream.
    fillUpstream("up-main", view.upstream, true);
    var mainHintId = "up-main-key-hint";
    var mainHint = document.getElementById(mainHintId);
    if (!mainHint) {
      mainHint = document.createElement("div");
      mainHint.id = mainHintId;
      mainHint.className = "hint";
      document.getElementById("up-main").appendChild(mainHint);
    }
    mainHint.textContent = keyHint(view.upstream);

    // VLM.
    document.getElementById("cfg-vlm-active").checked = !!view.vlm.active;
    document.getElementById("cfg-vlm-model").value = view.vlm.model || "";
    document.getElementById("cfg-vlm-max-tokens").value = view.vlm.max_tokens || 0;
    document.getElementById("cfg-vlm-inherit").checked = !!view.vlm.inherit;
    fillUpstream("up-vlm", view.vlm.upstream, true);
    syncVLMSection();

    // Chat.
    document.getElementById("cfg-chat-available").checked = !!view.chat.available;
    document.getElementById("cfg-chat-inherit").checked = !!view.chat.inherit;
    fillUpstream("up-chat", view.chat.upstream, true);
    syncChatSection();

    // The model table, paged.
    renderRouting(view.routing || []);

    // The effective table is read-only: it includes the haiku fallback, which is
    // derived rather than written.
    var eff = view.effective_routing || {};
    var names = Object.keys(eff).sort();
    document.getElementById("effective-routes").innerHTML = names.length
      ? names.map(function (alias) {
          var e = eff[alias];
          return '<span class="tag" title="' + (e.declared ? "方案中已声明" : "未声明，由兜底规则推导") + '">' +
                 esc(alias) + " → " + esc(e.model) +
                 (e.supports_image ? " · 原生图片" : "") +
                 (e.declared ? "" : " · 兜底") + "</span>";
        }).join(" ")
      : '<span class="muted">—</span>';
  }

  // renderRouting draws one page of the model table. The rows live in the DOM, so
  // an edit made on page 2 has to survive a repaint: rows are only re-created when
  // the list itself is replaced (a reload or a scheme switch), never when paging.
  function renderRouting(entries) {
    routingEntries = entries;
    rowCounts.routing = entries.length;
    var host = document.getElementById("routing");
    var pager = document.getElementById("routing-pager");
    if (!entries.length) {
      host.innerHTML = '<div class="empty-page">还没有别名。点下面的「添加别名」新增。</div>';
      if (pager) pager.innerHTML = "";
      return;
    }
    var win = pageWindow(entries.length, pagedState.routing.page, pagedState.routing.size);
    pagedState.routing.page = win.page;
    if (pager) pager.innerHTML = pagerHTML("routing", win, pagedState.routing.size);
    host.innerHTML = "";
    entries.slice(win.start, win.end).forEach(function (e) { host.appendChild(routeRow(e)); });
  }

  // renderMain fills the main-configuration page.
  function renderMain(cfg) {
    config = cfg;
    document.getElementById("cfg-port").value = cfg.proxy.port;
    document.getElementById("cfg-active-scheme").value = cfg.proxy.scheme || "";
    document.getElementById("cfg-paths").textContent =
      "主配置：" + cfg.config_path + "　方案目录：" + cfg.proxy.schemes_dir;

    document.getElementById("cfg-admin-token").value = "";
    document.getElementById("cfg-admin-clear").checked = false;
    document.getElementById("cfg-admin-hint").textContent = cfg.admin.token_from_env
      ? "已通过环境变量 LLM_PROXY_ADMIN_TOKEN 配置（优先于此处，在此修改不会生效）"
      : (cfg.admin.token_set ? "已配置（不回显明文）。修改后需用新口令重新登录。" : "未配置：管理页面当前已关闭");
  }

  // renderSchemeChoices rebuilds the scheme dropdown from the config view's list.
  function renderSchemeChoices(cfg) {
    var names = cfg.proxy.scheme_list || [];
    fillSelect(document.getElementById("cfg-scheme"), names.map(function (name) {
      return { value: name, label: name + (name === cfg.proxy.scheme ? " · 生效中" : "") };
    }));
    document.getElementById("cfg-scheme-hint").textContent =
      names.length + " 份方案位于 " + cfg.proxy.schemes_dir + "。下拉选择要编辑的方案；保存只写该方案文件。";
  }

  // syncVLMSection disables the describe-pass fields that the checkboxes make
  // irrelevant, so an operator cannot fill in a value the save will discard.
  function syncVLMSection() {
    var active = document.getElementById("cfg-vlm-active").checked;
    document.getElementById("vlm-body").style.opacity = active ? "1" : "0.45";
    var inherit = document.getElementById("cfg-vlm-inherit").checked;
    document.querySelectorAll("#up-vlm input, #up-vlm select").forEach(function (el) {
      el.disabled = !active || inherit;
    });
  }

  function syncChatSection() {
    var available = document.getElementById("cfg-chat-available").checked;
    document.getElementById("chat-body").style.opacity = available ? "1" : "0.45";
    var inherit = document.getElementById("cfg-chat-inherit").checked;
    document.querySelectorAll("#up-chat input, #up-chat select").forEach(function (el) {
      el.disabled = !available || inherit;
    });
  }

  // collectMainPayload is the main-configuration form's body.
  function collectMainPayload() {
    return {
      port: intOr0(document.getElementById("cfg-port").value),
      admin: {
        token_action: keyActionOf(
          document.getElementById("cfg-admin-token").value,
          document.getElementById("cfg-admin-clear").checked
        ),
        token: document.getElementById("cfg-admin-token").value
      }
    };
  }

  // collectSchemePayload is the scheme editor's body. The name says which file it
  // writes; the server edits that file whether or not it is the running one.
  function collectSchemePayload() {
    var routing = readRouting();
    if (routing.error) throw new Error(routing.error);
    return {
      name: document.getElementById("cfg-scheme").value,
      upstream: readUpstream("up-main", true),
      vlm: {
        active: document.getElementById("cfg-vlm-active").checked,
        model: document.getElementById("cfg-vlm-model").value.trim(),
        max_tokens: intOr0(document.getElementById("cfg-vlm-max-tokens").value),
        inherit: document.getElementById("cfg-vlm-inherit").checked,
        upstream: readUpstream("up-vlm", true)
      },
      chat: {
        available: document.getElementById("cfg-chat-available").checked,
        inherit: document.getElementById("cfg-chat-inherit").checked,
        upstream: readUpstream("up-chat", true)
      },
      routing: routing.entries
    };
  }

  // loadConfig refreshes both configuration pages from one config read.
  async function loadConfig() {
    try {
      var cfg = await api("/admin/api/config");
      renderMain(cfg);
      renderSchemeChoices(cfg);
      await loadScheme(document.getElementById("cfg-scheme").value);
    } catch (err) {
      banner("scheme-banner", "err", "读取配置失败：" + err.message, true);
    }
  }

  // loadScheme loads one scheme file into the editor.
  async function loadScheme(name) {
    if (!name) {
      banner("scheme-banner", "err", "没有可编辑的方案：方案目录里还没有 .toml 文件。", true);
      return;
    }
    renderScheme(await api("/admin/api/schemes?name=" + encodeURIComponent(name)));
  }

  // saveMain posts only the main configuration, so a port or password change can
  // never rewrite a scheme file as a side effect.
  async function saveMain() {
    var btn = document.getElementById("btn-save-main");
    btn.disabled = true;
    document.getElementById("main-save-hint").textContent = "保存中…";
    try {
      var res = await api("/admin/api/config/main", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(collectMainPayload())
      });
      renderMain(res.config);
      renderSchemeChoices(res.config);
      reportSave("main-banner", "main-save-hint", res, "主配置已保存并生效。");
      if (res.reauth_required) {
        setAuto(false);
        setTimeout(function () { location.reload(); }, 2500);
      }
    } catch (err) {
      banner("main-banner", "err", "保存失败：" + err.message, true);
      document.getElementById("main-save-hint").textContent = "";
    } finally {
      btn.disabled = false;
    }
  }

  // saveScheme posts one scheme file. Whether that file is the running one decides
  // what the page can promise: "生效" only when the server says it reloaded.
  async function saveScheme() {
    var btn = document.getElementById("btn-save-scheme");
    btn.disabled = true;
    document.getElementById("scheme-save-hint").textContent = "保存中…";
    try {
      var payload = collectSchemePayload();
      var res = await api("/admin/api/config/scheme", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(payload)
      });
      renderSchemeChoices(res.config);
      renderScheme(res.scheme);
      reportSave("scheme-banner", "scheme-save-hint", res,
        res.applied ? "方案已保存并生效。" : "方案已保存。它当前未启用，启用后才生效。");
      if (res.applied) refresh();
    } catch (err) {
      banner("scheme-banner", "err", "保存失败：" + err.message, true);
      document.getElementById("scheme-save-hint").textContent = "";
    } finally {
      btn.disabled = false;
    }
  }

  // activateScheme switches the running scheme. It writes one line of the main
  // file and no scheme file.
  async function activateScheme() {
    var btn = document.getElementById("btn-activate");
    var name = document.getElementById("cfg-scheme").value;
    btn.disabled = true;
    document.getElementById("activate-hint").textContent = "切换中…";
    try {
      var res = await api("/admin/api/config/activate", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ scheme: name })
      });
      renderMain(res.config);
      renderSchemeChoices(res.config);
      renderScheme(res.scheme);
      var warns = res.warnings || [];
      banner("scheme-banner", "warnbox", "已启用方案 " + name + "。" + (warns.length ? "\n· " + warns.join("\n· ") : ""), true);
      document.getElementById("activate-hint").textContent = "已启用 " + new Date().toLocaleTimeString("zh-CN");
      refresh();
    } catch (err) {
      banner("scheme-banner", "err", "启用失败：" + err.message, true);
      document.getElementById("activate-hint").textContent = "";
    } finally {
      btn.disabled = false;
    }
  }

  // reportSave renders one save's outcome, including the backend's own warnings.
  function reportSave(bannerId, hintId, res, okText) {
    var warns = res.warnings || [];
    if (res.reauth_required) {
      banner(bannerId, "warnbox", okText + (warns.length ? "\n· " + warns.join("\n· ") : "") +
        "\n管理口令已更新，页面即将刷新，请用新口令登录。", true);
    } else if (warns.length) {
      banner(bannerId, "warnbox", okText + "\n· " + warns.join("\n· "), true);
    } else {
      banner(bannerId, "ok", okText, true);
    }
    document.getElementById(hintId).textContent = "已保存 " + new Date().toLocaleTimeString("zh-CN");
  }

  // ---- theme ----
  //
  // The page ships both palettes; CSS picks one from `prefers-color-scheme` and
  // this toggle overrides it. The choice is remembered per browser so an operator
  // who prefers one does not have to re-pick on every visit, and an explicit pick
  // is written as data-theme="light"/"dark" so it outranks the media query.
  var THEME_KEY = "llm-proxy-theme";

  function storedTheme() {
    try {
      var v = window.localStorage.getItem(THEME_KEY);
      return v === "light" || v === "dark" ? v : "";
    } catch (err) {
      // Private mode and some embedded webviews throw on localStorage access;
      // a missing preference is not a reason to break the page.
      return "";
    }
  }

  function applyTheme(theme) {
    var root = document.documentElement;
    if (theme === "light" || theme === "dark") root.setAttribute("data-theme", theme);
    else root.removeAttribute("data-theme");
    var btn = document.getElementById("btn-theme");
    if (btn) {
      var dark = theme === "dark" ||
        (!theme && window.matchMedia && window.matchMedia("(prefers-color-scheme: dark)").matches);
      btn.textContent = dark ? "☀" : "☾";
      btn.title = dark ? "切换到浅色" : "切换到深色";
    }
  }

  function currentTheme() {
    var explicit = document.documentElement.getAttribute("data-theme");
    if (explicit) return explicit;
    if (window.matchMedia && window.matchMedia("(prefers-color-scheme: dark)").matches) return "dark";
    return "light";
  }

  function initTheme() {
    applyTheme(storedTheme());
    document.getElementById("btn-theme").addEventListener("click", function () {
      var next = currentTheme() === "dark" ? "light" : "dark";
      applyTheme(next);
      try { window.localStorage.setItem(THEME_KEY, next); } catch (err) { /* preference is best-effort */ }
      // The chart's colours come from CSS variables, but its geometry was measured
      // against the previous layout, so redraw after the palette settles.
      if (document.getElementById("history-chart")) renderHistory();
    });
    // Follow the OS when the operator has not picked a side.
    if (window.matchMedia) {
      var mq = window.matchMedia("(prefers-color-scheme: dark)");
      var onChange = function () { if (!storedTheme()) { applyTheme(""); if (document.getElementById("history-chart")) renderHistory(); } };
      if (mq.addEventListener) mq.addEventListener("change", onChange);
      else if (mq.addListener) mq.addListener(onChange);
    }
  }

  function initAdmin() {
  initTheme();
  initPages();
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

  // Pagers. Each list re-renders from the last snapshot it received, so paging does
  // not refetch: the data is already in memory.
  mountPager("models-pager", "models", function () { if (lastStats) { renderModels(lastStats); } });
  mountPager("failures-pager", "failures", function () { if (lastStats) { renderFailures(lastStats); } });
  mountPager("routing-pager", "routing", function () {
    // Paging the model table has to keep the edits already typed, so the current
    // rows are folded back into the stored list before the next page is drawn.
    var current = readRouting();
    if (!current.error) routingEntries = current.entries;
    renderRouting(routingEntries);
  });

  document.getElementById("btn-add-route").addEventListener("click", function () {
    // A new row joins the last page, which is where the operator will look for it.
    var list = document.getElementById("routing");
    if (list.querySelector(".empty-page")) {
      pagedState.routing.page = 1;
      renderRouting([]);
    }
    list.appendChild(routeRow({}));
    rowCounts.routing = list.querySelectorAll(".route-row").length;
  });
  document.getElementById("cfg-vlm-active").addEventListener("change", syncVLMSection);
  document.getElementById("cfg-vlm-inherit").addEventListener("change", syncVLMSection);
  document.getElementById("cfg-chat-available").addEventListener("change", syncChatSection);
  document.getElementById("cfg-chat-inherit").addEventListener("change", syncChatSection);
  mountUpstreamForms();

  document.getElementById("cfg-scheme").addEventListener("change", function () {
    hideBanner("scheme-banner");
    document.getElementById("scheme-save-hint").textContent = "";
    pagedState.routing.page = 1;
    loadScheme(this.value).catch(function (err) {
      banner("scheme-banner", "err", "读取方案失败：" + err.message, true);
    });
  });

  document.getElementById("btn-save-main").addEventListener("click", saveMain);
  document.getElementById("btn-reload-main").addEventListener("click", function () {
    hideBanner("main-banner");
    document.getElementById("main-save-hint").textContent = "";
    loadConfig();
  });
  document.getElementById("btn-save-scheme").addEventListener("click", saveScheme);
  document.getElementById("btn-activate").addEventListener("click", activateScheme);
  document.getElementById("btn-reload-scheme").addEventListener("click", function () {
    hideBanner("scheme-banner");
    document.getElementById("scheme-save-hint").textContent = "";
    loadScheme(document.getElementById("cfg-scheme").value).catch(function (err) {
      banner("scheme-banner", "err", "读取方案失败：" + err.message, true);
    });
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
      historyMetrics: HISTORY_METRICS,
      // The config form's helpers are pure functions over plain values, so they
      // are testable without a DOM: the payload rows the page builds are exactly
      // what the save validator has to accept.
      keyActionOf: keyActionOf,
      schemeRoutePayload: schemeRoutePayload,
      // Paging and view routing are pure functions of their inputs, so the node
      // tests can pin the edge cases (empty list, shrinking list, unknown hash)
      // without a DOM.
      pageCount: pageCount,
      pageWindow: pageWindow,
      applyPagerAction: applyPagerAction,
      pagerHTML: pagerHTML,
      normalisePage: normalisePage,
      viewPages: PAGES,
      // Hooks for the paged-list tests: they need to seed the stored table and the
      // page state the same way a load does, then read back what a save would send.
      __setRoutingEntries: function (entries, state) {
        routingEntries = entries;
        if (state) { pagedState.routing.page = state.page; pagedState.routing.size = state.size; }
      },
      __readRouting: readRouting,
    };
    return;
  }

  initAdmin();
})();
