// Unit tests for the admin page's chart arithmetic. The page has no build step
// and no framework, so the pure helpers in admin.js are exported through a
// `typeof document === "undefined"` guard and exercised here directly.
const assert = require("node:assert/strict");
const { test } = require("node:test");

const admin = require("./admin.js");

/** A snapshot shaped exactly like GET /admin/api/stats/history returns. */
function snapshot(overrides) {
  return Object.assign(
    {
      range: "1h",
      from: 1000,
      to: 1060,
      bucket_seconds: 60,
      timestamps: [1000, 1020, 1040, 1060],
      totals: { requests: 0, failures: 0, tokens: 0 },
      models: [],
      all_models: [],
      models_excluded: 0,
    },
    overrides,
  );
}

function series(model, points) {
  return { model, points };
}

test("formatCount abbreviates thousands and millions", () => {
  assert.equal(admin.formatCount(0), "0");
  assert.equal(admin.formatCount(999), "999");
  assert.equal(admin.formatCount(1500), "1.5K");
  assert.equal(admin.formatCount(12345678), "12.3M");
});

test("formatMetric switches units by metric", () => {
  assert.equal(admin.formatMetric(0.125, "failure"), "12.5%");
  assert.equal(admin.formatMetric(0, "failure"), "0%");
  assert.equal(admin.formatMetric(0.734, "cache"), "73.4%");
  assert.equal(admin.formatMetric(0.005, "cache"), "0.50%");
  assert.equal(admin.formatMetric(0, "cache"), "0%");
  assert.equal(admin.formatMetric(250, "latency"), "250 ms");
  assert.equal(admin.formatMetric(2500, "latency"), "2.50 s");
  assert.equal(admin.formatMetric(420, "tpm"), "420.0");
  assert.equal(admin.formatMetric(4200, "tpm"), "4.2K");
});

// The bucket length changes with the range, so a raw bucket count would look
// like traffic collapsing when the operator switches from 1h to 24h.
test("metricValue normalises bucket totals to per-minute rates", () => {
  const point = { tokens: 900, requests: 9, failures: 1, latency_ms: 9000 };
  assert.equal(admin.metricValue(point, "tpm", 60), 900);
  assert.equal(admin.metricValue(point, "tpm", 900), 60);
  assert.equal(admin.metricValue(point, "rpm", 900), 0.6);
  assert.equal(admin.metricValue(point, "failure", 900), 1 / 9);
  assert.equal(admin.metricValue(point, "latency", 900), 1000);
});

// A quiet bucket must read as zero rather than as a ratio against no requests.
test("metricValue treats a bucket with no requests as zero, not NaN", () => {
  const empty = { tokens: 0, requests: 0, failures: 0, latency_ms: 0 };
  assert.equal(admin.metricValue(empty, "failure", 60), 0);
  assert.equal(admin.metricValue(empty, "latency", 60), 0);
  assert.equal(admin.metricValue(empty, "cache", 60), 0);
});

// The cache hit rate is not a per-minute rate: it is a share of the prompt, so it
// must be the same number whatever the bucket length is.
test("metricValue reads the cache hit rate straight off the bucket", () => {
  const point = { input: 300, cache_read: 700, cache_creation: 0, requests: 2, tokens: 1010 };
  assert.equal(admin.metricValue(point, "cache", 60), 0.7);
  assert.equal(admin.metricValue(point, "cache", 900), 0.7);
});

// Cache writes belong in the denominator: a bucket that only wrote to the cache
// read nothing, and calling that a 100% hit would flatter the ratio.
test("metricValue counts cache writes against the hit rate", () => {
  const point = { input: 0, cache_read: 500, cache_creation: 500, requests: 1, tokens: 500 };
  assert.equal(admin.metricValue(point, "cache", 60), 0.5);
});

// Older payloads (and a bucket whose counters are all zero) must not divide by
// zero or fall back to the pre-computed field, which can disagree with the raw
// counters the tooltip shows.
test("metricValue falls back to the reported rate when counters are absent", () => {
  assert.equal(admin.metricValue({ cache_hit_rate: 0.42, requests: 1 }, "cache", 60), 0.42);
  assert.equal(admin.metricValue({ requests: 1 }, "cache", 60), 0);
});

test("buildChartRows keeps one row per timestamp", () => {
  const snap = snapshot({
    models: [
      series("m1", [
        { ts: 1000, tokens: 60, requests: 1, failures: 0, latency_ms: 100 },
        { ts: 1020, tokens: 120, requests: 2, failures: 1, latency_ms: 900 },
        { ts: 1040, tokens: 0, requests: 0, failures: 0, latency_ms: 0 },
        { ts: 1060, tokens: 30, requests: 1, failures: 0, latency_ms: 50 },
      ]),
    ],
  });
  const rows = admin.buildChartRows(snap, "tpm", {});
  assert.equal(rows.length, 4);
  assert.deepEqual(
    rows.map((r) => r.ts),
    snap.timestamps,
  );
  assert.deepEqual(
    rows.map((r) => r.values.m1),
    [60, 120, 0, 30],
  );
});

test("buildChartRows omits hidden models", () => {
  const snap = snapshot({
    models: [series("m1", [{ tokens: 60 }]), series("m2", [{ tokens: 90 }])],
  });
  const row = admin.buildChartRows(snap, "tpm", { m2: true })[0];
  assert.equal(row.values.m1, 60);
  assert.equal(row.values.m2, undefined);
});

test("chartMax sums a stacked metric and takes the peak of a line metric", () => {
  const rows = admin.buildChartRows(
    snapshot({
      models: [
        series("m1", [{ tokens: 100 }, { tokens: 10 }]),
        series("m2", [{ tokens: 50 }, { tokens: 5 }]),
      ],
    }),
    "tpm",
    {},
  );
  assert.equal(admin.chartMax(rows, ["m1", "m2"], true), 150);
  assert.equal(admin.chartMax(rows, ["m1", "m2"], false), 100);
});

test("niceCeil rounds up to a readable axis top", () => {
  assert.equal(admin.niceCeil(812), 1000);
  assert.equal(admin.niceCeil(1200), 2000);
  assert.equal(admin.niceCeil(1), 1);
  assert.equal(admin.niceCeil(0), 1);
  assert.equal(admin.niceCeil(23000), 25000);
});

// Ticks have to be evenly spaced, or the axis labels stop lining up with the
// data. The final tick is appended only when the label would clear the previous
// one; on a narrow panel it is dropped rather than drawn overlapping.
test("pickTickIndices spaces ticks evenly across the axis", () => {
  assert.deepEqual(admin.pickTickIndices(60, 6, 920), [0, 10, 20, 30, 40, 50, 59]);
  assert.deepEqual(admin.pickTickIndices(72, 6, 920), [0, 12, 24, 36, 48, 60, 71]);
  assert.deepEqual(admin.pickTickIndices(96, 8, 920), [0, 12, 24, 36, 48, 60, 72, 84, 95]);
  assert.deepEqual(admin.pickTickIndices(5, 6, 920), [0, 1, 2, 3, 4]);
  assert.deepEqual(admin.pickTickIndices(1, 6, 920), [0]);
  assert.deepEqual(admin.pickTickIndices(0, 6, 920), []);
});

test("pickTickIndices drops the last tick when the panel is too narrow for it", () => {
  // 60 points over 400px put 50 and 59 only 61px apart, inside one label's width.
  assert.deepEqual(admin.pickTickIndices(60, 6, 400), [0, 10, 20, 30, 40, 50]);
  // Without a measured width the safe answer is to not append it at all.
  assert.deepEqual(admin.pickTickIndices(60, 6, 0), [0, 10, 20, 30, 40, 50]);
});

test("buildAreaPath closes the band between its baseline and its top", () => {
  const rows = [{ values: { m1: 10 } }, { values: { m1: 20 } }];
  const scale = { x: (i) => i * 10, y: (v) => 100 - v };
  const d = admin.buildAreaPath(rows, "m1", [0, 0], scale);
  assert.equal(d, "M0 90L10 80L10 100L0 100Z");
});

test("buildAreaPath stacks on the baseline it is given", () => {
  const rows = [{ values: { m2: 10 } }];
  const scale = { x: (i) => i, y: (v) => 100 - v };
  // m2 sits on top of m1's 30, so its band spans 70..60 rather than 90..100.
  assert.equal(admin.buildAreaPath(rows, "m2", [30], scale), "M0 60L0 70Z");
});

test("buildLinePath emits an open polyline", () => {
  const rows = [{ values: { m1: 10 } }, { values: { m1: 20 } }];
  const scale = { x: (i) => i * 10, y: (v) => 100 - v };
  assert.equal(admin.buildLinePath(rows, "m1", scale), "M0 90L10 80");
});

test("buildBaselines accumulates the models drawn below each one", () => {
  const rows = [
    { values: { a: 1, b: 2, c: 3 } },
    { values: { a: 10, b: 20, c: 30 } },
  ];
  const base = admin.buildBaselines(rows, ["a", "b", "c"]);
  assert.deepEqual(base[0], [0, 0]);
  assert.deepEqual(base[1], [1, 10]);
  assert.deepEqual(base[2], [3, 30]);
});

test("summarizeModels ranks by failure rate and drops idle models", () => {
  const snap = snapshot({
    models: [
      series("healthy", [
        { requests: 10, failures: 0, tokens: 100, latency_ms: 1000, max_latency_ms: 200 },
      ]),
      series("flaky", [
        { requests: 4, failures: 2, tokens: 40, latency_ms: 800, max_latency_ms: 900 },
        { requests: 0, failures: 0, tokens: 0, latency_ms: 0, max_latency_ms: 0 },
      ]),
      series("idle", [{ requests: 0, failures: 0, tokens: 0, latency_ms: 0, max_latency_ms: 0 }]),
    ],
  });
  const rows = admin.summarizeModels(snap);
  assert.deepEqual(
    rows.map((r) => r.model),
    ["flaky", "healthy"],
  );
  assert.equal(rows[0].failureRate, 0.5);
  assert.equal(rows[0].avgLatency, 200);
  assert.equal(rows[0].maxLatency, 900);
  assert.equal(rows[1].failureRate, 0);
});

// The summary's cache column is a token-weighted share over the whole window,
// taken from the series totals rather than re-added from the buckets.
test("summarizeModels carries the window cache hit rate", () => {
  const snap = snapshot({
    models: [
      Object.assign(series("cached", [{ requests: 1, tokens: 10 }]), {
        cache_read: 700,
        cache_creation: 100,
        cache_hit_rate: 0.7,
      }),
      Object.assign(series("uncached", [{ requests: 1, tokens: 10 }]), {
        cache_hit_rate: 0,
      }),
    ],
  });
  const rows = admin.summarizeModels(snap);
  assert.equal(rows[0].model, "cached");
  assert.equal(rows[0].cacheHitRate, 0.7);
  assert.equal(rows[0].cacheRead, 700);
  assert.equal(rows[0].cacheCreation, 100);
  assert.equal(rows[1].cacheHitRate, 0);
});

// A payload without the cache fields must still summarize, so the table does not
// disappear against an older proxy.
test("summarizeModels tolerates missing cache fields", () => {
  const rows = admin.summarizeModels(snapshot({ models: [series("m1", [{ requests: 1 }])] }));
  assert.equal(rows[0].cacheHitRate, 0);
  assert.equal(rows[0].cacheRead, 0);
});

test("summarizeModels tolerates an empty payload", () => {
  assert.deepEqual(admin.summarizeModels(null), []);
  assert.deepEqual(admin.summarizeModels(snapshot()), []);
});

test("colorForSeries wraps instead of running out of colours", () => {
  assert.equal(admin.colorForSeries(0), admin.colorForSeries(8));
  assert.notEqual(admin.colorForSeries(0), admin.colorForSeries(1));
});

test("esc escapes markup from model names", () => {
  assert.equal(admin.esc('<img src=x onerror="alert(1)">'), "&lt;img src=x onerror=&quot;alert(1)&quot;&gt;");
});

test("formatBucketLabel widens with the bucket", () => {
  const ts = Math.floor(new Date(2026, 8, 21, 14, 5).getTime() / 1000);
  assert.equal(admin.formatBucketLabel(ts, 60), "14:05");
  assert.equal(admin.formatBucketLabel(ts, 3600), "09-21 14:00");
  assert.equal(admin.formatBucketLabel(ts, 86400), "09-21");
});

// ---- config form: profile options and the key three-state ----

// The key field is keep/set/clear, and blank has to mean "keep": the stored key is
// never sent to the page, so reading blank as "no key" would wipe it on every save.
test("keyActionOf reads blank as keep", () => {
  assert.equal(admin.keyActionOf("", false), "keep");
});

test("keyActionOf reads a typed value as set", () => {
  assert.equal(admin.keyActionOf("sk-new", false), "set");
});

test("keyActionOf reads the clear box as clear", () => {
  assert.equal(admin.keyActionOf("", true), "clear");
  // clear wins over a leftover typed value, matching the UI, which empties the
  // field as soon as the box is ticked.
  assert.equal(admin.keyActionOf("sk-typed", true), "clear");
});

// A model-table row carries only its own two names: the scheme already fixes
// which upstream an alias goes to, so there is nothing to choose.
test("schemeRoutePayload carries alias, model and the image flag", () => {
  assert.deepEqual(admin.schemeRoutePayload(" alpha-fast ", " alpha-model ", false), {
    alias: "alpha-fast",
    model: "alpha-model",
    supports_image: false,
  });
  assert.equal(admin.schemeRoutePayload("v", "m", true).supports_image, true);
  // An entirely blank row is a line the operator left behind.
  assert.equal(admin.schemeRoutePayload("  ", "  ", false), null);
});

// ---- pagination ----

// An empty list still occupies one page, so the pager shows "第 1 / 1 页 · 共 0 条"
// instead of a page 0 that the operator cannot leave.
test("pageCount always reports at least one page", () => {
  assert.equal(admin.pageCount(0, 20), 1);
  assert.equal(admin.pageCount(1, 20), 1);
  assert.equal(admin.pageCount(20, 20), 1);
  assert.equal(admin.pageCount(21, 20), 2);
  assert.equal(admin.pageCount(100, 20), 5);
});

// A zero or unparsable size must not divide by zero into Infinity pages.
test("pageCount tolerates a nonsensical page size", () => {
  assert.equal(admin.pageCount(50, 0), 1);
  assert.equal(admin.pageCount(50, -1), 1);
  assert.equal(admin.pageCount(50, NaN), 1);
});

// The page is clamped to the list: a list that shrinks under the operator (a model
// aging out of the retained window) must not leave them on a page with no rows.
test("pageWindow clamps a page past the end", () => {
  const win = admin.pageWindow(45, 9, 20);
  assert.equal(win.pages, 3);
  assert.equal(win.page, 3);
  assert.equal(win.start, 40);
  assert.equal(win.end, 45);
});

test("pageWindow clamps a page below the start", () => {
  const win = admin.pageWindow(45, 0, 20);
  assert.equal(win.page, 1);
  assert.equal(win.start, 0);
  assert.equal(win.end, 20);
});

// The last page is a partial slice: the bounds have to stop at the row count, or
// the renderer would emit undefined rows.
test("pageWindow bounds the last page to the row count", () => {
  const win = admin.pageWindow(41, 3, 20);
  assert.equal(win.start, 40);
  assert.equal(win.end, 41);
});

test("pageWindow of an empty list yields one empty page", () => {
  const win = admin.pageWindow(0, 1, 20);
  assert.deepEqual(win, { page: 1, pages: 1, start: 0, end: 0, total: 0 });
});

// Every pager button has to advance the same state, including from the edges.
test("applyPagerAction moves between the edges", () => {
  const total = 45;
  const state = { page: 2, size: 20 };
  assert.equal(admin.applyPagerAction(state, "next", total).page, 3);
  assert.equal(admin.applyPagerAction(state, "last", total).page, 3);
  assert.equal(admin.applyPagerAction(state, "prev", total).page, 2);
  assert.equal(admin.applyPagerAction(state, "first", total).page, 1);
  // Next on the last page stays put rather than running off the end.
  assert.equal(admin.applyPagerAction({ page: 3, size: 20 }, "next", total).page, 3);
  assert.equal(admin.applyPagerAction({ page: 1, size: 20 }, "prev", total).page, 1);
});

// The bar has to disable the buttons that would do nothing, so the operator is not
// left clicking a control that silently has no effect.
test("pagerHTML disables the buttons at the edges", () => {
  // A button's attributes carry data-pager too, so match the whole tag.
  const disabled = (html, action) =>
    new RegExp('<button[^>]*data-pager-action="' + action + '"[^>]*\\bdisabled>').test(html);

  const first = admin.pagerHTML("models", { page: 1, pages: 3, start: 0, end: 20, total: 45 }, 20);
  assert.ok(disabled(first, "first"), "first must be disabled on page 1");
  assert.ok(disabled(first, "prev"), "prev must be disabled on page 1");
  assert.ok(!disabled(first, "next"), "next must stay enabled on page 1");

  const last = admin.pagerHTML("models", { page: 3, pages: 3, start: 40, end: 45, total: 45 }, 20);
  assert.ok(disabled(last, "next"), "next must be disabled on the last page");
  assert.ok(disabled(last, "last"), "last must be disabled on the last page");
  assert.ok(!disabled(last, "prev"), "prev must stay enabled on the last page");
});

test("pagerHTML reports the position and the total", () => {
  const html = admin.pagerHTML("failures", { page: 2, pages: 4, start: 20, end: 40, total: 73 }, 20);
  assert.match(html, /第 2 \/ 4 页/);
  assert.match(html, /共 73 条/);
});

// The size selector has to show the size in force, or changing it would look like
// it did not take.
test("pagerHTML marks the current page size", () => {
  const html = admin.pagerHTML("models", { page: 1, pages: 1, start: 0, end: 10, total: 10 }, 50);
  assert.match(html, /<option value="50" selected>/);
  assert.doesNotMatch(html, /<option value="20" selected>/);
});

// ---- page routing ----

// The four views are separate pages. An unknown or empty hash must land on the
// first one rather than showing nothing.
test("normalisePage maps an unknown hash to the first view", () => {
  assert.equal(admin.normalisePage(""), "usage");
  assert.equal(admin.normalisePage("#"), "usage");
  assert.equal(admin.normalisePage("#/nope"), "usage");
  assert.equal(admin.normalisePage("#nope"), "usage");
});

test("normalisePage accepts every view it advertises", () => {
  admin.viewPages.forEach((name) => {
    assert.equal(admin.normalisePage("#/" + name), name);
    assert.equal(admin.normalisePage(name), name);
  });
});

test("the view list covers the four requested pages", () => {
  assert.deepEqual(admin.viewPages.slice().sort(), ["errors", "main", "schemes", "usage"]);
});

// ---- paged editing ----

// A paged editor only has one page of rows in the DOM, so the save payload must
// carry the aliases from the other pages too. Reading the DOM alone would delete
// every alias the operator was not looking at — the worst failure this paging
// could introduce, and a silent one.
test("readRouting keeps the aliases on other pages", () => {
  const path = require.resolve("./admin.js");
  const savedWindow = global.window;
  // The module exports helpers only when no DOM exists at require time.
  delete require.cache[path];
  global.window = { addEventListener() {}, localStorage: { getItem: () => null, setItem() {} } };
  const isolated = require("./admin.js");

  const rows = [];
  const rowEl = (alias, model) => ({
    querySelector: (sel) => ({
      ".r-alias": { value: alias },
      ".r-model": { value: model },
      ".r-image": { checked: false },
    })[sel],
  });
  global.document = {
    querySelectorAll: (sel) => (sel === "#routing .route-row" ? rows : []),
    getElementById: () => null,
  };

  try {
    isolated.__setRoutingEntries(
      ["a0", "a1", "a2", "a3", "a4", "a5"].map((a, i) => ({ alias: a, model: "m" + i })),
      { page: 2, size: 2 },
    );
    // The DOM holds page 2 only; the operator edits a2 there.
    rows.push(rowEl("a2", "m2-edited"), rowEl("a3", "m3"));

    const out = isolated.__readRouting();
    assert.equal(out.error, null);
    assert.deepEqual(out.entries.map((e) => e.alias), ["a0", "a1", "a2", "a3", "a4", "a5"]);
    // The edit made on the visible page is the one that lands; the rest are carried.
    assert.equal(out.entries[2].model, "m2-edited");
    assert.equal(out.entries[0].model, "m0");
    assert.equal(out.entries[5].model, "m5");
  } finally {
    delete require.cache[path];
    global.window = savedWindow;
    delete global.document;
  }
});

// A half-filled row on the visible page still fails the save, even though the rest
// of the table is carried over.
test("readRouting still reports a half-filled visible row", () => {
  const path = require.resolve("./admin.js");
  delete require.cache[path];
  global.window = { addEventListener() {}, localStorage: { getItem: () => null, setItem() {} } };
  const isolated = require("./admin.js");

  const rows = [{
    querySelector: (sel) => ({
      ".r-alias": { value: "only-model" },
      ".r-model": { value: "" },
      ".r-image": { checked: false },
    })[sel],
  }];
  global.document = {
    querySelectorAll: (sel) => (sel === "#routing .route-row" ? rows : []),
    getElementById: () => null,
  };

  try {
    isolated.__setRoutingEntries([], { page: 1, size: 20 });
    const out = isolated.__readRouting();
    assert.match(out.error, /缺少上游模型名/);
  } finally {
    delete require.cache[path];
    delete global.document;
  }
});
