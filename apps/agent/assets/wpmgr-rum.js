"use strict";
(() => {
  var __defProp = Object.defineProperty;
  var __getOwnPropSymbols = Object.getOwnPropertySymbols;
  var __hasOwnProp = Object.prototype.hasOwnProperty;
  var __propIsEnum = Object.prototype.propertyIsEnumerable;
  var __defNormalProp = (obj, key, value) => key in obj ? __defProp(obj, key, { enumerable: true, configurable: true, writable: true, value }) : obj[key] = value;
  var __spreadValues = (a2, b2) => {
    for (var prop in b2 || (b2 = {}))
      if (__hasOwnProp.call(b2, prop))
        __defNormalProp(a2, prop, b2[prop]);
    if (__getOwnPropSymbols)
      for (var prop of __getOwnPropSymbols(b2)) {
        if (__propIsEnum.call(b2, prop))
          __defNormalProp(a2, prop, b2[prop]);
      }
    return a2;
  };
  var __publicField = (obj, key, value) => __defNormalProp(obj, typeof key !== "symbol" ? key + "" : key, value);

  // ../../node_modules/.pnpm/web-vitals@5.3.0/node_modules/web-vitals/dist/web-vitals.js
  var e = -1;
  var t = (t2) => {
    addEventListener("pageshow", (n2) => {
      n2.persisted && (e = n2.timeStamp, t2(n2));
    }, true);
  };
  var n = (e2, t2, n2, i2) => {
    let s2, o2;
    return (r2) => {
      t2.value >= 0 && (r2 || i2) && (o2 = t2.value - (s2 != null ? s2 : 0), (o2 || void 0 === s2) && (s2 = t2.value, t2.delta = o2, t2.rating = ((e3, t3) => e3 > t3[1] ? "poor" : e3 > t3[0] ? "needs-improvement" : "good")(t2.value, n2), e2(t2)));
    };
  };
  var i = (e2) => {
    requestAnimationFrame(() => requestAnimationFrame(e2));
  };
  var s = () => {
    const e2 = performance.getEntriesByType("navigation")[0];
    if (e2 && e2.responseStart > 0 && e2.responseStart < performance.now()) return e2;
  };
  var o = () => {
    var _a, _b;
    return (_b = (_a = s()) == null ? void 0 : _a.activationStart) != null ? _b : 0;
  };
  var r = (t2, n2 = -1) => {
    const i2 = s();
    let r2 = "navigate";
    e >= 0 ? r2 = "back-forward-cache" : i2 && (document.prerendering || o() > 0 ? r2 = "prerender" : document.wasDiscarded ? r2 = "restore" : i2.type && (r2 = i2.type.replace(/_/g, "-")));
    return { name: t2, value: n2, rating: "good", delta: 0, entries: [], id: "v5-".concat(Date.now(), "-").concat(Math.floor(8999999999999 * Math.random()) + 1e12), navigationType: r2 };
  };
  var c = /* @__PURE__ */ new WeakMap();
  function a(e2, t2) {
    let n2 = c.get(t2);
    return n2 || (n2 = /* @__PURE__ */ new WeakMap(), c.set(t2, n2)), n2.get(e2) || n2.set(e2, new t2()), n2.get(e2);
  }
  var d = class {
    constructor() {
      __publicField(this, "t");
      __publicField(this, "i", 0);
      __publicField(this, "o", []);
    }
    h(e2) {
      var _a;
      if (e2.hadRecentInput) return;
      const t2 = this.o[0], n2 = this.o.at(-1);
      this.i && t2 && n2 && e2.startTime - n2.startTime < 1e3 && e2.startTime - t2.startTime < 5e3 ? (this.i += e2.value, this.o.push(e2)) : (this.i = e2.value, this.o = [e2]), (_a = this.t) == null ? void 0 : _a.call(this, e2);
    }
  };
  var h = (e2, t2, n2 = {}) => {
    try {
      if (PerformanceObserver.supportedEntryTypes.includes(e2)) {
        const i2 = new PerformanceObserver((e3) => {
          queueMicrotask(() => {
            t2(e3.getEntries());
          });
        });
        return i2.observe(__spreadValues({ type: e2, buffered: true }, n2)), i2;
      }
    } catch (e3) {
    }
  };
  var f = (e2) => {
    let t2 = false;
    return () => {
      t2 || (e2(), t2 = true);
    };
  };
  var l = -1;
  var u = /* @__PURE__ */ new Set();
  var m = () => "hidden" !== document.visibilityState || document.prerendering ? 1 / 0 : 0;
  var p = (e2) => {
    if ("hidden" === document.visibilityState) {
      if ("visibilitychange" === e2.type) for (const e3 of u) e3();
      isFinite(l) || (l = "visibilitychange" === e2.type ? e2.timeStamp : 0, removeEventListener("prerenderingchange", p, true));
    }
  };
  var g = () => {
    var _a;
    if (l < 0) {
      const e2 = o(), n2 = document.prerendering ? void 0 : (_a = globalThis.performance.getEntriesByType("visibility-state").find((t2) => "hidden" === t2.name && t2.startTime >= e2)) == null ? void 0 : _a.startTime;
      l = n2 != null ? n2 : m(), addEventListener("visibilitychange", p, true), addEventListener("prerenderingchange", p, true), t(() => {
        setTimeout(() => {
          l = m();
        });
      });
    }
    return { get firstHiddenTime() {
      return l;
    }, onHidden(e2) {
      u.add(e2);
    } };
  };
  var v = (e2) => {
    document.prerendering ? addEventListener("prerenderingchange", e2, true) : e2();
  };
  var y = [1800, 3e3];
  var T = (e2, s2 = {}) => {
    v(() => {
      const c2 = g();
      let a2, d2 = r("FCP");
      const f2 = h("paint", (e3) => {
        for (const t2 of e3) "first-contentful-paint" === t2.name && (f2.disconnect(), t2.startTime < c2.firstHiddenTime && (d2.value = Math.max(t2.startTime - o(), 0), d2.entries.push(t2), a2(true)));
      });
      f2 && (a2 = n(e2, d2, y, s2.reportAllChanges), t((t2) => {
        d2 = r("FCP"), a2 = n(e2, d2, y, s2.reportAllChanges), i(() => {
          d2.value = performance.now() - t2.timeStamp, a2(true);
        });
      }));
    });
  };
  var E = [0.1, 0.25];
  var b = (e2, s2 = {}) => {
    const o2 = g();
    T(f(() => {
      let c2, f2 = r("CLS", 0);
      const l2 = a(s2, d), u2 = (e3) => {
        for (const t2 of e3) l2.h(t2);
        l2.i > f2.value && (f2.value = l2.i, f2.entries = l2.o, c2());
      }, m2 = h("layout-shift", u2);
      m2 && (c2 = n(e2, f2, E, s2.reportAllChanges), o2.onHidden(() => {
        u2(m2.takeRecords()), c2(true);
      }), t(() => {
        l2.i = 0, f2 = r("CLS", 0), c2 = n(e2, f2, E, s2.reportAllChanges), i(c2);
      }), setTimeout(c2));
    }));
  };
  var L = 0;
  var P = 1 / 0;
  var _ = 0;
  var M = (e2) => {
    for (const t2 of e2) t2.interactionId && (P = Math.min(P, t2.interactionId), _ = Math.max(_, t2.interactionId), L = _ ? (_ - P) / 7 + 1 : 0);
  };
  var w;
  var C = () => {
    var _a;
    return w ? L : (_a = performance.interactionCount) != null ? _a : 0;
  };
  var I = () => {
    "interactionCount" in performance || w || (w = h("event", M, { durationThreshold: 0 }));
  };
  var F = 0;
  var k = class {
    constructor() {
      __publicField(this, "l", []);
      __publicField(this, "u", /* @__PURE__ */ new Map());
      __publicField(this, "m");
      __publicField(this, "p");
    }
    v() {
      F = C(), this.l.length = 0, this.u.clear();
    }
    T() {
      const e2 = Math.min(this.l.length - 1, Math.floor((C() - F) / 50));
      return this.l[e2];
    }
    h(e2) {
      var _a, _b;
      if ((_a = this.m) == null ? void 0 : _a.call(this, e2), !e2.interactionId && "first-input" !== e2.entryType) return;
      const t2 = this.l.at(-1);
      let n2 = this.u.get(e2.interactionId);
      if (n2 || this.l.length < 10 || e2.duration > t2.L) {
        if (n2 ? e2.duration > n2.L ? (n2.entries = [e2], n2.L = e2.duration) : e2.duration === n2.L && e2.startTime === n2.entries[0].startTime && n2.entries.push(e2) : (n2 = { id: e2.interactionId, entries: [e2], L: e2.duration }, this.u.set(n2.id, n2), this.l.push(n2)), this.l.sort((e3, t3) => t3.L - e3.L), this.l.length > 10) {
          const e3 = this.l.splice(10);
          for (const t3 of e3) this.u.delete(t3.id);
        }
        (_b = this.p) == null ? void 0 : _b.call(this, n2);
      }
    }
  };
  var A = (e2) => {
    const t2 = globalThis.requestIdleCallback || setTimeout, n2 = globalThis.cancelIdleCallback || clearTimeout;
    if ("hidden" === document.visibilityState) e2();
    else {
      const i2 = f(e2);
      let s2 = -1;
      const o2 = () => {
        n2(s2), i2();
      };
      addEventListener("visibilitychange", o2, { once: true, capture: true }), s2 = t2(() => {
        removeEventListener("visibilitychange", o2, { capture: true }), i2();
      });
    }
  };
  var B = [200, 500];
  var S = (e2, i2 = {}) => {
    if (!globalThis.PerformanceEventTiming || !("interactionId" in PerformanceEventTiming.prototype)) return;
    const s2 = g();
    v(() => {
      var _a;
      I();
      let o2, c2 = r("INP");
      const d2 = a(i2, k), f2 = (e3) => {
        A(() => {
          for (const t3 of e3) d2.h(t3);
          const t2 = d2.T();
          t2 && t2.L !== c2.value && (c2.value = t2.L, c2.entries = t2.entries, o2());
        });
      }, l2 = h("event", f2, { durationThreshold: (_a = i2.durationThreshold) != null ? _a : 40 });
      o2 = n(e2, c2, B, i2.reportAllChanges), l2 && (l2.observe({ type: "first-input", buffered: true }), s2.onHidden(() => {
        f2(l2.takeRecords()), o2(true);
      }), t(() => {
        d2.v(), c2 = r("INP"), o2 = n(e2, c2, B, i2.reportAllChanges);
      }));
    });
  };
  var q = class {
    constructor() {
      __publicField(this, "m");
    }
    h(e2) {
      var _a;
      (_a = this.m) == null ? void 0 : _a.call(this, e2);
    }
  };
  var N = [2500, 4e3];
  var x = (e2, s2 = {}) => {
    v(() => {
      const c2 = g();
      let d2, l2 = r("LCP");
      const u2 = a(s2, q), m2 = (e3) => {
        s2.reportAllChanges || (e3 = e3.slice(-1));
        for (const t2 of e3) u2.h(t2), t2.startTime < c2.firstHiddenTime && (l2.value = Math.max(t2.startTime - o(), 0), l2.entries = [t2], d2());
      }, p2 = h("largest-contentful-paint", m2);
      if (p2) {
        d2 = n(e2, l2, N, s2.reportAllChanges);
        const o2 = f(() => {
          m2(p2.takeRecords()), p2.disconnect(), d2(true);
        }), c3 = (e3) => {
          e3.isTrusted && (A(o2), removeEventListener(e3.type, c3, { capture: true }));
        };
        for (const e3 of ["keydown", "click", "visibilitychange"]) addEventListener(e3, c3, { capture: true });
        t((t2) => {
          l2 = r("LCP"), d2 = n(e2, l2, N, s2.reportAllChanges), i(() => {
            l2.value = performance.now() - t2.timeStamp, d2(true);
          });
        });
      }
    });
  };
  var H = [800, 1800];
  var O = (e2) => {
    document.prerendering ? v(() => O(e2)) : "complete" !== document.readyState ? addEventListener("load", () => O(e2), true) : setTimeout(e2);
  };
  var W = (e2, i2 = {}) => {
    let c2 = r("TTFB"), a2 = n(e2, c2, H, i2.reportAllChanges);
    O(() => {
      const d2 = s();
      d2 && (c2.value = Math.max(d2.responseStart - o(), 0), c2.entries = [d2], a2(true), t(() => {
        c2 = r("TTFB", 0), a2 = n(e2, c2, H, i2.reportAllChanges), a2(true);
      }));
    });
  };

  // src/vitals.ts
  function deviceType() {
    var _a, _b;
    try {
      if ((_a = navigator.userAgentData) == null ? void 0 : _a.mobile) {
        return window.innerWidth >= 768 ? "tablet" : "mobile";
      }
      const ua = (_b = navigator.userAgent) != null ? _b : "";
      if (/tablet|ipad|playbook|silk/i.test(ua)) {
        return "tablet";
      }
      if (/mobile|android|iphone|ipod|blackberry|iemobile|opera mini/i.test(ua)) {
        return "mobile";
      }
      return "desktop";
    } catch (e2) {
      return "desktop";
    }
  }
  function connType() {
    var _a;
    try {
      if (!navigator.onLine) {
        return "offline";
      }
      const et = (_a = navigator.connection) == null ? void 0 : _a.effectiveType;
      if (et === "4g" || et === "3g" || et === "2g" || et === "slow-2g") {
        return et;
      }
      return "unknown";
    } catch (e2) {
      return "unknown";
    }
  }
  function stripQuery(href) {
    try {
      const u2 = new URL(href);
      return u2.origin + u2.pathname;
    } catch (e2) {
      return href;
    }
  }
  function metricValue(name, value) {
    if (name === "CLS") {
      return Math.round(value * 1e3);
    }
    return Math.round(value);
  }
  function makeSender(config, pageUrl, device, conn) {
    return (m2) => {
      try {
        const body = JSON.stringify({
          key: config.key,
          url: pageUrl,
          metric: m2.name.toLowerCase(),
          value: metricValue(m2.name, m2.value),
          device,
          conn
        });
        const blob = new Blob([body], { type: "text/plain" });
        navigator.sendBeacon(config.url, blob);
      } catch (e2) {
      }
    };
  }
  function init() {
    try {
      if (!("sendBeacon" in navigator)) {
        return;
      }
      if (typeof window.PerformanceObserver === "undefined") {
        return;
      }
      const config = window.__WPMGR_RUM__;
      if (!config || typeof config.key !== "string" || config.key === "") {
        return;
      }
      if (typeof config.url !== "string" || config.url === "") {
        return;
      }
      const rate = typeof config.rate === "number" ? config.rate : 1;
      if (Math.random() >= rate) {
        return;
      }
      const pageUrl = stripQuery(window.location.href);
      const device = deviceType();
      const conn = connType();
      const send = makeSender(config, pageUrl, device, conn);
      W(send);
      T(send);
      x(send);
      b(send);
      S(send);
    } catch (e2) {
    }
  }

  // src/index.ts
  init();
})();
