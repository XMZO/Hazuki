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

  const SVG_NS = "http://www.w3.org/2000/svg";

  const reducedMotion = () =>
    !!(window.matchMedia && window.matchMedia("(prefers-reduced-motion: reduce)").matches);

  const currentPage = () => (document.body && document.body.getAttribute("data-page")) || "";

  const iconEl = (name, cls = "") => {
    const svg = document.createElementNS(SVG_NS, "svg");
    svg.setAttribute("class", ("icon " + cls).trim());
    svg.setAttribute("aria-hidden", "true");
    const use = document.createElementNS(SVG_NS, "use");
    use.setAttribute("href", "#i-" + name);
    svg.appendChild(use);
    return svg;
  };

  const restartAnimation = (el, cls) => {
    if (!el || reducedMotion()) return;
    el.classList.remove(cls);
    void el.offsetWidth;
    el.classList.add(cls);
    el.addEventListener("animationend", () => el.classList.remove(cls), { once: true });
  };

  // ---------------------------------------------------------------------------
  // Theme
  // ---------------------------------------------------------------------------
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

  const syncThemeColorMeta = (theme) => {
    for (const meta of qsa("meta[data-hz-theme-color]")) {
      const own = meta.getAttribute("data-hz-theme-color");
      if (!meta.dataset.hzDefault) meta.dataset.hzDefault = meta.getAttribute("content") || "";
      if (theme === "auto") {
        meta.setAttribute("content", meta.dataset.hzDefault);
        continue;
      }
      const other = qs(`meta[data-hz-theme-color="${theme}"]`);
      const color = (other && (other.dataset.hzDefault || other.getAttribute("content"))) || "";
      if (color && own) meta.setAttribute("content", color);
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
    syncThemeColorMeta(t);
  };

  const themeLabel = (theme) => {
    const t = normalizeTheme(theme);
    if (t === "dark") return tKey("theme.dark", "Dark");
    if (t === "light") return tKey("theme.light", "Light");
    return tKey("theme.auto", "Auto");
  };

  const updateThemeToggle = () => {
    const label = themeLabel(getTheme());
    const title = tFmt("theme.toggleHint", "Theme: {mode} (click to toggle)", { mode: label });
    for (const btn of qsa("[data-theme-toggle]")) {
      btn.title = title;
      btn.setAttribute("aria-label", title);
    }
  };

  const setTheme = (theme) => {
    const t = normalizeTheme(theme);
    try {
      if (t === "auto") localStorage.removeItem(THEME_KEY);
      else localStorage.setItem(THEME_KEY, t);
    } catch {
      // ignore
    }

    const html = document.documentElement;
    if (typeof document.startViewTransition === "function" && !reducedMotion()) {
      html.classList.add("hz-vt-theme");
      try {
        const vt = document.startViewTransition(() => applyTheme(t));
        vt.finished.finally(() => html.classList.remove("hz-vt-theme"));
      } catch {
        html.classList.remove("hz-vt-theme");
        applyTheme(t);
      }
    } else {
      applyTheme(t);
    }
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

  // ---------------------------------------------------------------------------
  // Formatting helpers
  // ---------------------------------------------------------------------------
  const canPjax = () =>
    typeof window.fetch === "function" &&
    typeof window.DOMParser === "function" &&
    !!(window.history && window.history.pushState);

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

  const formatCount = (v) => {
    const n = toInt(v);
    try {
      return n.toLocaleString();
    } catch {
      return String(n);
    }
  };

  const abortQuietly = (ctrl) => {
    if (ctrl && typeof ctrl.abort === "function") {
      try {
        ctrl.abort();
      } catch {
        // ignore
      }
    }
  };

  // ---------------------------------------------------------------------------
  // Dashboard live stats
  // ---------------------------------------------------------------------------
  const SPARK_POINTS = 40;

  let dashTimer = null;
  let dashPrev = null;
  let dashPrevAt = 0;
  let dashAbort = null;
  let dashSeq = 0;
  let sparkOut = [];
  let sparkIn = [];

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
    dashPrev = null;
    dashPrevAt = 0;
    sparkOut = [];
    sparkIn = [];
    abortQuietly(dashAbort);
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

    return { torcherino, cdnjs, git, sakuya, patchouli, total };
  };

  // Keys are either canonical aggregates (torcherino, git, total, ...) or
  // "raw:<metrics key>" for a single entry of the stats map (e.g. raw:git:mirror).
  const lookupSnap = (state, key) => {
    if (!state) return null;
    if (key.startsWith("raw:")) {
      const raw = state.raw && state.raw[key.slice(4)];
      return raw ? snapFrom(raw) : { bytesIn: 0, bytesOut: 0, requests: 0 };
    }
    return state.agg[key] || null;
  };

  const updateTrafficDom = (state, prev, dt) => {
    for (const box of qsa("[data-hz-svc]")) {
      const key = (box.getAttribute("data-hz-svc") || "").trim();
      if (!key) continue;
      const cur = lookupSnap(state, key) || { bytesIn: 0, bytesOut: 0, requests: 0 };
      const pre = lookupSnap(prev, key);

      let downRate = "-";
      let upRate = "-";
      if (pre && dt > 0) {
        downRate = formatBps(Math.max(0, cur.bytesOut - pre.bytesOut) / dt);
        upRate = formatBps(Math.max(0, cur.bytesIn - pre.bytesIn) / dt);
      }

      for (const el of qsa("[data-hz-field]", box)) {
        const f = (el.getAttribute("data-hz-field") || "").trim();
        let text = null;
        if (f === "downTotal") text = formatBytes(cur.bytesOut);
        else if (f === "upTotal") text = formatBytes(cur.bytesIn);
        else if (f === "downRate") text = downRate;
        else if (f === "upRate") text = upRate;
        if (text !== null && el.textContent !== text) el.textContent = text;
      }
    }
  };

  const renderSpark = (svg, values) => {
    const line = qs(".spark-line", svg);
    const area = qs(".spark-area", svg);
    if (!line || !area) return;
    if (values.length < 2) {
      // Baseline placeholder until two samples exist.
      line.setAttribute("d", "M0 24L100 24");
      area.setAttribute("d", "");
      return;
    }
    const max = Math.max(1, ...values);
    // Right-aligned window that starts at 12 samples and widens up to SPARK_POINTS.
    const windowSize = Math.max(12, values.length);
    const span = windowSize - 1;
    const offset = windowSize - values.length;
    let d = "";
    for (let i = 0; i < values.length; i += 1) {
      const x = ((offset + i) / span) * 100;
      const y = 24 - (Math.max(0, values[i]) / max) * 22;
      d += (i === 0 ? "M" : "L") + x.toFixed(2) + " " + y.toFixed(2);
    }
    const x0 = ((offset / span) * 100).toFixed(2);
    line.setAttribute("d", d);
    area.setAttribute("d", d + "L100 26L" + x0 + " 26Z");
  };

  const updateSparks = () => {
    for (const svg of qsa("[data-hz-spark]")) {
      const kind = svg.getAttribute("data-hz-spark");
      renderSpark(svg, kind === "in" ? sparkIn : sparkOut);
    }
  };

  const updateRedisDom = (redis) => {
    const pill = qs("#hz-redis-pill");
    if (pill) {
      pill.classList.remove("ok", "err", "off");
      const status = (redis && redis.status ? String(redis.status) : "").toLowerCase();

      if (status === "ok") {
        pill.classList.add("ok");
        const latency = toInt(redis.latencyMS);
        pill.textContent = tKey("redis.ok", "Redis OK") + " · " + latency + "ms";
        pill.title = (redis.addr || "").toString();
      } else if (status === "error") {
        pill.classList.add("err");
        pill.textContent = tKey("redis.error", "Redis error");
        pill.title = (redis.addr || "").toString();
      } else {
        pill.classList.add("off");
        pill.textContent = tKey("redis.notConfigured", "Redis not configured");
      }
    }

    const hits = toInt(redis && redis.keyspaceHits);
    const misses = toInt(redis && redis.keyspaceMisses);
    const total = hits + misses;
    const hitRate = total > 0 ? ((hits / total) * 100).toFixed(1) + "%" : "-";

    for (const el of qsa("[data-hz-redis-field]")) {
      const f = (el.getAttribute("data-hz-redis-field") || "").trim();
      let text = null;
      if (f === "dbSize") text = redis ? formatCount(redis.dbSize) : "-";
      else if (f === "usedMemoryHuman") text = redis && redis.usedMemoryHuman ? String(redis.usedMemoryHuman) : "-";
      else if (f === "keyspaceHits") text = formatCount(hits);
      else if (f === "keyspaceMisses") text = formatCount(misses);
      else if (f === "hitRate") text = hitRate;
      if (text !== null && el.textContent !== text) el.textContent = text;
    }
  };

  const pollDashboardStats = async () => {
    if (currentPage() !== "dashboard") {
      stopDashboardStats();
      return;
    }
    if (document.hidden) return;

    const mySeq = (dashSeq += 1);
    abortQuietly(dashAbort);
    dashAbort = typeof AbortController === "function" ? new AbortController() : null;

    try {
      const opts = { method: "GET", headers: { Accept: "application/json" }, credentials: "same-origin" };
      if (dashAbort) opts.signal = dashAbort.signal;

      const resp = await fetch("/_hazuki/stats", opts);
      if (mySeq !== dashSeq) return;

      const ct = (resp.headers.get("content-type") || "").toLowerCase();
      if (!resp.ok || !ct.includes("application/json")) return;

      const payload = await resp.json();
      if (mySeq !== dashSeq) return;

      const nowAt = typeof performance !== "undefined" && performance.now ? performance.now() : Date.now();
      const dt = dashPrevAt > 0 ? Math.max(0.001, (nowAt - dashPrevAt) / 1000) : 0;

      const raw = payload && payload.services && typeof payload.services === "object" ? payload.services : {};
      const state = { agg: aggregateServices(raw), raw };

      updateTrafficDom(state, dashPrev, dt);
      updateRedisDom(payload && payload.redis ? payload.redis : null);

      if (dashPrev && dt > 0) {
        const total = state.agg.total;
        const prevTotal = dashPrev.agg.total;
        sparkOut.push(Math.max(0, total.bytesOut - prevTotal.bytesOut) / dt);
        sparkIn.push(Math.max(0, total.bytesIn - prevTotal.bytesIn) / dt);
        if (sparkOut.length > SPARK_POINTS) sparkOut = sparkOut.slice(-SPARK_POINTS);
        if (sparkIn.length > SPARK_POINTS) sparkIn = sparkIn.slice(-SPARK_POINTS);
        updateSparks();
      }

      dashPrev = state;
      dashPrevAt = nowAt;
    } catch {
      // network hiccup or abort: keep the previous frame
    }
  };

  const ensureDashboardStats = () => {
    if (currentPage() !== "dashboard") {
      stopDashboardStats();
      return;
    }
    if (dashTimer) return;
    dashPrev = null;
    dashPrevAt = 0;
    pollDashboardStats();
    dashTimer = setInterval(pollDashboardStats, 2000);
  };

  const stopTrafficPage = () => {
    if (trafficTimer) {
      clearInterval(trafficTimer);
      trafficTimer = null;
    }
    abortQuietly(trafficAbort);
    trafficAbort = null;
    trafficLast = null;
    if (trafficResizeObserver) {
      trafficResizeObserver.disconnect();
      trafficResizeObserver = null;
    }
  };

  const stopSystemPage = () => {
    if (systemTimer) {
      clearInterval(systemTimer);
      systemTimer = null;
    }
    abortQuietly(systemAbort);
    systemAbort = null;
  };

  // ---------------------------------------------------------------------------
  // System: rewrite runtime
  // ---------------------------------------------------------------------------
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
      const show = key === activeTab;
      if (show && panel.hidden) {
        panel.hidden = false;
        restartAnimation(panel, "panel-enter");
      } else if (!show) {
        panel.hidden = true;
      }
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
      const text = typeof value === "string" && value.trim() !== "" ? value : "-";
      if (el.textContent !== text) {
        el.textContent = text;
        el.title = text;
      }
    }

    for (const pill of qsa("[data-hz-rewrite-enabled-pill]", card)) {
      const key = (pill.getAttribute("data-hz-rewrite-enabled-pill") || "").trim();
      if (!key) continue;
      const enabled = !!getObjectPath(info, key + ".enabled");
      pill.classList.toggle("ok", enabled);
      pill.classList.toggle("off", !enabled);
      pill.textContent = enabled
        ? pill.getAttribute("data-label-on") || tKey("common.enabled", "Enabled")
        : pill.getAttribute("data-label-off") || tKey("common.disabled", "Disabled");
    }
  };

  const pollSystemRewriteRuntime = async () => {
    if (currentPage() !== "system") {
      stopSystemPage();
      return;
    }
    if (document.hidden) return;

    const mySeq = (systemSeq += 1);
    abortQuietly(systemAbort);
    systemAbort = typeof AbortController === "function" ? new AbortController() : null;

    try {
      const opts = { method: "GET", headers: { Accept: "application/json" }, credentials: "same-origin" };
      if (systemAbort) opts.signal = systemAbort.signal;

      const resp = await fetch("/_hazuki/system/rewrite-runtime", opts);
      if (mySeq !== systemSeq) return;

      const ct = (resp.headers.get("content-type") || "").toLowerCase();
      if (!resp.ok || !ct.includes("application/json")) return;

      const payload = await resp.json();
      if (mySeq !== systemSeq) return;

      updateSystemRewriteRuntimeDom(payload && payload.rewriteRuntime ? payload.rewriteRuntime : null);
    } catch {
      // keep previous values
    }
  };

  const ensureSystemPage = () => {
    if (currentPage() !== "system") {
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

  // ---------------------------------------------------------------------------
  // Time formatting
  // ---------------------------------------------------------------------------
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

    const label = offsetMinutes === 0 ? "UTC" : `${sign === 1 ? "+" : "-"}${pad2(hh)}:${pad2(mm)}`;
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

  // Compact axis label: drop the parts every tick shares.
  const formatAxisTime = (kind, ts) => {
    const n = Number(ts);
    if (!Number.isFinite(n) || n <= 0) return "";
    const parts = datePartsFromMs(n * 1000);
    if (!parts) return "";
    if (kind === "year") return String(parts.y);
    if (kind === "month") return `${parts.y}-${parts.m}`;
    if (kind === "day") return `${parts.m}-${parts.day}`;
    return `${parts.m}-${parts.day} ${parts.h}:00`;
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
      if (el.tagName === "TIME") el.setAttribute("datetime", iso);
    }
  };

  // ---------------------------------------------------------------------------
  // Traffic chart (rendered at device pixels so text and strokes stay crisp)
  // ---------------------------------------------------------------------------
  let trafficLast = null;
  let trafficResizeObserver = null;
  let trafficGeom = null;

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

  // Nice 1/2/2.5/5 x 10^k steps in the byte unit that fits the max (1024-based).
  const niceByteTicks = (rawMax, count) => {
    const max = Math.max(1, rawMax);
    let unit = 1;
    while (max / unit >= 1024 && unit < 1024 ** 4) unit *= 1024;
    const scaled = max / unit;
    const rough = scaled / count;
    const pow = 10 ** Math.floor(Math.log10(rough));
    const steps = [1, 2, 2.5, 5, 10];
    let step = steps[steps.length - 1] * pow;
    for (const s of steps) {
      if (s * pow >= rough) {
        step = s * pow;
        break;
      }
    }
    const top = Math.ceil(scaled / step) * step;
    const ticks = [];
    for (let v = 0; v <= top + step / 2; v += step) ticks.push(v * unit);
    return { max: top * unit, ticks };
  };

  const svgEl = (name, attrs) => {
    const el = document.createElementNS(SVG_NS, name);
    for (const [k, v] of Object.entries(attrs || {})) el.setAttribute(k, String(v));
    return el;
  };

  const drawTrafficChart = () => {
    const box = qs("#hzTrafficChart");
    const svg = qs("#hzTrafficSvg");
    if (!box || !svg || !trafficLast) return;

    const W = Math.max(240, Math.round(box.clientWidth));
    const H = Math.max(160, Math.round(box.clientHeight));
    const pts = Array.isArray(trafficLast.points) ? trafficLast.points : [];
    const kind = (trafficLast.kind || "").toString();

    const outs = pts.map((p) => Math.max(0, toInt(p && p.bytesOut)));
    const ins = pts.map((p) => Math.max(0, toInt(p && p.bytesIn)));
    // Floor the scale at 1 KB so an idle series doesn't produce sub-byte ticks.
    const { max, ticks } = niceByteTicks(Math.max(1024, ...outs, ...ins), 4);

    const padL = 62;
    const padR = 10;
    const padT = 10;
    const padB = 24;
    const iw = Math.max(1, W - padL - padR);
    const ih = Math.max(1, H - padT - padB);
    const n = pts.length;
    const xAt = (i) => (n <= 1 ? padL + iw / 2 : padL + (i * iw) / (n - 1));
    const yAt = (v) => padT + ih - (v / max) * ih;

    svg.setAttribute("viewBox", `0 0 ${W} ${H}`);
    svg.replaceChildren();

    for (const t of ticks) {
      const y = Math.round(yAt(t)) + 0.5;
      svg.appendChild(svgEl("line", { class: t === 0 ? "chart-base" : "chart-grid", x1: padL, x2: W - padR, y1: y, y2: y }));
      const label = svgEl("text", { class: "chart-tick", x: padL - 8, y: y + 4, "text-anchor": "end" });
      label.textContent = t === 0 ? "0" : formatBytes(t);
      svg.appendChild(label);
    }

    if (!n) {
      const empty = svgEl("text", { class: "chart-empty", x: padL + iw / 2, y: padT + ih / 2, "text-anchor": "middle" });
      empty.textContent = tKey("traffic.empty", "No data yet");
      svg.appendChild(empty);
      trafficGeom = null;
      return;
    }

    const labelIdx = n === 1 ? [0] : n === 2 ? [0, 1] : [0, Math.floor((n - 1) / 2), n - 1];
    for (const i of labelIdx) {
      const anchor = n === 1 ? "middle" : i === 0 ? "start" : i === n - 1 ? "end" : "middle";
      const label = svgEl("text", { class: "chart-tick", x: xAt(i), y: H - 6, "text-anchor": anchor });
      label.textContent = formatAxisTime(kind, pts[i] && pts[i].startTs);
      svg.appendChild(label);
    }

    const linePath = (vals) => {
      if (vals.length === 1) {
        const y = yAt(vals[0]).toFixed(1);
        return `M${padL} ${y}L${padL + iw} ${y}`;
      }
      let d = "";
      for (let i = 0; i < vals.length; i += 1) {
        d += (i === 0 ? "M" : "L") + xAt(i).toFixed(1) + " " + yAt(vals[i]).toFixed(1);
      }
      return d;
    };
    const areaPath = (vals) => {
      const line = linePath(vals);
      const x0 = vals.length === 1 ? padL : xAt(0);
      const x1 = vals.length === 1 ? padL + iw : xAt(vals.length - 1);
      return `${line}L${x1.toFixed(1)} ${padT + ih}L${x0.toFixed(1)} ${padT + ih}Z`;
    };

    svg.appendChild(svgEl("path", { class: "chart-area s2", d: areaPath(ins) }));
    svg.appendChild(svgEl("path", { class: "chart-area s1", d: areaPath(outs) }));
    svg.appendChild(svgEl("path", { class: "chart-line s2", d: linePath(ins) }));
    svg.appendChild(svgEl("path", { class: "chart-line s1", d: linePath(outs) }));

    const hover = svgEl("g", { class: "chart-hover" });
    const cross = svgEl("line", { class: "chart-cross", x1: 0, x2: 0, y1: padT, y2: padT + ih });
    const dotOut = svgEl("circle", { class: "chart-dot s1", r: 4, cx: 0, cy: 0 });
    const dotIn = svgEl("circle", { class: "chart-dot s2", r: 4, cx: 0, cy: 0 });
    hover.append(cross, dotIn, dotOut);
    svg.appendChild(hover);

    trafficGeom = { W, padL, iw, n, xAt, yAt, outs, ins, pts, kind, cross, dotOut, dotIn };
  };

  const showTrafficTip = (clientX) => {
    const box = qs("#hzTrafficChart");
    const tip = qs("#hzTrafficTip");
    const g = trafficGeom;
    if (!box || !tip || !g || !g.n) return;

    const rect = box.getBoundingClientRect();
    const x = ((clientX - rect.left) / Math.max(1, rect.width)) * g.W;
    let i = g.n <= 1 ? 0 : Math.round(((x - g.padL) / g.iw) * (g.n - 1));
    i = Math.max(0, Math.min(g.n - 1, i));

    const cx = g.n <= 1 ? g.padL + g.iw / 2 : g.xAt(i);
    const px = Math.round(cx) + 0.5;
    g.cross.setAttribute("x1", px);
    g.cross.setAttribute("x2", px);
    g.dotOut.setAttribute("cx", cx);
    g.dotOut.setAttribute("cy", g.yAt(g.outs[i]));
    g.dotIn.setAttribute("cx", cx);
    g.dotIn.setAttribute("cy", g.yAt(g.ins[i]));

    const p = g.pts[i] || {};
    const rows = [
      ["s1", tKey("traffic.out", "Out"), formatBytes(g.outs[i])],
      ["s2", tKey("traffic.in", "In"), formatBytes(g.ins[i])],
      ["", tKey("traffic.req", "Requests"), formatCount(p.requests)],
    ];
    const frag = document.createDocumentFragment();
    const time = document.createElement("div");
    time.className = "tip-time";
    time.textContent = formatBucketTime(g.kind, p.startTs);
    frag.appendChild(time);
    for (const [cls, label, value] of rows) {
      const row = document.createElement("div");
      row.className = "tip-row";
      const key = document.createElement("i");
      key.className = "key-line " + cls;
      if (!cls) key.style.visibility = "hidden";
      const name = document.createElement("span");
      name.textContent = label;
      const val = document.createElement("b");
      val.textContent = value;
      row.append(key, name, val);
      frag.appendChild(row);
    }
    tip.replaceChildren(frag);

    const cssX = (cx / g.W) * rect.width;
    const tipW = tip.offsetWidth || 180;
    let left = cssX + 14;
    if (left + tipW > rect.width) left = cssX - tipW - 14;
    tip.style.setProperty("--tx", Math.max(0, left).toFixed(0) + "px");
    box.classList.add("is-hover");
  };

  const hideTrafficTip = () => {
    const box = qs("#hzTrafficChart");
    if (box) box.classList.remove("is-hover");
  };

  const renderTrafficSeries = (payload) => {
    trafficLast = payload || { points: [] };
    const kind = (trafficLast.kind || "").toString();
    const pts = Array.isArray(trafficLast.points) ? trafficLast.points : [];

    drawTrafficChart();

    let sumOut = 0;
    let sumIn = 0;
    for (const p of pts) {
      sumOut += Math.max(0, toInt(p && p.bytesOut));
      sumIn += Math.max(0, toInt(p && p.bytesIn));
    }
    const outLastEl = qs("#hzTrafficOutLast");
    const inLastEl = qs("#hzTrafficInLast");
    if (outLastEl) outLastEl.textContent = formatBytes(sumOut);
    if (inLastEl) inLastEl.textContent = formatBytes(sumIn);

    const rangeEl = qs("#hzTrafficRange");
    if (rangeEl) {
      const fromTs = trafficLast.fromTs || 0;
      const toTs = trafficLast.toTs || 0;
      rangeEl.textContent = formatBucketTime(kind, fromTs) + " → " + formatBucketTime(kind, toTs);
    }

    const tbody = qs("#hzTrafficTableBody");
    if (tbody) {
      const frag = document.createDocumentFragment();
      for (let i = pts.length - 1; i >= 0; i -= 1) {
        const p = pts[i] || {};
        const tr = document.createElement("tr");
        const cells = [
          [formatBucketTime(kind, p.startTs), ""],
          [formatBytes(toInt(p.bytesOut)), "num"],
          [formatBytes(toInt(p.bytesIn)), "num"],
          [formatCount(p.requests), "num"],
        ];
        for (const [text, cls] of cells) {
          const td = document.createElement("td");
          if (cls) td.className = cls;
          td.textContent = text;
          tr.appendChild(td);
        }
        frag.appendChild(tr);
      }
      if (!pts.length) {
        const tr = document.createElement("tr");
        const td = document.createElement("td");
        td.colSpan = 4;
        td.className = "empty";
        td.textContent = tKey("traffic.empty", "No data yet");
        tr.appendChild(td);
        frag.appendChild(tr);
      }
      tbody.replaceChildren(frag);
    }
  };

  const fetchTrafficSeries = async () => {
    if (currentPage() !== "traffic") return;
    if (document.hidden && trafficLast) return;

    const kind = getTrafficKind();
    const svc = getTrafficService();
    const mySeq = (trafficSeq += 1);

    abortQuietly(trafficAbort);
    trafficAbort = typeof AbortController === "function" ? new AbortController() : null;

    const url = new URL("/_hazuki/traffic/series", window.location.href);
    url.searchParams.set("kind", kind);
    url.searchParams.set("svc", svc);

    const box = qs("#hzTrafficChart");
    if (box) box.classList.add("is-loading");
    try {
      const resp = await fetch(url.toString(), {
        method: "GET",
        headers: { accept: "application/json" },
        credentials: "same-origin",
        signal: trafficAbort ? trafficAbort.signal : undefined,
      });
      if (mySeq !== trafficSeq) return;
      const ct = (resp.headers.get("content-type") || "").toLowerCase();
      if (!resp.ok || !ct.includes("application/json")) return;

      const payload = await resp.json();
      if (mySeq !== trafficSeq) return;

      renderTrafficSeries(payload);
    } catch {
      // keep the previous frame
    } finally {
      if (mySeq === trafficSeq && box) box.classList.remove("is-loading");
    }
  };

  const ensureTrafficPage = () => {
    if (currentPage() !== "traffic") {
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

    const box = qs("#hzTrafficChart");
    if (box && !box.__hzTrafficBound) {
      box.__hzTrafficBound = true;
      box.addEventListener("pointermove", (e) => showTrafficTip(e.clientX));
      box.addEventListener("pointerdown", (e) => showTrafficTip(e.clientX));
      box.addEventListener("pointerleave", hideTrafficTip);
      if (typeof ResizeObserver === "function") {
        let raf = 0;
        trafficResizeObserver = new ResizeObserver(() => {
          cancelAnimationFrame(raf);
          raf = requestAnimationFrame(() => {
            hideTrafficTip();
            drawTrafficChart();
          });
        });
        trafficResizeObserver.observe(box);
      }
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
      // Formatting is a user action: keep the value and refresh the unsaved state.
      userTyped.add(ta);
      if (ta.form && ta.form.hasAttribute("data-hz-dirty")) syncDirty(ta.form);
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

    if (input.classList.contains("secret") && input.type === "text") {
      input.classList.toggle("is-revealed", el.checked);
      return;
    }
    input.type = el.checked ? "text" : "password";
  };

  // Masked secrets rely on -webkit-text-security; where it's missing, fall back
  // to a real password field so the value is never shown in clear text.
  const SECRET_MASK_SUPPORTED = !!(
    window.CSS &&
    typeof window.CSS.supports === "function" &&
    window.CSS.supports("-webkit-text-security", "disc")
  );
  const applySecretFallback = () => {
    if (SECRET_MASK_SUPPORTED) return;
    for (const input of qsa("input.secret")) {
      input.type = "password";
      input.setAttribute("autocomplete", "new-password");
    }
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

  // ---------------------------------------------------------------------------
  // Tabs: one strip per page ([data-hz-tabs]) switching [data-hz-panel] sections.
  // Route tabs ([data-hz-route-tabs]) are plain links; only the ink is managed.
  // ---------------------------------------------------------------------------
  const TAB_STORE_KEY = "hazuki_tabs";
  let tabsResizeObserver = null;

  const readTabStore = () => {
    try {
      const v = JSON.parse(sessionStorage.getItem(TAB_STORE_KEY) || "{}");
      return v && typeof v === "object" ? v : {};
    } catch {
      return {};
    }
  };

  const writeTabStore = (path, tab) => {
    try {
      const store = readTabStore();
      store[path] = tab;
      sessionStorage.setItem(TAB_STORE_KEY, JSON.stringify(store));
    } catch {
      // ignore (private mode, quota)
    }
  };

  const tabStrip = () => qs("[data-hz-tabs]") || qs("[data-hz-route-tabs]");

  const positionInk = (strip, animate) => {
    if (!strip) return;
    const ink = qs(".tab-ink", strip);
    if (!ink) return;
    const active = qs('.tab[aria-selected="true"], .tab[aria-current="page"]', strip);
    if (!active || !active.offsetWidth) {
      ink.style.opacity = "0";
      return;
    }
    const inset = 8;
    const x = active.offsetLeft + inset;
    const w = Math.max(8, active.offsetWidth - inset * 2);
    if (!animate) ink.classList.add("no-anim");
    ink.style.transform = `translateX(${x}px) scaleX(${w})`;
    ink.style.opacity = "1";
    if (!animate) {
      void ink.offsetWidth;
      ink.classList.remove("no-anim");
    }
  };

  const scrollTabIntoView = (strip, tab, animate) => {
    if (!strip || !tab || strip.scrollWidth <= strip.clientWidth + 1) return;
    const left = tab.offsetLeft - (strip.clientWidth - tab.offsetWidth) / 2;
    try {
      strip.scrollTo({ left: Math.max(0, left), behavior: animate && !reducedMotion() ? "smooth" : "auto" });
    } catch {
      strip.scrollLeft = Math.max(0, left);
    }
  };

  const syncTabForms = () => {
    for (const form of qsa("form[data-hz-tabform]")) {
      form.classList.toggle("hz-form-idle", !qs("[data-hz-panel]:not([hidden])", form));
    }
  };

  const activateTab = (name, { animate = true, store = true, focus = false } = {}) => {
    const strip = qs("[data-hz-tabs]");
    if (!strip || !name) return false;
    const tabs = qsa("[data-hz-tab]", strip);
    const target = tabs.find((t) => t.getAttribute("data-hz-tab") === name);
    if (!target) return false;

    for (const t of tabs) {
      const on = t === target;
      t.setAttribute("aria-selected", on ? "true" : "false");
      t.tabIndex = on ? 0 : -1;
    }
    for (const panel of qsa("[data-hz-panel]")) {
      const on = panel.getAttribute("data-hz-panel") === name;
      if (on && panel.hidden) {
        panel.hidden = false;
        if (animate) restartAnimation(panel, "panel-enter");
      } else if (!on && !panel.hidden) {
        panel.hidden = true;
      }
    }
    syncTabForms();
    positionInk(strip, animate);
    scrollTabIntoView(strip, target, animate);
    if (store) writeTabStore(window.location.pathname, name);
    if (focus) target.focus({ preventScroll: true });
    return true;
  };

  const initTabs = () => {
    if (tabsResizeObserver) {
      tabsResizeObserver.disconnect();
      tabsResizeObserver = null;
    }
    const strip = tabStrip();
    if (!strip) return;

    if (strip.hasAttribute("data-hz-tabs")) {
      const has = (name) => !!name && qsa("[data-hz-tab]", strip).some((t) => t.getAttribute("data-hz-tab") === name);
      let initial = "";
      const hash = decodeURIComponent((window.location.hash || "").slice(1));
      if (hash) {
        const el = document.getElementById(hash);
        const panel = el ? el.closest("[data-hz-panel]") : null;
        if (panel) initial = panel.getAttribute("data-hz-panel") || "";
        else if (has(hash)) initial = hash;
      }
      if (!initial) {
        const stored = readTabStore()[window.location.pathname];
        if (has(stored)) initial = stored;
      }
      if (!initial) {
        const cur = qs('[data-hz-tab][aria-selected="true"]', strip) || qs("[data-hz-tab]", strip);
        initial = cur ? cur.getAttribute("data-hz-tab") || "" : "";
      }
      activateTab(initial, { animate: false, store: false });
    } else {
      positionInk(strip, false);
      scrollTabIntoView(strip, qs('[aria-current="page"]', strip), false);
    }

    if (typeof ResizeObserver === "function") {
      tabsResizeObserver = new ResizeObserver(() => positionInk(strip, false));
      tabsResizeObserver.observe(strip);
    }
    if (document.fonts && document.fonts.ready) {
      document.fonts.ready.then(() => positionInk(strip, false)).catch(() => {});
    }
  };

  const onTabClick = (e) => {
    const tab = e.target instanceof Element ? e.target.closest("[data-hz-tab]") : null;
    if (!tab || !tab.closest("[data-hz-tabs]")) return;
    e.preventDefault();
    activateTab(tab.getAttribute("data-hz-tab") || "");
  };

  const onTabKeydown = (e) => {
    const tab = e.target instanceof Element ? e.target.closest("[data-hz-tab]") : null;
    if (!tab) return;
    const strip = tab.closest("[data-hz-tabs]");
    if (!strip) return;
    const tabs = qsa("[data-hz-tab]", strip);
    const idx = tabs.indexOf(tab);
    let next = -1;
    if (e.key === "ArrowRight") next = (idx + 1) % tabs.length;
    else if (e.key === "ArrowLeft") next = (idx - 1 + tabs.length) % tabs.length;
    else if (e.key === "Home") next = 0;
    else if (e.key === "End") next = tabs.length - 1;
    if (next < 0) return;
    e.preventDefault();
    activateTab(tabs[next].getAttribute("data-hz-tab") || "", { focus: true });
  };

  // A required field inside a hidden tab can't show its validation bubble:
  // switch to that tab first.
  const onInvalid = (e) => {
    const panel = e.target instanceof Element ? e.target.closest("[data-hz-panel]") : null;
    if (panel && panel.hidden) activateTab(panel.getAttribute("data-hz-panel") || "", { animate: false });
  };

  // ---------------------------------------------------------------------------
  // Drawer (mobile navigation)
  // ---------------------------------------------------------------------------
  const desktopQuery = window.matchMedia ? window.matchMedia("(min-width: 1024px)") : null;

  const setDrawer = (open) => {
    const body = document.body;
    if (!body) return;
    if (open && desktopQuery && desktopQuery.matches) open = false;
    const was = body.classList.contains("hz-drawer-open");
    if (was === open) return;
    body.classList.toggle("hz-drawer-open", open);
    for (const btn of qsa("[data-hz-drawer-open]")) btn.setAttribute("aria-expanded", open ? "true" : "false");
    if (open) {
      const target = qs("#hz-sidebar .nav-link.active") || qs("#hz-sidebar .nav-link");
      if (target) target.focus({ preventScroll: true });
    }
  };

  const onDrawerClick = (e) => {
    if (!(e.target instanceof Element)) return;
    if (e.target.closest("[data-hz-drawer-open]")) {
      e.preventDefault();
      setDrawer(true);
      return;
    }
    if (e.target.closest("[data-hz-drawer-close]")) {
      e.preventDefault();
      setDrawer(false);
      return;
    }
    if (e.target.closest("#hz-sidebar a[href]")) setDrawer(false);
  };

  let swipe = null;
  const onSidebarTouchStart = (e) => {
    if (!document.body.classList.contains("hz-drawer-open") || e.touches.length !== 1) return;
    swipe = { x: e.touches[0].clientX, y: e.touches[0].clientY };
  };
  const onSidebarTouchEnd = (e) => {
    if (!swipe) return;
    const t = e.changedTouches && e.changedTouches[0];
    const dx = t ? t.clientX - swipe.x : 0;
    const dy = t ? t.clientY - swipe.y : 0;
    swipe = null;
    if (dx < -60 && Math.abs(dy) < Math.abs(dx) * 0.7) setDrawer(false);
  };

  // ---------------------------------------------------------------------------
  // Toasts (server flash messages are lifted into them)
  // ---------------------------------------------------------------------------
  const dismissToast = (el) => {
    if (!el || !el.isConnected || el.classList.contains("leaving")) return;
    if (reducedMotion()) {
      el.remove();
      return;
    }
    el.classList.add("leaving");
    el.addEventListener("animationend", () => el.remove(), { once: true });
    setTimeout(() => el.remove(), 400);
  };

  const showToast = (kind, text, timeoutMs) => {
    const root = qs("#hz-toasts");
    const msgText = (text || "").toString().trim();
    if (!root || !msgText) return;

    const el = document.createElement("div");
    el.className = "toast " + (kind === "err" ? "err" : "ok");
    el.setAttribute("role", kind === "err" ? "alert" : "status");
    el.appendChild(iconEl(kind === "err" ? "alert" : "check-circle"));

    const msg = document.createElement("div");
    msg.className = "toast-msg";
    msg.textContent = msgText;
    el.appendChild(msg);

    const close = document.createElement("button");
    close.type = "button";
    close.className = "icon-btn";
    close.setAttribute("aria-label", tKey("ui.close", "Close"));
    close.appendChild(iconEl("x"));
    close.addEventListener("click", () => dismissToast(el));
    el.appendChild(close);

    root.appendChild(el);
    while (root.children.length > 4) root.firstElementChild.remove();

    const ms = typeof timeoutMs === "number" ? timeoutMs : kind === "err" ? 0 : 4200;
    if (ms > 0) {
      let timer = setTimeout(() => dismissToast(el), ms);
      el.addEventListener("pointerenter", () => clearTimeout(timer));
      el.addEventListener("pointerleave", () => {
        clearTimeout(timer);
        timer = setTimeout(() => dismissToast(el), 1800);
      });
    }
  };

  const clearErrorToasts = () => {
    for (const el of qsa("#hz-toasts .toast.err")) dismissToast(el);
  };

  const liftFlashes = () => {
    for (const flash of qsa("#pjax-root .flash")) {
      showToast(flash.classList.contains("err") ? "err" : "ok", flash.textContent);
      flash.remove();
    }
  };

  // ---------------------------------------------------------------------------
  // Unsaved-change tracking for forms marked [data-hz-dirty]
  //
  // "Dirty" means a named control differs from its server-rendered value
  // (defaultValue / defaultChecked / defaultSelected), so undoing an edit clears it.
  // Password managers and browser autofill write into fields without the user
  // typing there (e.g. the admin login landing in WORKER_SECRET_KEY); such
  // writes into never-typed text fields are reverted instead of being saved.
  // ---------------------------------------------------------------------------
  const TEXTLIKE_TYPES = new Set(["text", "password", "email", "url", "search", "tel", "number"]);
  const userTyped = new WeakSet();

  const isTextLike = (el) =>
    el instanceof HTMLTextAreaElement || (el instanceof HTMLInputElement && TEXTLIKE_TYPES.has(el.type));

  // Only trusted events count: extensions fill via synthetic (untrusted) events,
  // while a real click/tap or keystroke in the field precedes any user-chosen value.
  const markUserTyped = (e) => {
    const t = e.target;
    if (e.isTrusted && isTextLike(t)) userTyped.add(t);
  };

  const isTrackedControl = (el) =>
    (el instanceof HTMLInputElement || el instanceof HTMLTextAreaElement || el instanceof HTMLSelectElement) &&
    !!el.name &&
    !(el instanceof HTMLInputElement && el.type === "hidden") &&
    !!el.form &&
    el.form.hasAttribute("data-hz-dirty");

  const controlChanged = (el) => {
    if (el instanceof HTMLSelectElement) {
      return Array.from(el.options).some((o, i) => {
        const dflt = o.defaultSelected || (!el.multiple && i === 0 && !qs("option[selected]", el));
        return o.selected !== dflt;
      });
    }
    if (el instanceof HTMLInputElement && (el.type === "checkbox" || el.type === "radio")) {
      return el.checked !== el.defaultChecked;
    }
    return el.value !== el.defaultValue;
  };

  // Undo values that appeared in text fields the user never typed into.
  const revertUntypedText = (form) => {
    for (const el of Array.from(form.elements)) {
      if (!isTrackedControl(el) || !isTextLike(el) || userTyped.has(el)) continue;
      if (el.value !== el.defaultValue) el.value = el.defaultValue;
    }
  };

  const syncDirty = (form) => {
    const dirty = Array.from(form.elements).some((el) => isTrackedControl(el) && controlChanged(el));
    form.classList.toggle("is-dirty", dirty);
  };

  const onFieldEdited = (e) => {
    const t = e.target;
    if (!isTrackedControl(t)) return;
    if (isTextLike(t) && !userTyped.has(t) && t.value !== t.defaultValue) {
      t.value = t.defaultValue;
    }
    syncDirty(t.form);
  };

  const hasDirtyForm = () => !!qs("form.is-dirty");

  const confirmLeaveIfDirty = () => {
    if (!hasDirtyForm()) return Promise.resolve(true);
    return confirmModal({
      title: tKey("unsaved.title", "Discard unsaved changes?"),
      detail: tKey("unsaved.detail", ""),
      okText: tKey("unsaved.leave", "Leave"),
    });
  };

  const onBeforeUnload = (e) => {
    if (!hasDirtyForm()) return;
    e.preventDefault();
    e.returnValue = "";
  };

  // ---------------------------------------------------------------------------
  // Page lifecycle
  // ---------------------------------------------------------------------------
  let titleObserver = null;

  const watchPageTitle = () => {
    const bar = qs(".topbar");
    if (!bar) return;
    const titleEl = qs("#hz-title");
    if (titleEl) titleEl.textContent = (document.title || "").replace(/\s*-\s*Hazuki\s*$/, "");
    if (titleObserver) {
      titleObserver.disconnect();
      titleObserver = null;
    }
    bar.classList.remove("show-title");
    const h1 = qs("#pjax-root .page-title");
    if (!h1 || typeof IntersectionObserver !== "function") return;
    titleObserver = new IntersectionObserver(
      (entries) => {
        const entry = entries[entries.length - 1];
        if (!entry) return;
        bar.classList.toggle("show-title", !entry.isIntersecting && entry.boundingClientRect.top < 60);
      },
      { rootMargin: "-56px 0px 0px 0px" }
    );
    titleObserver.observe(h1);
  };

  const refreshPage = ({ swapped = false } = {}) => {
    if (swapped) {
      stopDashboardStats();
      stopTrafficPage();
      stopSystemPage();
    }
    updateNavActive();
    updateThemeToggle();
    applySecretFallback();
    liftFlashes();
    applyTimeFormatting();
    initTabs();
    updateSakuyaExampleUrls();
    updateGitPreview();
    updateCdnjsPreview();
    updateTorcherinoPreview();
    ensureDashboardStats();
    ensureTrafficPage();
    ensureSystemPage();
    watchPageTitle();
  };

  const onVisibilityChange = () => {
    if (document.hidden) return;
    const page = currentPage();
    if (page === "dashboard") pollDashboardStats();
    else if (page === "traffic") fetchTrafficSeries();
    else if (page === "system") pollSystemRewriteRuntime();
  };

  // ---------------------------------------------------------------------------
  // Navigation state
  // ---------------------------------------------------------------------------
  const navScore = (a, path) => {
    const hrefs = [a.getAttribute("href") || "", ...(a.getAttribute("data-hz-also") || "").split(/\s+/)];
    let best = -1;
    for (const raw of hrefs) {
      const h = raw.split("?")[0].trim();
      if (!h) continue;
      if (h === path) best = Math.max(best, 10000 + h.length);
      else if (h !== "/" && path.startsWith(h + "/")) best = Math.max(best, h.length);
    }
    return best;
  };

  const bestNavLink = (pathname) => {
    const path = (pathname || window.location.pathname || "").toString();
    let best = null;
    let bestScore = -1;
    for (const a of qsa("[data-hz-nav]")) {
      const s = navScore(a, path);
      if (s > bestScore) {
        best = a;
        bestScore = s;
      }
    }
    return bestScore >= 0 ? best : null;
  };

  const updateNavActive = (pathname) => {
    const best = bestNavLink(pathname);
    for (const a of qsa("[data-hz-nav]")) {
      const on = a === best;
      a.classList.toggle("active", on);
      if (on) a.setAttribute("aria-current", "page");
      else a.removeAttribute("aria-current");
    }
  };

  const setNavPending = (pathname) => {
    document.body.classList.remove("hz-loaded");
    document.body.classList.add("hz-loading");
    const root = qs("#pjax-root");
    if (root) root.setAttribute("aria-busy", "true");
    const best = bestNavLink(pathname);
    for (const a of qsa("[data-hz-nav]")) a.classList.toggle("hz-pending", a === best);
  };

  let loadedTimer = null;
  const clearNavPending = () => {
    const body = document.body;
    if (body.classList.contains("hz-loading")) {
      body.classList.remove("hz-loading");
      body.classList.add("hz-loaded");
      clearTimeout(loadedTimer);
      loadedTimer = setTimeout(() => body.classList.remove("hz-loaded"), 450);
    }
    const root = qs("#pjax-root");
    if (root) root.removeAttribute("aria-busy");
    for (const a of qsa("[data-hz-nav].hz-pending")) a.classList.remove("hz-pending");
  };

  // ---------------------------------------------------------------------------
  // PJAX navigation (GET links and opted-in POST forms)
  // ---------------------------------------------------------------------------
  let navAbortController = null;
  let navSeq = 0;
  let vtDepth = 0;

  const isSameLayout = (doc) => {
    const curHasSidebar = !!qs(".sidebar");
    const nextHasSidebar = !!(doc && doc.querySelector && doc.querySelector(".sidebar"));
    return curHasSidebar === nextHasSidebar;
  };

  const parseHtml = (html) => {
    try {
      return new DOMParser().parseFromString(html, "text/html");
    } catch {
      return null;
    }
  };

  const applyDocToDom = (doc) => {
    const root = qs("#pjax-root");
    const nextRoot = doc ? doc.querySelector("#pjax-root") : null;
    if (!root || !nextRoot) return false;

    root.replaceChildren(...Array.from(nextRoot.childNodes).map((n) => document.importNode(n, true)));

    const page = (doc.body && doc.body.getAttribute("data-page")) || "";
    if (page) document.body.setAttribute("data-page", page);

    const title = doc.title || "";
    if (title) document.title = title;

    refreshPage({ swapped: true });
    return true;
  };

  const swapWithTransition = async (doc, isValid, beforeApply) => {
    let applied = false;
    let ok = false;
    const apply = () => {
      if (applied) return;
      applied = true;
      if (isValid && !isValid()) return;
      if (beforeApply) beforeApply();
      ok = applyDocToDom(doc);
    };

    if (typeof document.startViewTransition === "function" && !reducedMotion()) {
      const html = document.documentElement;
      vtDepth += 1;
      html.classList.add("hz-vt-nav");
      const done = () => {
        vtDepth = Math.max(0, vtDepth - 1);
        if (!vtDepth) html.classList.remove("hz-vt-nav");
      };
      try {
        const vt = document.startViewTransition(apply);
        vt.finished.then(done, done);
        await vt.updateCallbackDone;
        return ok;
      } catch {
        if (!applied) done();
      }
    }

    if (!applied) {
      apply();
      if (ok) restartAnimation(qs("#pjax-root"), "hz-page-enter");
    }
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
    if (p.startsWith("/_hazuki/")) return true;
    if (p === "/favicon.ico" || p === "/fav.png") return true;
    if (p === "/lang") return true; // needs full reload to update layout translations

    return false;
  };

  const nativeSubmit = (form) => {
    if (!form) return;
    form.classList.remove("is-dirty");
    try {
      HTMLFormElement.prototype.submit.call(form);
    } catch {
      // ignore
    }
  };

  const navigate = async (url, opts = {}) => {
    const { replace = false, addHistory = true, method = "GET", body = null, form = null, keepScroll = false } = opts;
    if (!url) return;
    const isPost = method === "POST";
    if (!canPjax()) {
      if (isPost) nativeSubmit(form);
      else window.location.href = url.href;
      return;
    }

    const mySeq = (navSeq += 1);
    abortQuietly(navAbortController);
    const controller = typeof AbortController === "function" ? new AbortController() : null;
    navAbortController = controller;

    clearErrorToasts();
    if (!isPost) updateNavActive(url.pathname);
    setNavPending(isPost ? window.location.pathname : url.pathname);

    const scrollY = window.scrollY;

    try {
      const fetchOpts = {
        method,
        headers: { Accept: "text/html", "X-Hazuki-Pjax": "1" },
        credentials: "same-origin",
      };
      if (body) fetchOpts.body = body;
      if (controller) fetchOpts.signal = controller.signal;

      const resp = await fetch(url.href, fetchOpts);
      if (mySeq !== navSeq) return;

      const ct = (resp.headers.get("content-type") || "").toLowerCase();
      if (!ct.includes("text/html") || (!resp.ok && !isPost)) {
        if (isPost) nativeSubmit(form);
        else window.location.href = url.href;
        return;
      }

      const html = await resp.text();
      if (mySeq !== navSeq) return;
      const doc = parseHtml(html);
      if (!doc || !doc.documentElement || !doc.querySelector("#pjax-root")) {
        if (isPost) nativeSubmit(form);
        else window.location.href = url.href;
        return;
      }

      const finalURL = new URL(resp.url || url.href, window.location.href);
      if (!isPost && url.hash && !finalURL.hash) finalURL.hash = url.hash;

      if (!isSameLayout(doc)) {
        window.location.href = finalURL.href;
        return;
      }

      const samePage = finalURL.pathname === window.location.pathname;
      const swapped = await swapWithTransition(
        doc,
        () => mySeq === navSeq,
        () => {
          if (addHistory) {
            const st = { href: finalURL.href };
            if (replace || (isPost && finalURL.href === window.location.href)) {
              window.history.replaceState(st, "", finalURL.href);
            } else {
              window.history.pushState(st, "", finalURL.href);
            }
          }
          const y = keepScroll && samePage ? scrollY : 0;
          window.scrollTo(0, y);
        }
      );
      if (mySeq !== navSeq) return;
      if (!swapped) {
        window.location.href = finalURL.href;
        return;
      }

      if (finalURL.hash && !isPost) {
        let target = null;
        try {
          target = document.getElementById(decodeURIComponent(finalURL.hash.slice(1)));
        } catch {
          target = null;
        }
        if (target) target.scrollIntoView({ block: "start" });
      }
    } catch {
      if (controller && controller.signal && controller.signal.aborted) return;
      if (isPost) showToast("err", tKey("request.failed", "Request failed."));
      else window.location.href = url.href;
    } finally {
      if (mySeq === navSeq) clearNavPending();
    }
  };

  const onFormSubmit = (e) => {
    const form = e.target;
    if (!(form instanceof HTMLFormElement)) return;
    if (e.defaultPrevented) return;
    // Autofill may have filled fields without firing events; never submit those.
    if (form.hasAttribute("data-hz-dirty")) revertUntypedText(form);
    if (!form.hasAttribute("data-hz-pjax") || !canPjax()) {
      form.classList.remove("is-dirty");
      return;
    }
    if ((form.getAttribute("method") || "get").toLowerCase() !== "post") return;
    if ((form.getAttribute("enctype") || "").toLowerCase().includes("multipart")) return;

    const submitter = e.submitter instanceof HTMLElement ? e.submitter : null;
    const actionAttr = (submitter && submitter.getAttribute("formaction")) || form.getAttribute("action") || window.location.href;
    const action = new URL(actionAttr, window.location.href);
    if (action.origin !== window.location.origin) return;

    e.preventDefault();

    let data;
    try {
      data = submitter ? new FormData(form, submitter) : new FormData(form);
    } catch {
      data = new FormData(form);
      if (submitter && submitter.getAttribute("name")) {
        data.append(submitter.getAttribute("name"), submitter.getAttribute("value") || "");
      }
    }
    const params = new URLSearchParams();
    for (const [k, v] of data.entries()) {
      if (typeof v === "string") params.append(k, v);
    }

    if (submitter instanceof HTMLButtonElement) submitter.disabled = true;
    form.classList.remove("is-dirty");
    navigate(action, { method: "POST", body: params, form, keepScroll: true }).finally(() => {
      if (submitter instanceof HTMLButtonElement && submitter.isConnected) submitter.disabled = false;
    });
  };

  // ---------------------------------------------------------------------------
  // Confirm modal
  // ---------------------------------------------------------------------------
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
        document.removeEventListener("keydown", onKeyDown, true);
        okBtn.removeEventListener("click", onOk);
        for (const el of cancelEls) el.removeEventListener("click", onCancel);
      };

      const finish = (ok) => {
        if (finished) return;
        finished = true;
        cleanup();

        document.body.classList.remove("hz-modal-open");
        root.hidden = true;

        if (prev && typeof prev.focus === "function") {
          try {
            prev.focus({ preventScroll: true });
          } catch {
            // ignore
          }
        }
        resolve(ok);
      };

      const onKeyDown = (evt) => {
        if (evt.key === "Escape") {
          evt.preventDefault();
          finish(false);
          return;
        }
        if (evt.key === "Tab") {
          const focusables = qsa("button", root).filter((b) => b.offsetParent !== null);
          if (!focusables.length) return;
          const first = focusables[0];
          const last = focusables[focusables.length - 1];
          if (evt.shiftKey && document.activeElement === first) {
            evt.preventDefault();
            last.focus();
          } else if (!evt.shiftKey && document.activeElement === last) {
            evt.preventDefault();
            first.focus();
          }
        }
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
      const danger = /删除|清空|清理|轮换|回滚|delete|clear|rotate|restore|discard|leave|离开|放弃/i.test(t + " " + okBtn.textContent);
      okBtn.classList.toggle("danger", danger);
      okBtn.classList.toggle("primary", !danger);

      for (const el of cancelEls) {
        if (el instanceof HTMLButtonElement) el.textContent = tKey("modal.cancel", "取消");
      }

      root.hidden = false;
      document.body.classList.add("hz-modal-open");

      document.addEventListener("keydown", onKeyDown, true);
      okBtn.addEventListener("click", onOk);
      for (const el of cancelEls) el.addEventListener("click", onCancel);

      try {
        okBtn.focus({ preventScroll: true });
      } catch {
        // ignore
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
        if (typeof form.requestSubmit === "function") form.requestSubmit(btn);
        else nativeSubmit(form);
      } catch {
        nativeSubmit(form);
      }
    });
  };

  // ---------------------------------------------------------------------------
  // Copy helpers
  // ---------------------------------------------------------------------------
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

    e.preventDefault();
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
    if (url.href === window.location.href && !hasDirtyForm()) {
      navigate(url, { replace: true });
      return;
    }
    confirmLeaveIfDirty().then((ok) => {
      if (!ok) return;
      for (const f of qsa("form.is-dirty")) f.classList.remove("is-dirty");
      navigate(url);
    });
  };

  const onPopState = () => {
    for (const f of qsa("form.is-dirty")) f.classList.remove("is-dirty");
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
    }
  };

  const onKeydown = (e) => {
    if (e.key === "Escape" && document.body.classList.contains("hz-drawer-open")) {
      setDrawer(false);
      const opener = qs("[data-hz-drawer-open]");
      if (opener) opener.focus({ preventScroll: true });
    }
  };

  document.addEventListener("click", onDrawerClick);
  document.addEventListener("click", onTabClick);
  document.addEventListener("keydown", onTabKeydown);
  document.addEventListener("keydown", onKeydown);
  document.addEventListener("click", onFormatJsonClick);
  document.addEventListener("change", onTogglePassword);
  document.addEventListener("click", onConfirmSubmitClick);
  document.addEventListener("click", onThemeToggleClick);
  document.addEventListener("click", onSakuyaCopyExampleClick);
  document.addEventListener("click", onCopyClick);
  document.addEventListener("click", onLinkClick);
  document.addEventListener("submit", onFormSubmit);
  document.addEventListener("invalid", onInvalid, true);
  document.addEventListener("input", onPreviewInput);
  document.addEventListener("change", onPreviewInput);
  for (const type of ["keydown", "paste", "drop", "compositionstart", "pointerdown"]) {
    document.addEventListener(type, markUserTyped, true);
  }
  document.addEventListener("input", onFieldEdited);
  document.addEventListener("change", onFieldEdited);
  document.addEventListener("visibilitychange", onVisibilityChange);
  document.addEventListener("touchstart", (e) => {
    if (e.target instanceof Element && e.target.closest("#hz-sidebar")) onSidebarTouchStart(e);
  }, { passive: true });
  document.addEventListener("touchend", onSidebarTouchEnd, { passive: true });
  window.addEventListener("popstate", onPopState);
  window.addEventListener("beforeunload", onBeforeUnload);
  window.addEventListener("hashchange", () => initTabs());
  window.addEventListener("storage", (e) => {
    if (!e || e.key !== THEME_KEY) return;
    applyTheme(getTheme());
    updateThemeToggle();
  });
  if (desktopQuery) {
    const onDesktopChange = () => {
      if (desktopQuery.matches) setDrawer(false);
    };
    if (typeof desktopQuery.addEventListener === "function") desktopQuery.addEventListener("change", onDesktopChange);
    else if (typeof desktopQuery.addListener === "function") desktopQuery.addListener(onDesktopChange);
  }

  applyTheme(getTheme());

  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", () => refreshPage(), { once: true });
  } else {
    refreshPage();
  }

  window.HazukiUI = { qs, qsa, formatJson, navigate, showToast, activateTab };
})();
