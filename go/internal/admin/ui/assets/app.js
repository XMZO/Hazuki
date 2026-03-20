(() => {
  const qs = (sel, root = document) => root.querySelector(sel);
  const qsa = (sel, root = document) => Array.from(root.querySelectorAll(sel));

  const tKey = (key, fallback = "") => {
    const dict = window.HazukiI18n || {};
    const v = dict["js." + key] || dict[key];
    if (typeof v === "string" && v.trim() !== "") return v;
    return fallback || key;
  };

  const replaceAll = (s, search, replacement) => (s || "").toString().split(search).join(replacement);

  const tFmt = (key, fallback, vars) => {
    let out = tKey(key, fallback);
    if (!vars || typeof vars !== "object") return out;
    for (const [k, v] of Object.entries(vars)) {
      out = replaceAll(out, "{" + k + "}", String(v));
    }
    return out;
  };

  const THEME_KEY = "hazuki_theme";

  const normalizeTheme = (v) => {
    const t = (v || "").toString().trim().toLowerCase();
    if (t === "dark" || t === "light") return t;
    return "auto";
  };

  const getTheme = () => {
    try {
      return normalizeTheme(localStorage.getItem(THEME_KEY));
    } catch {
      return "auto";
    }
  };

  const applyTheme = (theme) => {
    const t = normalizeTheme(theme);
    const root = document.documentElement;
    if (!root) return;
    if (t === "dark" || t === "light") {
      root.setAttribute("data-theme", t);
    } else {
      root.removeAttribute("data-theme");
    }
  };

  const themeLabel = (theme) => {
    const t = normalizeTheme(theme);
    if (t === "dark") return tKey("theme.dark", "Dark");
    if (t === "light") return tKey("theme.light", "Light");
    return tKey("theme.auto", "Auto");
  };

  const updateThemeToggle = () => {
    const btn = qs("[data-theme-toggle]");
    if (!btn) return;
    const t = getTheme();
    const label = themeLabel(t);
    btn.textContent = label;
    const title = tFmt("theme.toggleHint", "Theme: {mode} (click to toggle)", { mode: label });
    btn.title = title;
    btn.setAttribute("aria-label", title);
  };

  const setTheme = (theme) => {
    const t = normalizeTheme(theme);
    try {
      if (t === "auto") localStorage.removeItem(THEME_KEY);
      else localStorage.setItem(THEME_KEY, t);
    } catch {
      // ignore
    }
    applyTheme(t);
    updateThemeToggle();
  };

  const nextTheme = (cur) => {
    const t = normalizeTheme(cur);
    if (t === "auto") return "dark";
    if (t === "dark") return "light";
    return "auto";
  };

  const onThemeToggleClick = (e) => {
    const btn = e.target instanceof Element ? e.target.closest("[data-theme-toggle]") : null;
    if (!btn) return;
    e.preventDefault();
    setTheme(nextTheme(getTheme()));
  };

  const canPjax = () =>
    typeof window.fetch === "function" &&
    typeof window.DOMParser === "function" &&
    !!(window.history && window.history.pushState);

  const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
  const nextFrame = () => new Promise((r) => requestAnimationFrame(() => r()));

  const waitTransitionEnd = async (el, maxMs) => {
    if (!el) return;
    if (window.matchMedia && window.matchMedia("(prefers-reduced-motion: reduce)").matches) {
      return;
    }
    await Promise.race([
      new Promise((resolve) => {
        const onEnd = (evt) => {
          if (evt.target !== el) return;
          cleanup();
        };
        const cleanup = () => {
          el.removeEventListener("transitionend", onEnd);
          resolve();
        };
        el.addEventListener("transitionend", onEnd);
        setTimeout(cleanup, maxMs);
      }),
      sleep(maxMs),
    ]);
  };

  const toInt = (v) => {
    const n = Number(v);
    if (!Number.isFinite(n)) return 0;
    return Math.trunc(n);
  };

  const formatBytes = (v) => {
    const n = Number(v);
    if (!Number.isFinite(n) || n < 0) return "-";
    const units = ["B", "KB", "MB", "GB", "TB"];
    let val = n;
    let idx = 0;
    while (val >= 1024 && idx < units.length - 1) {
      val /= 1024;
      idx += 1;
    }
    const digits = idx === 0 ? 0 : val >= 100 ? 0 : val >= 10 ? 1 : 2;
    return val.toFixed(digits) + " " + units[idx];
  };

  const formatBps = (v) => {
    const n = Number(v);
    if (!Number.isFinite(n) || n < 0) return "-";
    return formatBytes(n) + "/s";
  };

  let dashTimer = null;
  let dashPrevAgg = null;
  let dashPrevAt = 0;
  let dashAbort = null;
  let dashSeq = 0;

  let trafficTimer = null;
  let trafficAbort = null;
  let trafficSeq = 0;

  let systemTimer = null;
  let systemAbort = null;
  let systemSeq = 0;

  const stopDashboardStats = () => {
    if (dashTimer) {
      clearInterval(dashTimer);
      dashTimer = null;
    }
    dashPrevAgg = null;
    dashPrevAt = 0;

    if (dashAbort && typeof dashAbort.abort === "function") {
      try {
        dashAbort.abort();
      } catch {
        // ignore
      }
    }
    dashAbort = null;
  };

  const snapFrom = (v) => {
    const obj = v && typeof v === "object" ? v : {};
    return {
      bytesIn: toInt(obj.bytesIn),
      bytesOut: toInt(obj.bytesOut),
      requests: toInt(obj.requests),
    };
  };

  const sumSnaps = (a, b) => ({
    bytesIn: toInt((a && a.bytesIn) || 0) + toInt((b && b.bytesIn) || 0),
    bytesOut: toInt((a && a.bytesOut) || 0) + toInt((b && b.bytesOut) || 0),
    requests: toInt((a && a.requests) || 0) + toInt((b && b.requests) || 0),
  });

  const aggregateServices = (services) => {
    const map = services && typeof services === "object" ? services : {};

    const torcherino = snapFrom(map.torcherino);
    const cdnjs = snapFrom(map.cdnjs);
    const sakuya = snapFrom(map.sakuya);
    const patchouli = snapFrom(map.patchouli);

    let git = { bytesIn: 0, bytesOut: 0, requests: 0 };
    for (const [k, val] of Object.entries(map)) {
      if (k === "git" || k.startsWith("git:")) {
        git = sumSnaps(git, snapFrom(val));
      }
    }

    const total = sumSnaps(sumSnaps(sumSnaps(sumSnaps(torcherino, cdnjs), git), sakuya), patchouli);

    return {
      torcherino,
      cdnjs,
      git,
      sakuya,
      patchouli,
      total,
    };
  };

  const updateTrafficDom = (agg, prev, dt) => {
    const card = qs("#hz-traffic-card");
    if (!card) return;

    for (const row of qsa("tr[data-hz-svc]", card)) {
      const key = (row.getAttribute("data-hz-svc") || "").trim();
      const cur = (agg && agg[key]) || { bytesIn: 0, bytesOut: 0, requests: 0 };
      const pre = prev && prev[key];

      const downTotal = formatBytes(cur.bytesOut);
      const upTotal = formatBytes(cur.bytesIn);

      let downRate = "-";
      let upRate = "-";
      if (pre && dt > 0) {
        const dOut = Math.max(0, toInt(cur.bytesOut) - toInt(pre.bytesOut));
        const dIn = Math.max(0, toInt(cur.bytesIn) - toInt(pre.bytesIn));
        downRate = formatBps(dOut / dt);
        upRate = formatBps(dIn / dt);
      }

      for (const el of qsa("[data-hz-field]", row)) {
        const f = (el.getAttribute("data-hz-field") || "").trim();
        if (!f) continue;
        if (f === "downTotal") el.textContent = downTotal;
        else if (f === "upTotal") el.textContent = upTotal;
        else if (f === "downRate") el.textContent = downRate;
        else if (f === "upRate") el.textContent = upRate;
      }
    }
  };

  const updateRedisDom = (redis) => {
    const card = qs("#hz-redis-card");
    if (!card) return;

    const pill = qs("#hz-redis-pill", card);
    if (pill) {
      pill.classList.remove("ok", "err");
      const status = (redis && redis.status ? String(redis.status) : "").toLowerCase();

      if (status === "ok") {
        pill.classList.add("ok");
        const latency = toInt(redis.latencyMS);
        const addr = (redis.addr || "").toString();
        pill.textContent = tKey("redis.ok", "Redis OK") + " · " + latency + "ms" + (addr ? " · " + addr : "");
      } else if (status === "error") {
        pill.classList.add("err");
        const addr = (redis.addr || "").toString();
        pill.textContent = tKey("redis.error", "Redis error") + (addr ? " · " + addr : "");
      } else {
        pill.textContent = tKey("redis.notConfigured", "Redis not configured");
      }
    }

    const hits = toInt(redis && redis.keyspaceHits);
    const misses = toInt(redis && redis.keyspaceMisses);
    const total = hits + misses;
    const hitRate = total > 0 ? ((hits / total) * 100).toFixed(1) + "%" : "-";

    for (const el of qsa("[data-hz-redis-field]", card)) {
      const f = (el.getAttribute("data-hz-redis-field") || "").trim();
      if (!f) continue;

      if (f === "dbSize") el.textContent = String(toInt(redis && redis.dbSize));
      else if (f === "usedMemoryHuman") el.textContent = (redis && redis.usedMemoryHuman ? String(redis.usedMemoryHuman) : "-");
      else if (f === "keyspaceHits") el.textContent = String(hits);
      else if (f === "keyspaceMisses") el.textContent = String(misses);
      else if (f === "hitRate") el.textContent = hitRate;
    }
  };

  const pollDashboardStats = async () => {
    const page = (document.body && document.body.getAttribute("data-page")) || "";
    if (page !== "dashboard") {
      stopDashboardStats();
      return;
    }

    const mySeq = (dashSeq += 1);
    if (dashAbort && typeof dashAbort.abort === "function") {
      try {
        dashAbort.abort();
      } catch {
        // ignore
      }
    }

    let signal = null;
    if (typeof AbortController === "function") {
      dashAbort = new AbortController();
      signal = dashAbort.signal;
    } else {
      dashAbort = null;
    }

    try {
      const opts = { method: "GET", headers: { Accept: "application/json" }, credentials: "same-origin" };
      if (signal) opts.signal = signal;

      const resp = await fetch("/_hazuki/stats", opts);
      if (mySeq !== dashSeq) return;

      const ct = (resp.headers.get("content-type") || "").toLowerCase();
      if (!resp.ok || !ct.includes("application/json")) {
        return;
      }

      const payload = await resp.json();
      if (mySeq !== dashSeq) return;

      const nowAt = performance && typeof performance.now === "function" ? performance.now() : Date.now();
      const dt = dashPrevAt > 0 ? Math.max(0.001, (nowAt - dashPrevAt) / 1000) : 0;

      const services = payload && payload.services ? payload.services : {};
      const canon = aggregateServices(services);

      updateTrafficDom(canon, dashPrevAgg, dt);

      updateRedisDom(payload && payload.redis ? payload.redis : null);

      dashPrevAgg = canon;
      dashPrevAt = nowAt;
    } catch {
      if (dashAbort && dashAbort.signal && dashAbort.signal.aborted) {
        return;
      }
    }
  };

  const ensureDashboardStats = () => {
    const page = (document.body && document.body.getAttribute("data-page")) || "";
    if (page !== "dashboard") {
      stopDashboardStats();
      return;
    }

    if (dashTimer) return;
    dashPrevAgg = null;
    dashPrevAt = 0;
    pollDashboardStats();
    dashTimer = setInterval(pollDashboardStats, 2000);
  };

  const stopTrafficPage = () => {
    if (trafficTimer) {
      clearInterval(trafficTimer);
      trafficTimer = null;
    }
    if (trafficAbort && typeof trafficAbort.abort === "function") {
      try {
        trafficAbort.abort();
      } catch {
        // ignore
      }
    }
    trafficAbort = null;
  };

  const stopSystemPage = () => {
    if (systemTimer) {
      clearInterval(systemTimer);
      systemTimer = null;
    }
    if (systemAbort && typeof systemAbort.abort === "function") {
      try {
        systemAbort.abort();
      } catch {
        // ignore
      }
    }
    systemAbort = null;
  };

  const getObjectPath = (obj, path) => {
    const parts = (path || "").toString().split(".");
    let cur = obj;
    for (const part of parts) {
      const key = (part || "").trim();
      if (!key || !cur || typeof cur !== "object") return null;
      cur = cur[key];
    }
    return cur;
  };

  const setRewriteTabActive = (tab) => {
    const root = qs("[data-hz-rewrite-tabs]");
    if (!root) return;
    const activeTab = (tab || "").trim() || "gitHTML";
    for (const btn of qsa("[data-hz-rewrite-tab]", root)) {
      const key = (btn.getAttribute("data-hz-rewrite-tab") || "").trim();
      btn.classList.toggle("active", key === activeTab);
    }
    for (const panel of qsa("[data-hz-rewrite-panel]")) {
      const key = (panel.getAttribute("data-hz-rewrite-panel") || "").trim();
      panel.hidden = key !== activeTab;
    }
  };

  const getRewriteTab = () => {
    const root = qs("[data-hz-rewrite-tabs]");
    if (!root) return "gitHTML";
    const active = qs("[data-hz-rewrite-tab].active", root);
    const key = active ? (active.getAttribute("data-hz-rewrite-tab") || "").trim() : "";
    return key || "gitHTML";
  };

  const updateSystemRewriteRuntimeDom = (info) => {
    const card = qs("[data-hz-rewrite-runtime-card]");
    if (!card) return;

    for (const el of qsa("[data-hz-rewrite-field]", card)) {
      const path = (el.getAttribute("data-hz-rewrite-field") || "").trim();
      if (!path) continue;
      const value = getObjectPath(info, path);
      el.textContent = typeof value === "string" && value.trim() !== "" ? value : "-";
    }

    for (const pill of qsa("[data-hz-rewrite-enabled-pill]", card)) {
      const key = (pill.getAttribute("data-hz-rewrite-enabled-pill") || "").trim();
      if (!key) continue;
      const enabled = !!getObjectPath(info, key + ".enabled");
      pill.classList.toggle("ok", enabled);
      pill.classList.toggle("err", !enabled);
      pill.textContent = enabled
        ? tKey("common.enabled", "Enabled")
        : tKey("common.disabled", "Disabled");
    }
  };

  const pollSystemRewriteRuntime = async () => {
    const page = (document.body && document.body.getAttribute("data-page")) || "";
    if (page !== "system") {
      stopSystemPage();
      return;
    }

    const mySeq = (systemSeq += 1);
    if (systemAbort && typeof systemAbort.abort === "function") {
      try {
        systemAbort.abort();
      } catch {
        // ignore
      }
    }

    let signal = null;
    if (typeof AbortController === "function") {
      systemAbort = new AbortController();
      signal = systemAbort.signal;
    } else {
      systemAbort = null;
    }

    try {
      const opts = { method: "GET", headers: { Accept: "application/json" }, credentials: "same-origin" };
      if (signal) opts.signal = signal;

      const resp = await fetch("/_hazuki/system/rewrite-runtime", opts);
      if (mySeq !== systemSeq) return;

      const ct = (resp.headers.get("content-type") || "").toLowerCase();
      if (!resp.ok || !ct.includes("application/json")) {
        return;
      }

      const payload = await resp.json();
      if (mySeq !== systemSeq) return;

      updateSystemRewriteRuntimeDom(payload && payload.rewriteRuntime ? payload.rewriteRuntime : null);
    } catch {
      if (systemAbort && systemAbort.signal && systemAbort.signal.aborted) {
        return;
      }
    }
  };

  const ensureSystemPage = () => {
    const page = (document.body && document.body.getAttribute("data-page")) || "";
    if (page !== "system") {
      stopSystemPage();
      return;
    }

    const root = qs("[data-hz-rewrite-tabs]");
    if (root) {
      for (const btn of qsa("[data-hz-rewrite-tab]", root)) {
        if (btn.__hzRewriteBound) continue;
        btn.__hzRewriteBound = true;
        btn.addEventListener("click", (e) => {
          e.preventDefault();
          const key = (btn.getAttribute("data-hz-rewrite-tab") || "").trim();
          if (!key) return;
          setRewriteTabActive(key);
        });
      }
      setRewriteTabActive(getRewriteTab());
    }

    if (systemTimer) return;
    pollSystemRewriteRuntime();
    systemTimer = setInterval(pollSystemRewriteRuntime, 5000);
  };

  const pad2 = (n) => String(n).padStart(2, "0");

  const getTimeZoneSpec = () => ((window && window.HazukiTimeZone) || "").toString().trim();

  const parseTimeZoneSpec = (spec) => {
    const raw = (spec || "").toString().trim();
    if (!raw) return { mode: "auto", offsetMinutes: 0, label: "auto" };

    const lower = raw.toLowerCase();
    if (lower === "auto" || lower === "browser" || lower === "local") {
      return { mode: "auto", offsetMinutes: 0, label: "auto" };
    }
    if (lower === "utc" || lower === "z") {
      return { mode: "utc", offsetMinutes: 0, label: "UTC" };
    }

    let s = raw;
    if (lower.startsWith("utc")) {
      s = raw.slice(3).trim();
      if (!s) return { mode: "utc", offsetMinutes: 0, label: "UTC" };
    }

    const m = s.match(/^([+-])\s*(\d{1,2})(?::?\s*(\d{2}))?$/);
    if (!m) return { mode: "auto", offsetMinutes: 0, label: "auto" };

    const sign = m[1] === "-" ? -1 : 1;
    const hh = Number(m[2]);
    const mm = m[3] ? Number(m[3]) : 0;
    if (!Number.isFinite(hh) || !Number.isFinite(mm)) return { mode: "auto", offsetMinutes: 0, label: "auto" };
    if (hh < 0 || hh > 14 || mm < 0 || mm > 59) return { mode: "auto", offsetMinutes: 0, label: "auto" };

    const offsetMinutes = sign * (hh * 60 + mm);
    if (offsetMinutes < -14 * 60 || offsetMinutes > 14 * 60) return { mode: "auto", offsetMinutes: 0, label: "auto" };

    const label = (offsetMinutes === 0 ? "UTC" : `${sign === 1 ? "+" : "-"}${pad2(hh)}:${pad2(mm)}`);
    return { mode: "offset", offsetMinutes, label };
  };

  let tzCacheSpec = null;
  let tzCacheInfo = null;
  const getTimeZoneInfo = () => {
    const spec = getTimeZoneSpec();
    if (spec === tzCacheSpec && tzCacheInfo) return tzCacheInfo;
    tzCacheSpec = spec;
    tzCacheInfo = parseTimeZoneSpec(spec);
    return tzCacheInfo;
  };

  const datePartsFromMs = (ms) => {
    const tz = getTimeZoneInfo();
    let d = new Date(ms);
    let useUTC = false;

    if (tz.mode === "utc") {
      useUTC = true;
    } else if (tz.mode === "offset") {
      d = new Date(ms + tz.offsetMinutes * 60 * 1000);
      useUTC = true;
    }

    if (!Number.isFinite(d.getTime())) return null;

    const y = useUTC ? d.getUTCFullYear() : d.getFullYear();
    const m = pad2((useUTC ? d.getUTCMonth() : d.getMonth()) + 1);
    const day = pad2(useUTC ? d.getUTCDate() : d.getDate());
    const h = pad2(useUTC ? d.getUTCHours() : d.getHours());
    const min = pad2(useUTC ? d.getUTCMinutes() : d.getMinutes());
    const sec = pad2(useUTC ? d.getUTCSeconds() : d.getSeconds());
    return { y, m, day, h, min, sec, tz };
  };

  const formatBucketTime = (kind, ts) => {
    const n = Number(ts);
    if (!Number.isFinite(n) || n <= 0) return "-";
    const parts = datePartsFromMs(n * 1000);
    if (!parts) return "-";
    if (kind === "year") return String(parts.y);
    if (kind === "month") return `${parts.y}-${parts.m}`;
    if (kind === "day") return `${parts.y}-${parts.m}-${parts.day}`;
    return `${parts.y}-${parts.m}-${parts.day} ${parts.h}:${parts.min}`;
  };

  const normalizeIsoForDate = (iso) => {
    let s = (iso || "").toString().trim();
    if (!s) return "";

    // RFC3339Nano -> keep at most 3 fractional digits for JS Date parsing compatibility.
    s = s.replace(/(\.\d{3})\d+(?=Z|[+-]\d{2}:?\d{2}$)/, "$1");
    return s;
  };

  const parseIsoToMs = (iso) => {
    const s0 = (iso || "").toString().trim();
    if (!s0) return NaN;

    let s = normalizeIsoForDate(s0);
    let ms = Date.parse(s);
    if (Number.isFinite(ms)) return ms;

    // Fallback: drop all fractional seconds.
    s = s.replace(/\.\d+(?=Z|[+-]\d{2}:?\d{2}$)/, "");
    ms = Date.parse(s);
    return ms;
  };

  const formatInstantMs = (ms) => {
    const parts = datePartsFromMs(ms);
    if (!parts) return "-";
    return `${parts.y}-${parts.m}-${parts.day} ${parts.h}:${parts.min}:${parts.sec}`;
  };

  const applyTimeFormatting = () => {
    for (const el of qsa("[data-hz-iso]")) {
      const iso = (el.getAttribute("data-hz-iso") || "").trim();
      if (!iso) continue;
      const ms = parseIsoToMs(iso);
      if (!Number.isFinite(ms) || ms <= 0) continue;
      el.textContent = formatInstantMs(ms);
    }
  };

  const setTrafficKindActive = (kind) => {
    const root = qs("[data-hz-traffic-kind]");
    if (!root) return;
    for (const btn of qsa("[data-hz-kind]", root)) {
      const k = (btn.getAttribute("data-hz-kind") || "").trim();
      btn.classList.toggle("active", k === kind);
    }
  };

  const getTrafficKind = () => {
    const root = qs("[data-hz-traffic-kind]");
    if (!root) return "hour";
    const active = qs("[data-hz-kind].active", root);
    const k = active ? (active.getAttribute("data-hz-kind") || "").trim() : "";
    return k || "hour";
  };

  const getTrafficService = () => {
    const sel = qs("#hzTrafficSvc");
    if (!(sel instanceof HTMLSelectElement)) return "total";
    return (sel.value || "").trim() || "total";
  };

  const buildLinePath = ({ values, width, height, pad, max }) => {
    const n = values.length;
    if (n === 0) return "";
    const innerW = Math.max(1, width - pad * 2);
    const innerH = Math.max(1, height - pad * 2);
    const denom = Math.max(1, max);

    // When there's only one bucket, draw a flat line so it's still visible.
    if (n === 1) {
      const v = Math.max(0, Number(values[0]) || 0);
      const y = pad + (1 - Math.min(1, v / denom)) * innerH;
      const x0 = pad;
      const x1 = pad + innerW;
      return `M${x0.toFixed(2)} ${y.toFixed(2)} L${x1.toFixed(2)} ${y.toFixed(2)}`;
    }

    const step = innerW / (n - 1);

    let d = "";
    for (let i = 0; i < n; i += 1) {
      const x = pad + step * i;
      const v = Math.max(0, Number(values[i]) || 0);
      const y = pad + (1 - Math.min(1, v / denom)) * innerH;
      d += (i === 0 ? "M" : "L") + x.toFixed(2) + " " + y.toFixed(2) + " ";
    }
    return d.trim();
  };

  const buildGridPath = ({ width, height, pad }) => {
    const innerW = Math.max(1, width - pad * 2);
    const innerH = Math.max(1, height - pad * 2);

    let d = "";
    for (let i = 1; i <= 3; i += 1) {
      const y = pad + (innerH * i) / 4;
      d += "M" + pad + " " + y.toFixed(2) + " L" + (pad + innerW) + " " + y.toFixed(2) + " ";
    }
    for (let i = 1; i <= 3; i += 1) {
      const x = pad + (innerW * i) / 4;
      d += "M" + x.toFixed(2) + " " + pad + " L" + x.toFixed(2) + " " + (pad + innerH) + " ";
    }
    return d.trim();
  };

  const renderTrafficSeries = (payload) => {
    const kind = (payload && payload.kind ? payload.kind : "").toString();
    const pts = Array.isArray(payload && payload.points) ? payload.points : [];

    const outEl = qs("#hzTrafficOut");
    const inEl = qs("#hzTrafficIn");
    const gridEl = qs("#hzTrafficGrid");
    const outLastEl = qs("#hzTrafficOutLast");
    const inLastEl = qs("#hzTrafficInLast");
    const rangeEl = qs("#hzTrafficRange");
    const tbody = qs("#hzTrafficTableBody");

    if (!(outEl instanceof SVGPathElement) || !(inEl instanceof SVGPathElement) || !(gridEl instanceof SVGPathElement)) {
      return;
    }

    const bytesOut = pts.map((p) => toInt(p && p.bytesOut));
    const bytesIn = pts.map((p) => toInt(p && p.bytesIn));
    const reqs = pts.map((p) => toInt(p && p.requests));

    const max = Math.max(1, ...bytesOut, ...bytesIn);

    const width = 1000;
    const height = 260;
    const pad = 18;

    gridEl.setAttribute("d", buildGridPath({ width, height, pad }));
    outEl.setAttribute("d", buildLinePath({ values: bytesOut, width, height, pad, max }));
    inEl.setAttribute("d", buildLinePath({ values: bytesIn, width, height, pad, max }));

    const last = pts.length ? pts[pts.length - 1] : null;
    if (outLastEl) outLastEl.textContent = formatBytes(toInt(last && last.bytesOut));
    if (inLastEl) inLastEl.textContent = formatBytes(toInt(last && last.bytesIn));

    if (rangeEl) {
      const fromTs = payload && payload.fromTs ? payload.fromTs : 0;
      const toTs = payload && payload.toTs ? payload.toTs : 0;
      rangeEl.textContent =
        formatBucketTime(kind, fromTs) +
        " → " +
        formatBucketTime(kind, toTs) +
        " · " +
        tKey("dashboard.traffic.total", "Total") +
        ": " +
        formatBytes(max);
    }

    if (tbody) {
      tbody.innerHTML = "";
      for (let i = pts.length - 1; i >= 0; i -= 1) {
        const p = pts[i] || {};
        const tr = document.createElement("tr");

        const tdTime = document.createElement("td");
        tdTime.textContent = formatBucketTime(kind, p.startTs);
        tr.appendChild(tdTime);

        const tdOut = document.createElement("td");
        tdOut.innerHTML = "<code>" + formatBytes(toInt(p.bytesOut)) + "</code>";
        tr.appendChild(tdOut);

        const tdIn = document.createElement("td");
        tdIn.innerHTML = "<code>" + formatBytes(toInt(p.bytesIn)) + "</code>";
        tr.appendChild(tdIn);

        const tdReq = document.createElement("td");
        tdReq.innerHTML = "<code>" + String(toInt(p.requests)) + "</code>";
        tr.appendChild(tdReq);

        tbody.appendChild(tr);
      }

      if (!pts.length) {
        const tr = document.createElement("tr");
        const td = document.createElement("td");
        td.colSpan = 4;
        td.className = "muted small";
        td.textContent = "-";
        tr.appendChild(td);
        tbody.appendChild(tr);
      }
    }
  };

  const fetchTrafficSeries = async () => {
    const page = (document.body && document.body.getAttribute("data-page")) || "";
    if (page !== "traffic") return;

    const kind = getTrafficKind();
    const svc = getTrafficService();

    const mySeq = (trafficSeq += 1);

    if (trafficAbort && typeof trafficAbort.abort === "function") {
      try {
        trafficAbort.abort();
      } catch {
        // ignore
      }
    }
    trafficAbort = typeof AbortController === "function" ? new AbortController() : null;

    const url = new URL("/_hazuki/traffic/series", window.location.href);
    url.searchParams.set("kind", kind);
    url.searchParams.set("svc", svc);

    try {
      const resp = await fetch(url.toString(), {
        method: "GET",
        headers: { accept: "application/json" },
        signal: trafficAbort ? trafficAbort.signal : undefined,
      });
      if (mySeq !== trafficSeq) return;
      const ct = (resp.headers.get("content-type") || "").toLowerCase();
      if (!resp.ok || !ct.includes("application/json")) return;

      const payload = await resp.json();
      if (mySeq !== trafficSeq) return;

      renderTrafficSeries(payload);
    } catch {
      if (trafficAbort && trafficAbort.signal && trafficAbort.signal.aborted) {
        return;
      }
    }
  };

  const ensureTrafficPage = () => {
    const page = (document.body && document.body.getAttribute("data-page")) || "";
    if (page !== "traffic") {
      stopTrafficPage();
      return;
    }

    const kindRoot = qs("[data-hz-traffic-kind]");
    if (kindRoot) {
      for (const btn of qsa("[data-hz-kind]", kindRoot)) {
        if (btn.__hzTrafficBound) continue;
        btn.__hzTrafficBound = true;
        btn.addEventListener("click", (e) => {
          e.preventDefault();
          const k = (btn.getAttribute("data-hz-kind") || "").trim();
          if (!k) return;
          setTrafficKindActive(k);
          fetchTrafficSeries();
        });
      }
    }

    const svcSel = qs("#hzTrafficSvc");
    if (svcSel instanceof HTMLSelectElement && !svcSel.__hzTrafficBound) {
      svcSel.__hzTrafficBound = true;
      svcSel.addEventListener("change", () => fetchTrafficSeries());
    }

    const refreshBtn = qs("#hzTrafficRefresh");
    if (refreshBtn instanceof HTMLElement && !refreshBtn.__hzTrafficBound) {
      refreshBtn.__hzTrafficBound = true;
      refreshBtn.addEventListener("click", (e) => {
        e.preventDefault();
        fetchTrafficSeries();
      });
    }

    if (!trafficTimer) {
      fetchTrafficSeries();
      trafficTimer = setInterval(fetchTrafficSeries, 10000);
    }
  };

  const formatJson = (ta, msgEl) => {
    if (!ta) return;
    const raw = (ta.value || "").trim();
    if (!raw) return;
    try {
      const v = JSON.parse(raw);
      ta.value = JSON.stringify(v, null, 2);
      if (msgEl) msgEl.textContent = tKey("json.formatted", "Formatted");
    } catch (e) {
      if (msgEl) {
        msgEl.textContent =
          tKey("json.invalidPrefix", "Invalid JSON: ") +
          (e && e.message ? e.message : tKey("json.parseError", "parse error"));
      }
    }
  };

  const onFormatJsonClick = (e) => {
    const btn =
      e.target instanceof HTMLElement
        ? e.target.closest("[data-format-json]")
        : null;
    if (!btn) return;

    const sel = (btn.getAttribute("data-format-json") || "").trim();
    const row = btn.closest(".field");
    const msg = row ? qs(".json-msg", row) : null;
    const ta = sel ? qs(sel) : row ? row.querySelector("textarea") : null;

    if (ta instanceof HTMLTextAreaElement) {
      formatJson(ta, msg);
    }
  };

  const onTogglePassword = (e) => {
    const el = e.target;
    if (!(el instanceof HTMLInputElement)) return;
    if (!el.matches("[data-toggle-password]")) return;

    const targetSel = (el.getAttribute("data-toggle-password") || "").trim();
    const input = targetSel ? qs(targetSel) : null;
    if (!(input instanceof HTMLInputElement)) return;

    input.type = el.checked ? "text" : "password";
  };

  const extractExt = (requestPath) => {
    const base = ((requestPath || "").split("/").pop() || "").trim();
    const dot = base.lastIndexOf(".");
    if (dot <= 0 || dot === base.length - 1) return "";
    return base.slice(dot + 1).toLowerCase();
  };

  const guessMimeFromPath = (requestPath) => {
    const ext = extractExt(requestPath);
    if (!ext) return "";
    const map = {
      js: "application/javascript",
      mjs: "application/javascript",
      cjs: "application/javascript",
      jsx: "application/javascript",
      css: "text/css",
      html: "text/html",
      htm: "text/html",
      json: "application/json",
      map: "application/json",
      xml: "application/xml",
      yml: "application/x-yaml",
      yaml: "application/x-yaml",
      toml: "application/toml",
      txt: "text/plain",
      md: "text/plain",
      csv: "text/csv",
      wasm: "application/wasm",
      png: "image/png",
      jpg: "image/jpeg",
      jpeg: "image/jpeg",
      gif: "image/gif",
      webp: "image/webp",
      avif: "image/avif",
      svg: "image/svg+xml",
      ico: "image/x-icon",
      mp4: "video/mp4",
      webm: "video/webm",
      mp3: "audio/mpeg",
      wav: "audio/wav",
      ogg: "audio/ogg",
      m4a: "audio/mp4",
      woff2: "font/woff2",
      woff: "font/woff",
      ttf: "font/ttf",
      otf: "font/otf",
      eot: "application/vnd.ms-fontobject",
      m3u: "application/vnd.apple.mpegurl",
      m3u8: "application/vnd.apple.mpegurl",
    };
    return map[ext] || "";
  };

  const updateGitPreview = () => {
    const pathEl = qs("#gitPreviewPath");
    const outEl = qs("#gitPreviewUrl");
    const hintEl = qs("#gitPreviewHint");
    if (!(pathEl instanceof HTMLInputElement) || !(outEl instanceof HTMLInputElement) || !hintEl) return;

    const upstream = (qs('input[name="upstream"]')?.value || "").trim() || "raw.githubusercontent.com";

    let upstreamPath = (qs('input[name="upstreamPath"]')?.value || "").trim();
    if (!upstreamPath) upstreamPath = "/";
    if (!upstreamPath.startsWith("/")) upstreamPath = "/" + upstreamPath;
    if (upstreamPath.length > 1 && upstreamPath.endsWith("/")) upstreamPath = upstreamPath.slice(0, -1);

    const useHttps = !!(qs('input[name="upstreamHttps"]')?.checked);

    let reqPath = (pathEl.value || "").trim();
    if (!reqPath) reqPath = "/";
    if (!reqPath.startsWith("/")) reqPath = "/" + reqPath;

    const joinPath = (prefix, p) => {
      if (!p) p = "/";
      if (p === "/" || p === "") return prefix === "/" ? "" : prefix;
      if (prefix === "/" || prefix === "") return p;
      return prefix + p;
    };

    const scheme = useHttps ? "https" : "http";
    const finalPath = joinPath(upstreamPath, reqPath);
    outEl.value = scheme + "://" + upstream + finalPath;

    const t = guessMimeFromPath(reqPath);
    if (t) {
      hintEl.textContent =
        tKey("gitPreview.mimePrefix", "Guessed Content-Type: ") +
        t +
        tKey("gitPreview.mimeSuffix", " (used for cache classification / type fix)");
      return;
    }
    hintEl.textContent = tKey(
      "gitPreview.noExtHint",
      "No extension: Content-Type follows the upstream response."
    );
  };

  const cdnjsBuiltInDefaultTTL = 86400;
  const cdnjsBuiltInTTLByExt = {
    // Node defaults (kept for compatibility).
    js: 2592000,
    css: 2592000,
    png: 2592000,
    jpg: 2592000,
    jpeg: 2592000,
    gif: 2592000,
    svg: 2592000,
    ico: 2592000,
    woff: 2592000,
    woff2: 2592000,
    ttf: 2592000,
    eot: 2592000,
    webp: 2592000,
    moc3: 2592000,
    map: 2592000,
    cur: 2592000,
    mp4: 604800,
    mp3: 604800,
    pdf: 604800,
    json: 86400,
    xml: 86400,
    txt: 86400,
    html: 3600,

    // Extra common extensions (Go version).
    mjs: 2592000,
    cjs: 2592000,
    wasm: 2592000,
    avif: 2592000,
    apng: 2592000,
    bmp: 2592000,
    tif: 2592000,
    tiff: 2592000,
    otf: 2592000,
    svgz: 2592000,
    webm: 604800,
    m4a: 604800,
    aac: 604800,
    ogg: 604800,
    wav: 604800,
    flac: 604800,
    htm: 3600,
    md: 86400,
    yml: 86400,
    yaml: 86400,
    toml: 86400,
    jsonc: 86400,
  };

  const parseTTLOverrides = (raw) => {
    const maxTTLSeconds = 315360000; // 10 years
    const trimmed = (raw || "").toString().trim();
    if (!trimmed) return { ok: true, map: {} };

    const normExt = (ext) =>
      (ext || "")
        .toString()
        .trim()
        .toLowerCase()
        .replace(/^\./, "");

    if (trimmed.startsWith("{")) {
      try {
        const obj = JSON.parse(trimmed);
        if (!obj || typeof obj !== "object" || Array.isArray(obj)) {
          return {
            ok: false,
            err: tKey("ttlOverrides.errNotObject", "TTL Overrides must be a JSON object"),
            map: {},
          };
        }
        const out = {};
        for (const [k, v] of Object.entries(obj)) {
          const ext = normExt(k);
          if (!ext) continue;
          if (ext === "default") {
            return {
              ok: false,
              err: tKey(
                "ttlOverrides.errDefaultNotAllowed",
                "TTL Overrides: do not use default=...; use Default TTL above"
              ),
              map: {},
            };
          }
          const ttl = Number.parseInt(String(v), 10);
          if (!Number.isFinite(ttl) || ttl < 1 || ttl > maxTTLSeconds) {
            return {
              ok: false,
              err: tFmt(
                "ttlOverrides.errEntryRange",
                "TTL Overrides[{ext}] must be an integer between 1 and {max} (seconds)",
                { ext, max: maxTTLSeconds }
              ),
              map: {},
            };
          }
          out[ext] = ttl;
        }
        return { ok: true, map: out };
      } catch (e) {
        return {
          ok: false,
          err: tFmt(
            "ttlOverrides.errJsonInvalid",
            "TTL Overrides JSON invalid: {msg}",
            {
              msg: e && e.message ? e.message : tKey("json.parseError", "parse error"),
            }
          ),
          map: {},
        };
      }
    }

    const out = {};
    const lines = (raw || "").toString().split(/\r?\n/);
    for (let i = 0; i < lines.length; i += 1) {
      let s = (lines[i] || "").trim();
      if (!s || s.startsWith("#")) continue;

      const hashIdx = s.indexOf("#");
      if (hashIdx !== -1) s = s.slice(0, hashIdx).trim();
      if (!s) continue;

      const sepIdx = s.search(/[=:]/);
      if (sepIdx === -1) {
        return {
          ok: false,
          err: tFmt(
            "ttlOverrides.errLineFormat",
            "TTL Overrides line {line}: use ext=seconds format",
            { line: i + 1 }
          ),
          map: {},
        };
      }

      const ext = normExt(s.slice(0, sepIdx));
      if (!ext) {
        return {
          ok: false,
          err: tFmt("ttlOverrides.errLineEmptyExt", "TTL Overrides line {line}: empty extension", {
            line: i + 1,
          }),
          map: {},
        };
      }
      if (ext === "default") {
        return {
          ok: false,
          err: tFmt(
            "ttlOverrides.errLineDefaultNotAllowed",
            "TTL Overrides line {line}: do not use default=...; use Default TTL above",
            { line: i + 1 }
          ),
          map: {},
        };
      }

      const ttlRaw = s.slice(sepIdx + 1).trim();
      const ttl = Number.parseInt(ttlRaw, 10);
      if (!Number.isFinite(ttl) || ttl < 1 || ttl > maxTTLSeconds) {
        return {
          ok: false,
          err: tFmt(
            "ttlOverrides.errLineRange",
            "TTL Overrides line {line}: TTL must be an integer between 1 and {max} (seconds)",
            { line: i + 1, max: maxTTLSeconds }
          ),
          map: {},
        };
      }

      out[ext] = ttl;
    }
    return { ok: true, map: out };
  };

  const prettyTTL = (sec) => {
    const s = Number(sec) || 0;
    if (s >= 86400 && s % 86400 === 0) return tFmt("ttl.unitDay", "{n} days", { n: s / 86400 });
    if (s >= 3600 && s % 3600 === 0) return tFmt("ttl.unitHour", "{n} hours", { n: s / 3600 });
    if (s >= 60 && s % 60 === 0) return tFmt("ttl.unitMinute", "{n} minutes", { n: s / 60 });
    return tFmt("ttl.unitSecond", "{n} seconds", { n: s });
  };

  const updateCdnjsPreview = () => {
    const pathEl = qs("#cdnjsPreviewPath");
    const outEl = qs("#cdnjsPreviewUrl");
    const hintEl = qs("#cdnjsPreviewHint");
    if (!(pathEl instanceof HTMLInputElement) || !(outEl instanceof HTMLInputElement) || !hintEl) return;

    let p = (pathEl.value || "").trim();
    if (!p) p = "repo@ref/path/file.js";
    p = p.replace(/^\/+/, "");

    const asset = (qs('input[name="assetUrl"]')?.value || "").trim().replace(/\/+$/, "") || "https://cdn.jsdelivr.net";
    const defUser = (qs('input[name="defaultGhUser"]')?.value || "").trim();

    if (!defUser) {
      outEl.value = asset + "/gh/<DEFAULT_GH_USER>/" + p;
      hintEl.textContent = tKey(
        "cdnjsPreview.defaultUserMissing",
        "DEFAULT_GH_USER is empty: short paths /<path> will return 400."
      );
      return;
    }

    const defaultTTLRaw = (qs('input[name="defaultTTLSeconds"]')?.value || "").trim();
    const defaultTTL = (() => {
      const n = Number.parseInt(defaultTTLRaw, 10);
      return Number.isFinite(n) && n > 0 ? n : cdnjsBuiltInDefaultTTL;
    })();

    const overridesRaw = (qs('textarea[name="ttlOverrides"]')?.value || "").toString();
    const overrides = parseTTLOverrides(overridesRaw);
    if (!overrides.ok) {
      hintEl.textContent = overrides.err || tKey("ttlOverrides.parseFailed", "TTL Overrides parse failed");
    }

    const ttlByExt = { ...cdnjsBuiltInTTLByExt, ...(overrides.map || {}) };
    const ext = extractExt(p);
    const ttl = ext && ttlByExt[ext] ? ttlByExt[ext] : defaultTTL;

    outEl.value = asset + "/gh/" + defUser + "/" + p;
    const ttlPrefix = overrides.ok
      ? tKey("cdnjsPreview.ttlMatchedPrefix", "TTL match: ")
      : tKey("cdnjsPreview.ttlMatchedPrefixWithError", "TTL match (but overrides has errors): ");
    hintEl.textContent =
      ttlPrefix +
      prettyTTL(ttl) +
      tKey("cdnjsPreview.cacheControlPrefix", " (Cache-Control: public, max-age=") +
      ttl +
      tKey("cdnjsPreview.cacheControlSuffix", ")");
  };

  const updateTorcherinoPreview = () => {
    const hostEl = qs("#torcherinoPreviewHost");
    const pathEl = qs("#torcherinoPreviewPath");
    const targetEl = qs("#torcherinoPreviewTarget");
    const urlEl = qs("#torcherinoPreviewUrl");
    const hintEl = qs("#torcherinoPreviewHint");
    if (
      !(hostEl instanceof HTMLInputElement) ||
      !(pathEl instanceof HTMLInputElement) ||
      !(targetEl instanceof HTMLInputElement) ||
      !(urlEl instanceof HTMLInputElement) ||
      !hintEl
    ) {
      return;
    }

    const normHost = (h) => (h || "").toString().trim().toLowerCase();
    const normPath = (p) => {
      let v = (p || "").toString().trim();
      if (!v) v = "/";
      if (!v.startsWith("/")) v = "/" + v;
      return v;
    };

    const host = normHost(hostEl.value || "");
    const path = normPath(pathEl.value || "");
    const defTarget = (qs('input[name="defaultTarget"]')?.value || "").trim();

    const raw = (qs("#hostMappingJson")?.value || "").trim();
    let mapping = {};
    if (raw) {
      try {
        const v = JSON.parse(raw);
        if (!v || typeof v !== "object" || Array.isArray(v)) {
          targetEl.value = "";
          urlEl.value = "";
          hintEl.textContent = tKey(
            "torcherinoPreview.mappingNotObject",
            "HOST_MAPPING must be a JSON object"
          );
          return;
        }
        mapping = v;
      } catch (e) {
        targetEl.value = "";
        urlEl.value = "";
        hintEl.textContent = tFmt(
          "torcherinoPreview.mappingJsonInvalid",
          "HOST_MAPPING JSON invalid: {msg}",
          { msg: e && e.message ? e.message : tKey("json.parseError", "parse error") }
        );
        return;
      }
    }

    const mappingLower = {};
    for (const [k, v] of Object.entries(mapping || {})) {
      const kk = normHost(k);
      const vv = (v || "").toString().trim();
      if (!kk || !vv) continue;
      mappingLower[kk] = vv;
    }

    const fromMap = host && mappingLower[host] ? String(mappingLower[host]) : "";
    const target = (fromMap || defTarget || "").trim();

    if (!target) {
      targetEl.value = "";
      urlEl.value = "";
      hintEl.textContent = tKey(
        "torcherinoPreview.noTarget",
        "DEFAULT_TARGET is empty and HOST_MAPPING didn't match: requests will return 502."
      );
      return;
    }

    targetEl.value =
      target +
      (fromMap
        ? tKey("torcherinoPreview.fromMapSuffix", " (mapped)")
        : tKey("torcherinoPreview.defaultSuffix", " (default)"));
    urlEl.value = "https://" + target + path;
    hintEl.textContent = tKey(
      "torcherinoPreview.rewriteNotice",
      "Note: Torcherino rewrites *.pages.dev / *.hf.space in upstream responses to the current host."
    );
  };

  const refreshPage = ({ skipNav = false, pathname = "" } = {}) => {
    if (!skipNav) updateNavActive(pathname);
    updateThemeToggle();
    applyTimeFormatting();
    updateSakuyaExampleUrls();
    updateGitPreview();
    updateCdnjsPreview();
    updateTorcherinoPreview();
    ensureDashboardStats();
    ensureTrafficPage();
    ensureSystemPage();
    openHashAccordion();
  };

  const openHashAccordion = () => {
    const hash = ((window.location && window.location.hash) || "").toString().trim();
    if (!hash || hash.length < 2) return;

    let target = null;
    try {
      target = document.querySelector(hash);
    } catch {
      return;
    }
    if (!(target instanceof HTMLDetailsElement)) return;
    if (!target.classList.contains("accordion")) return;
    target.open = true;
  };

  const updateNavActive = (pathname) => {
    const path = (pathname || (window.location && window.location.pathname) || "").toString();
    if (!path) return;

    for (const a of qsa(".nav a")) {
      const href = (a.getAttribute("href") || "").trim();
      if (!href) continue;
      const active = href === path || (href !== "/" && path.startsWith(href + "/"));
      a.classList.toggle("active", active);
    }
  };

  const setNavPending = (pathname) => {
    const path = (pathname || (window.location && window.location.pathname) || "").toString();
    if (!path) return;

    document.body.classList.add("hz-loading");
    const root = qs("#pjax-root");
    if (root) {
      root.setAttribute("aria-busy", "true");
      root.classList.remove("hz-leave", "hz-enter");
    }

    for (const a of qsa(".nav a")) {
      const href = (a.getAttribute("href") || "").trim();
      if (!href) continue;
      const active = href === path || (href !== "/" && path.startsWith(href + "/"));
      a.classList.toggle("hz-pending", active);
    }
  };

  const clearNavPending = () => {
    document.body.classList.remove("hz-loading");
    const root = qs("#pjax-root");
    if (root) root.removeAttribute("aria-busy");

    for (const a of qsa(".nav a.hz-pending")) {
      a.classList.remove("hz-pending");
    }
  };

  let navAbortController = null;
  let navSeq = 0;

  const isSameLayout = (doc) => {
    const curHasSidebar = !!qs(".sidebar");
    const nextHasSidebar = !!(doc && doc.querySelector && doc.querySelector(".sidebar"));
    return curHasSidebar === nextHasSidebar;
  };

  const parseHtml = (html) => {
    try {
      const parser = new DOMParser();
      return parser.parseFromString(html, "text/html");
    } catch {
      return null;
    }
  };

  const applyDocToDom = (doc) => {
    const root = qs("#pjax-root");
    const nextRoot = doc ? doc.querySelector("#pjax-root") : null;
    if (!root || !nextRoot) return false;

    root.innerHTML = nextRoot.innerHTML;

    const page = (doc.body && doc.body.getAttribute("data-page")) || "";
    if (page) {
      document.body.setAttribute("data-page", page);
    }

    const title = doc.title || "";
    if (title) {
      document.title = title;
      const headTitle = qs(".topbar .title");
      if (headTitle) {
        const stripped = title.replace(/\s*-\s*Hazuki\s*$/, "");
        headTitle.textContent = stripped || headTitle.textContent;
      }
    }

    const mobileNav = qs(".mobile-nav");
    if (mobileNav && "open" in mobileNav) {
      mobileNav.open = false;
    }

    // Avoid nav highlight flicker: during PJAX navigation, the URL may not be
    // updated yet (pushState happens later). Nav state is handled by navigate().
    refreshPage({ skipNav: true });
    return true;
  };

  const swapWithTransition = async (doc, isValid) => {
    const root = qs("#pjax-root");
    let ok = false;
    const apply = () => {
      if (typeof isValid === "function" && !isValid()) return;
      ok = applyDocToDom(doc);
    };

    if (!root) {
      apply();
      return ok;
    }

    if (typeof isValid === "function" && !isValid()) {
      return false;
    }

    if (typeof document.startViewTransition === "function") {
      try {
        const vt = document.startViewTransition(() => apply());
        if (vt && vt.finished) {
          await vt.finished;
        }
        return ok;
      } catch {
        // fall through
      }
    }

    root.classList.add("hz-leave");
    await waitTransitionEnd(root, 220);
    if (typeof isValid === "function" && !isValid()) {
      root.classList.remove("hz-leave");
      return false;
    }
    apply();
    root.classList.remove("hz-leave");
    root.classList.add("hz-enter");
    await nextFrame();
    root.classList.remove("hz-enter");

    return ok;
  };

  const shouldBypassPjax = (url, a) => {
    if (!url) return true;
    if (!canPjax()) return true;

    if (a) {
      if (a.hasAttribute("download")) return true;
      const target = (a.getAttribute("target") || "").toLowerCase();
      if (target && target !== "_self") return true;
      if ((a.getAttribute("rel") || "").toLowerCase().includes("external")) return true;
      if ((a.getAttribute("data-no-pjax") || "").trim() !== "") return true;
    }

    if (url.origin !== window.location.origin) return true;
    if (url.hash && url.pathname === window.location.pathname && url.search === window.location.search) return true;

    const p = url.pathname || "";
    if (p.startsWith("/assets/")) return true;
    if (p === "/favicon.ico" || p === "/fav.png") return true;
    if (p === "/lang") return true; // needs full reload to update layout translations
    if (p === "/config/export") return true; // download

    return false;
  };

  const navigate = async (url, { replace = false, addHistory = true } = {}) => {
    if (!url) return;
    if (!canPjax()) {
      window.location.href = url.href;
      return;
    }

    const mySeq = (navSeq += 1);

    if (navAbortController && typeof navAbortController.abort === "function") {
      try {
        navAbortController.abort();
      } catch {
        // ignore
      }
    }

    let controller = null;
    let signal;
    if (typeof AbortController === "function") {
      controller = new AbortController();
      navAbortController = controller;
      signal = controller.signal;
    } else {
      navAbortController = null;
    }

    updateNavActive(url.pathname);
    setNavPending(url.pathname);

    try {
      const fetchOpts = {
        method: "GET",
        headers: { Accept: "text/html", "X-Hazuki-Pjax": "1" },
        credentials: "same-origin",
      };
      if (signal) fetchOpts.signal = signal;

      const resp = await fetch(url.href, fetchOpts);
      if (mySeq !== navSeq) return;
      if (!resp.ok) {
        window.location.href = url.href;
        return;
      }

      const ct = (resp.headers.get("content-type") || "").toLowerCase();
      if (!ct.includes("text/html")) {
        window.location.href = url.href;
        return;
      }

      const html = await resp.text();
      if (mySeq !== navSeq) return;
      const doc = parseHtml(html);
      if (!doc || !doc.documentElement) {
        window.location.href = url.href;
        return;
      }

      if (!isSameLayout(doc)) {
        window.location.href = resp.url || url.href;
        return;
      }

      const finalURL = new URL(resp.url || url.href, window.location.href);

      const swapped = await swapWithTransition(doc, () => mySeq === navSeq);
      if (mySeq !== navSeq) return;
      if (!swapped) {
        window.location.href = finalURL.href;
        return;
      }

      if (addHistory) {
        const st = { href: finalURL.href };
        if (replace) window.history.replaceState(st, "", finalURL.href);
        else window.history.pushState(st, "", finalURL.href);
      }
      updateNavActive(finalURL.pathname);
      window.scrollTo(0, 0);
    } catch {
      if (controller && controller.signal && controller.signal.aborted) {
        return;
      }
      window.location.href = url.href;
    } finally {
      if (mySeq === navSeq) {
        clearNavPending();
      }
    }
  };

  const confirmModal = ({ title, detail, okText } = {}) => {
    const t = (title || tKey("modal.confirmTitle", "确认操作")).toString();
    const d = (detail || "").toString();

    const root = qs("#hz-modal");
    const titleEl = root ? qs("#hz-modal-title", root) : null;
    const detailEl = root ? qs("#hz-modal-detail", root) : null;
    const okBtn = root ? qs("[data-hz-modal-ok]", root) : null;
    const cancelEls = root ? qsa("[data-hz-modal-cancel]", root) : [];

    if (!root || !titleEl || !okBtn) {
      return Promise.resolve(window.confirm(d ? `${t}\n\n${d}` : t));
    }

    return new Promise((resolve) => {
      const prev = document.activeElement instanceof HTMLElement ? document.activeElement : null;
      let finished = false;

      const cleanup = () => {
        document.removeEventListener("keydown", onKeyDown);
        okBtn.removeEventListener("click", onOk);
        for (const el of cancelEls) el.removeEventListener("click", onCancel);
      };

      const finish = (ok) => {
        if (finished) return;
        finished = true;
        cleanup();

        document.body.classList.remove("hz-modal-open");
        root.hidden = true;
        root.classList.add("hz-enter"); // reset for next open

        if (prev && typeof prev.focus === "function") {
          try {
            prev.focus();
          } catch {
            // ignore
          }
        }

        resolve(ok);
      };

      const onKeyDown = (evt) => {
        if (evt.key !== "Escape") return;
        evt.preventDefault();
        finish(false);
      };

      const onCancel = (evt) => {
        evt.preventDefault();
        finish(false);
      };

      const onOk = (evt) => {
        evt.preventDefault();
        finish(true);
      };

      titleEl.textContent = t;
      if (detailEl) {
        detailEl.textContent = d;
        detailEl.style.display = d ? "" : "none";
      }
      okBtn.textContent = (okText || tKey("modal.ok", "确认")).toString();

      for (const el of cancelEls) {
        // Avoid putting visible text into non-button cancel elements (e.g. the backdrop).
        if (el instanceof HTMLButtonElement) el.textContent = tKey("modal.cancel", "取消");
        else el.textContent = "";
      }

      root.hidden = false;
      root.classList.add("hz-enter");
      document.body.classList.add("hz-modal-open");
      void root.offsetHeight;
      root.classList.remove("hz-enter");

      document.addEventListener("keydown", onKeyDown);
      okBtn.addEventListener("click", onOk);
      for (const el of cancelEls) el.addEventListener("click", onCancel);

      if (typeof okBtn.focus === "function") {
        try {
          okBtn.focus();
        } catch {
          // ignore
        }
      }
    });
  };

  const onConfirmSubmitClick = (e) => {
    const btn = e.target instanceof Element ? e.target.closest("[data-confirm-submit]") : null;
    if (!btn) return;

    const form = btn.closest("form");
    if (!(form instanceof HTMLFormElement)) return;

    const title = (btn.getAttribute("data-confirm-submit") || "").trim();
    if (!title) return;

    const detail = (btn.getAttribute("data-confirm-detail") || "").trim();
    const okText = (btn.getAttribute("data-confirm-ok") || "").trim();

    e.preventDefault();
    confirmModal({ title, detail, okText }).then((ok) => {
      if (!ok) return;
      try {
        form.submit();
      } catch {
        // ignore
      }
    });
  };

  const copyText = async (raw) => {
    const text = (raw || "").toString();
    if (!text) return false;

    if (navigator.clipboard && typeof navigator.clipboard.writeText === "function") {
      try {
        await navigator.clipboard.writeText(text);
        return true;
      } catch {
        // ignore
      }
    }

    try {
      const el = document.createElement("textarea");
      el.value = text;
      el.setAttribute("readonly", "true");
      el.style.position = "fixed";
      el.style.left = "-9999px";
      el.style.top = "0";
      el.style.opacity = "0";
      document.body.appendChild(el);

      el.focus();
      el.select();
      el.setSelectionRange(0, el.value.length);

      const ok = document.execCommand && document.execCommand("copy");
      document.body.removeChild(el);
      return !!ok;
    } catch {
      return false;
    }
  };

  const copyTimers = new WeakMap();
  const flashCopied = (el) => {
    if (!(el instanceof Element)) return;

    const prev = copyTimers.get(el);
    if (prev) clearTimeout(prev);

    el.classList.add("hz-copied");
    const t = setTimeout(() => {
      el.classList.remove("hz-copied");
      copyTimers.delete(el);
    }, 900);
    copyTimers.set(el, t);
  };

  const trimRight = (s, ch) => {
    let out = (s || "").toString();
    while (out.endsWith(ch)) out = out.slice(0, -ch.length);
    return out;
  };

  const sanitizeUrlBase = (raw) => {
    let out = (raw || "").toString().trim();
    out = trimRight(out, "/");
    return out;
  };

  const splitUrlSuffix = (raw) => {
    const s = (raw || "").toString();
    const q = s.indexOf("?");
    const h = s.indexOf("#");
    let idx = -1;
    if (q >= 0 && h >= 0) idx = Math.min(q, h);
    else if (q >= 0) idx = q;
    else if (h >= 0) idx = h;
    if (idx < 0) return [s, ""];
    return [s.slice(0, idx), s.slice(idx)];
  };

  const stripLastUrlPathSegment = (raw) => {
    const s = (raw || "").toString().trim();
    if (!s) return "";
    try {
      const u = new URL(s, window.location.href);
      const parts = (u.pathname || "").split("/").filter((p) => p && p.trim() !== "");
      if (parts.length <= 1) {
        u.pathname = "";
      } else {
        parts.pop();
        u.pathname = "/" + parts.join("/");
      }
      u.search = "";
      u.hash = "";
      return u.toString();
    } catch {
      const idx = s.lastIndexOf("/");
      if (idx <= 0) return s;
      return s.slice(0, idx);
    }
  };

  const sanitizePathSegment = (raw) => {
    let out = (raw || "").toString().trim();
    while (out.startsWith("/") || out.startsWith("\\")) out = out.slice(1);
    while (out.endsWith("/") || out.endsWith("\\")) out = out.slice(0, -1);
    return out;
  };

  const buildSakuyaExampleUrl = (el) => {
    if (!(el instanceof Element)) return "";
    const form = el.closest("form") || document;

    const pubEl = qs('input[name="oplistPublicUrl"]', form);
    const prefixEl = qs('input[name="oplistPrefix"]', form);
    const baseEl = qs("[data-sakuya-base-url]");

    let rawBase = ((pubEl && pubEl.value) || "").toString().trim();
    const isUsingFallbackBase = !rawBase;
    if (!rawBase) rawBase = ((baseEl && baseEl.textContent) || "").toString().trim();
    if (!rawBase) return "";

    let urlSuffix = "";
    [rawBase, urlSuffix] = splitUrlSuffix(rawBase);

    if (isUsingFallbackBase && prefixEl instanceof HTMLInputElement) {
      rawBase = stripLastUrlPathSegment(rawBase);
    }
    let base = sanitizeUrlBase(rawBase);

    let prefix = sanitizePathSegment(prefixEl && prefixEl.value);

    if (prefix) {
      const prefixPath = "/" + prefix;
      if (!base.toLowerCase().endsWith(prefixPath.toLowerCase())) {
        base += prefixPath;
      }
    }

    base = sanitizeUrlBase(base);
    return base + urlSuffix;
  };

  const onSakuyaCopyExampleClick = (e) => {
    const btn = e.target instanceof Element ? e.target.closest("[data-sakuya-copy-example]") : null;
    if (!btn) return;

    e.preventDefault();

    const url = buildSakuyaExampleUrl(btn);
    if (!url) return;

    copyText(url).then((ok) => {
      if (!ok) return;
      flashCopied(btn);
    });
  };

  const updateSakuyaExampleUrls = () => {
    for (const el of qsa("[data-sakuya-example-url]")) {
      const url = buildSakuyaExampleUrl(el);
      const out = (url || "").toString().trim();
      el.textContent = out || "-";
      if (out) el.setAttribute("title", out);
      else el.removeAttribute("title");
    }
  };

  const onCopyClick = (e) => {
    const el = e.target instanceof Element ? e.target.closest("[data-hz-copy]") : null;
    if (!el) return;

    const attrText = (el.getAttribute("data-hz-copy-text") || "").trim();
    const text = (attrText || el.textContent || "").trim();
    if (!text) return;

    copyText(text).then((ok) => {
      if (!ok) return;
      flashCopied(el);
    });
  };

  const onLinkClick = (e) => {
    if (e.defaultPrevented) return;
    if (e.button !== 0) return;
    if (e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return;

    const a = e.target instanceof Element ? e.target.closest("a[href]") : null;
    if (!a) return;

    const href = (a.getAttribute("href") || "").trim();
    if (!href || href.startsWith("#")) return;

    const url = new URL(a.href, window.location.href);

    const confirmTitle = (a.getAttribute("data-confirm-download") || "").trim();
    if (confirmTitle) {
      const detail = (a.getAttribute("data-confirm-detail") || "").trim();
      e.preventDefault();
      confirmModal({ title: confirmTitle, detail, okText: tKey("modal.download", "下载") }).then((ok) => {
        if (!ok) return;
        window.location.href = url.href;
      });
      return;
    }

    if (shouldBypassPjax(url, a)) return;

    e.preventDefault();
    navigate(url);
  };

  const onPopState = () => {
    navigate(new URL(window.location.href), { replace: true, addHistory: false });
  };

  const onPreviewInput = (e) => {
    const t = e.target;
    if (!(t instanceof Element)) return;

    const name = (t.getAttribute("name") || "").trim();
    const id = (t.getAttribute("id") || "").trim();

    if (name === "oplistPublicUrl" || name === "oplistPrefix") {
      updateSakuyaExampleUrls();
      return;
    }

    if (id === "gitPreviewPath" || name === "upstream" || name === "upstreamPath" || name === "upstreamHttps") {
      updateGitPreview();
      return;
    }

    if (
      id === "cdnjsPreviewPath" ||
      name === "assetUrl" ||
      name === "defaultGhUser" ||
      name === "defaultTTLSeconds" ||
      name === "ttlOverrides"
    ) {
      updateCdnjsPreview();
      return;
    }

    if (
      id === "torcherinoPreviewHost" ||
      id === "torcherinoPreviewPath" ||
      id === "hostMappingJson" ||
      name === "defaultTarget"
    ) {
      updateTorcherinoPreview();
      return;
    }
  };

  document.addEventListener("click", onFormatJsonClick);
  document.addEventListener("change", onTogglePassword);
  document.addEventListener("click", onConfirmSubmitClick);
  document.addEventListener("click", onThemeToggleClick);
  document.addEventListener("click", onSakuyaCopyExampleClick);
  document.addEventListener("click", onCopyClick);
  document.addEventListener("click", onLinkClick);
  document.addEventListener("input", onPreviewInput);
  document.addEventListener("change", onPreviewInput);
  window.addEventListener("popstate", onPopState);
  window.addEventListener("hashchange", openHashAccordion);
  window.addEventListener("storage", (e) => {
    if (!e) return;
    if (e.key !== THEME_KEY) return;
    applyTheme(getTheme());
    updateThemeToggle();
  });

  applyTheme(getTheme());

  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", () => refreshPage(), { once: true });
  } else {
    refreshPage();
  }

  window.HazukiUI = { qs, qsa, formatJson, navigate };
})();
