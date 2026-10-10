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

// The profile list a save submits is built entirely from these helpers, so a
// filtering bug here shows up as a rejected save rather than as a silent no-op.
const PROFILES = [
  { name: "anthropic", protocol: "anthropic", url: "https://a.example/api", builtin: true },
  { name: "openai", protocol: "openai", url: "https://o.example/v1", builtin: true },
  { name: "deepseek", protocol: "anthropic", url: "https://api.deepseek.com/anthropic" },
  { name: "local", protocol: "openai", url: "http://127.0.0.1:9000/v1" },
];

test("upstreamOptions keeps the empty default choice first", () => {
  const opts = admin.upstreamOptions(PROFILES);
  assert.equal(opts[0].value, "");
  assert.deepEqual(
    opts.slice(1).map((o) => o.value),
    ["anthropic", "openai", "deepseek", "local"],
  );
});

// An empty selector is not the same as naming "anthropic": it means "inherit
// default_upstream", and collapsing the two would freeze an inherited value into
// the file on the next save.
test("upstreamOptions lists every profile including the builtin ones", () => {
  const opts = admin.upstreamOptions(PROFILES);
  assert.equal(opts.length, PROFILES.length + 1);
  assert.ok(opts.some((o) => o.value === "anthropic"));
  assert.ok(opts.some((o) => o.value === "deepseek"));
});

test("upstreamOptions marks the builtin profiles in the label", () => {
  const opts = admin.upstreamOptions(PROFILES);
  const byValue = Object.fromEntries(opts.map((o) => [o.value, o]));
  assert.match(byValue["anthropic"].label, /内置/);
  assert.doesNotMatch(byValue["deepseek"].label, /内置/);
  // The URL is surfaced as the option's tooltip, so two profiles on the same
  // protocol are still tellable apart.
  assert.equal(byValue["deepseek"].title, "https://api.deepseek.com/anthropic");
});

test("upstreamOptions tolerates a missing profile list", () => {
  assert.equal(admin.upstreamOptions(undefined).length, 1);
  assert.equal(admin.upstreamOptions(null)[0].value, "");
});

// The VLM describe pass speaks anthropic and the chat passthrough openai, so each
// selector may only offer profiles of its own protocol — anything else is a save
// the server would reject.
test("proxySelectorOptions filters to the caller's protocol", () => {
  const vlm = admin.proxySelectorOptions(PROFILES, "anthropic").map((o) => o.value);
  assert.deepEqual(vlm, ["", "anthropic", "deepseek"]);

  const chat = admin.proxySelectorOptions(PROFILES, "openai").map((o) => o.value);
  assert.deepEqual(chat, ["", "openai", "local"]);
});

test("proxySelectorOptions offers the implicit profile when nothing matches", () => {
  const only = admin.proxySelectorOptions([{ name: "d", protocol: "anthropic" }], "openai");
  assert.deepEqual(only.map((o) => o.value), [""]);
});

test("profileOptions with no protocol keeps every profile", () => {
  assert.equal(admin.profileOptions(PROFILES).length, PROFILES.length);
  assert.deepEqual(admin.profileOptions(undefined).length, 0);
});

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

// ---- config form: the payload the save posts ----

// A blank key field must read as "keep": the page never receives a stored key, so
// anything else would either wipe it or force the operator to retype it.
test("profilePayloadFromRow reads a blank key as keep", () => {
  const row = admin.profilePayloadFromRow({
    name: "  deepseek  ",
    protocol: "anthropic",
    url: "  https://api.deepseek.com/anthropic  ",
    key: "",
    keyCleared: false,
    keyEnv: "  DEEPSEEK_API_KEY  ",
    headerTimeout: "30",
    bodyIdle: "",
    maxRetries: "4",
  });
  assert.deepEqual(row, {
    name: "deepseek",
    protocol: "anthropic",
    url: "https://api.deepseek.com/anthropic",
    key_action: "keep",
    key: "",
    key_env: "DEEPSEEK_API_KEY",
    header_timeout_seconds: 30,
    body_idle_seconds: 0,
    max_retries: 4,
  });
});

test("profilePayloadFromRow reads a typed key as set", () => {
  const row = admin.profilePayloadFromRow({ name: "p", protocol: "openai", url: "http://x/y", key: "sk-new" });
  assert.equal(row.key_action, "set");
  assert.equal(row.key, "sk-new");
});

test("profilePayloadFromRow lets the clear box win over a typed key", () => {
  const row = admin.profilePayloadFromRow({ name: "p", key: "sk-typed", keyCleared: true });
  assert.equal(row.key_action, "clear");
});

// The validator rejects a negative timeout, so a field the browser reports as
// empty or unparsable has to become 0 rather than NaN or a negative.
test("profilePayloadFromRow normalises unparsable numbers to zero", () => {
  const row = admin.profilePayloadFromRow({ name: "p", headerTimeout: "", bodyIdle: "abc", maxRetries: "-5" });
  assert.equal(row.header_timeout_seconds, 0);
  assert.equal(row.body_idle_seconds, 0);
  assert.equal(row.max_retries, -5, "a negative value is passed through for the server to reject");
});

test("profilePayloadFromRow tolerates absent fields", () => {
  const row = admin.profilePayloadFromRow({});
  assert.equal(row.name, "");
  assert.equal(row.key_action, "keep");
  assert.equal(row.key, "");
  assert.equal(row.header_timeout_seconds, 0);
});

// An empty routing row is a line the operator left behind; submitting it would be
// rejected as an empty alias, so it has to be dropped instead.
test("routePayloadFromRow drops an empty row", () => {
  assert.equal(admin.routePayloadFromRow({ alias: "  ", model: "  " }), null);
  assert.equal(admin.routePayloadFromRow({}), null);
});

test("routePayloadFromRow trims and keeps a filled row", () => {
  assert.deepEqual(
    admin.routePayloadFromRow({ alias: " sonnet ", model: " deepseek-chat ", upstream: "deepseek", supportsImage: true }),
    { alias: "sonnet", model: "deepseek-chat", upstream: "deepseek", supports_image: true },
  );
  assert.equal(admin.routePayloadFromRow({ alias: "x", model: "y" }).supports_image, false);
});
