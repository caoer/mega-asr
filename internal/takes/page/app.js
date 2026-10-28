// The Takes page: every take of megavoice's store, newest first, grouped by
// day, with search and filters, from the API under /takes/api/. Every record
// field is set as text, never as HTML: a window title or an engine's answer
// is text others control. The page's Content-Security-Policy runs no inline
// script or style, so nothing here writes a style attribute; a position is
// set through element.style.
//
// The detail beside the list is the container #detail; data-take on it is
// the selected take's id.
(function () {
  "use strict";

  // The page's language, fixed at load: Chinese when the html's lang (which
  // lang.js sets from megavoice's language) is zh, English otherwise. tr is
  // every word the page shows, in both: tr(zh, en).
  var ZH = /^zh/i.test(document.documentElement.lang || "");
  function tr(zh, en) {
    return ZH ? zh : en;
  }
  // takesN and charsN are counts in English: 1 take, 3 takes.
  function takesN(n) {
    return n + (n === 1 ? " take" : " takes");
  }
  function charsN(n) {
    return n + (n === 1 ? " character" : " characters");
  }
  var MON = ["Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"];
  // the title, which megavoice knows the page's window by; the title
  // element precedes this script
  document.title = tr("录音历史 · megavoice", "Recordings · megavoice");
  // localize puts the English of the html's own text in place: data-en is
  // an element's text, data-en-<attr> one of its attributes.
  function localize() {
    if (ZH) return;
    document.querySelectorAll("*").forEach(function (el) {
      Array.prototype.slice.call(el.attributes).forEach(function (a) {
        if (a.name === "data-en") el.textContent = a.value;
        else if (a.name.slice(0, 8) === "data-en-") el.setAttribute(a.name.slice(8), a.value);
      });
    });
  }

  // The theme before the first paint: this file loads in <head>.
  try {
    var saved = localStorage.getItem("takes.theme");
    if (saved === "light" || saved === "dark") document.documentElement.dataset.theme = saved;
  } catch (e) {}

  // ------------------------------------------------------------ requests

  var Refused = new Error("refused");

  // The listener's key: megavoice opens the page with #k=<key>, which a
  // browser never sends to a server. This tab keeps it in sessionStorage,
  // which a browser keeps per origin, port included; a new #k= (the menu
  // reopening this tab) reloads the page with it.
  var hashKey = new URLSearchParams(location.hash.slice(1)).get("k");
  if (hashKey) {
    sessionStorage.setItem("megavoice-key", hashKey);
    history.replaceState(null, "", location.pathname + location.search);
  }
  addEventListener("hashchange", function () {
    if (new URLSearchParams(location.hash.slice(1)).get("k")) location.reload();
  });

  // stopped: the listener refused the key, or did not answer. megavoice
  // rotates the key at every start, and while it is down another process
  // may hold the port, so the tab drops the key and sends no request again
  // until the menu reopens it with a new #k=.
  var stopped = false;
  function stop() {
    stopped = true;
    sessionStorage.removeItem("megavoice-key");
    return Refused;
  }

  // api is the one door of every request the page makes: the list, the
  // take (its label included), its peaks, its audio and the actions, all
  // under /takes/. It sends the key as the X-Megavoice-Key header; the
  // guard answers 403 without it. The answer is JSON, or a Blob when
  // init.blob is set (a take's audio: an <audio> element sends no header,
  // so the player plays a blob: URL). A refused request's error carries the
  // server's code (err.code, worded by refusal) and its words. A GET gives
  // up after 10 s, an action waits for its answer (a re-transcription takes
  // as long as the decode). A 403, or no answer, stops the tab: Refused.
  function api(path, init) {
    if (stopped) return Promise.reject(Refused);
    init = init || {};
    var headers = Object.assign({ "X-Megavoice-Key": sessionStorage.getItem("megavoice-key") || "" }, init.headers);
    var ctl = new AbortController();
    var timer = init.method ? 0 : setTimeout(function () { ctl.abort(); }, 10000);
    return fetch("/takes/" + path, Object.assign({ cache: "no-store", signal: ctl.signal }, init, { headers: headers })).then(function (r) {
      clearTimeout(timer);
      if (r.status === 403) throw stop();
      if (!r.ok)
        return r.text().then(function (s) {
          var j = null;
          try {
            j = JSON.parse(s);
          } catch (x) {}
          var e = new Error((j && j.error) || s.trim() || r.status + " " + r.statusText);
          e.status = r.status;
          e.code = (j && j.code) || "";
          throw e;
        });
      return init.blob ? r.blob() : r.json();
    }, function () {
      clearTimeout(timer);
      throw stop();
    });
  }
  function postJSON(path, body) {
    return api(path, { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(body) });
  }

  // ------------------------------------------------------------ helpers

  function h(tag, props) {
    var el = document.createElement(tag);
    if (props)
      for (var k in props) {
        var v = props[k];
        if (v == null || v === false) continue;
        if (k === "class") el.className = v;
        else if (k === "text") el.textContent = v;
        else if (k.slice(0, 2) === "on") el.addEventListener(k.slice(2), v);
        else el.setAttribute(k, v === true ? "" : v);
      }
    for (var i = 2; i < arguments.length; i++) add(el, arguments[i]);
    return el;
  }
  // add appends each child: an element, a string as text, an array of them;
  // null and false are skipped.
  function add(el) {
    for (var i = 1; i < arguments.length; i++) {
      var c = arguments[i];
      if (c == null || c === false) continue;
      if (Array.isArray(c)) c.forEach(function (x) { add(el, x); });
      else el.appendChild(typeof c === "string" ? document.createTextNode(c) : c);
    }
  }
  function svg(w, hgt, body) {
    var s = document.createElementNS("http://www.w3.org/2000/svg", "svg");
    s.setAttribute("width", w);
    s.setAttribute("height", hgt);
    s.setAttribute("viewBox", "0 0 " + w + " " + hgt);
    s.setAttribute("aria-hidden", "true");
    s.innerHTML = body; // fixed markup of this file, never a record field
    return s;
  }
  function p2(n) {
    return String(n).padStart(2, "0");
  }
  function dur(s) {
    s = Math.max(0, Math.round(s || 0));
    return Math.floor(s / 60) + ":" + p2(s % 60);
  }
  function hms(d) {
    return p2(d.getHours()) + ":" + p2(d.getMinutes()) + ":" + p2(d.getSeconds());
  }
  function hm(d) {
    return p2(d.getHours()) + ":" + p2(d.getMinutes());
  }
  var WD = tr(["周日", "周一", "周二", "周三", "周四", "周五", "周六"], ["Sun", "Mon", "Tue", "Wed", "Thu", "Fri", "Sat"]);
  function dayKey(d) {
    return d.getFullYear() + "-" + p2(d.getMonth() + 1) + "-" + p2(d.getDate());
  }
  // dayLabel is a day key (YYYY-MM-DD) as the day heads read it.
  function dayLabel(key) {
    var d = new Date(+key.slice(0, 4), +key.slice(5, 7) - 1, +key.slice(8, 10));
    return (key === dayKey(new Date()) ? tr("今天 · ", "Today · ") : "") + WD[d.getDay()] + " " + (d.getMonth() + 1) + "/" + d.getDate();
  }
  // newer orders take ids newest first, as the server does: by the stamp,
  // then by the -N of a second take in the same second.
  function seq(id) {
    return id.length > 15 ? +id.slice(16) : 1;
  }
  function newer(a, b) {
    if (a.slice(0, 15) !== b.slice(0, 15)) return a.slice(0, 15) > b.slice(0, 15) ? -1 : 1;
    return seq(b) - seq(a);
  }

  var WORD = tr(
    { sent: "已发送", pasted: "已粘贴", cancelled: "已取消", empty: "无语音", undelivered: "未送达", dismissed: "已忽略", recording: "录音中", transcribing: "识别中", unknown: "早期录音" },
    { sent: "Sent", pasted: "Pasted", cancelled: "Cancelled", empty: "No speech", undelivered: "Not delivered", dismissed: "Dismissed", recording: "Recording", transcribing: "Transcribing", unknown: "Early take" },
  );
  var WHY = tr(
    { deliver_failed: "送达失败", asr_failed: "识别失败", interrupted: "被打断", delivery_cut: "粘贴时被打断", incomplete: "不完整" },
    { deliver_failed: "Delivery failed", asr_failed: "Transcription failed", interrupted: "Interrupted", delivery_cut: "Cut off while pasting", incomplete: "Incomplete" },
  );
  var MARK = { unknown: "legacy" }; // the state whose mark has another name
  var GLYPH = {
    sent: '<circle cx="5" cy="5" r="4" fill="currentColor"/>',
    pasted: '<circle cx="5" cy="5" r="3.5" fill="none" stroke="currentColor" stroke-width="1.4"/><path d="M5 1.5a3.5 3.5 0 0 1 0 7z" fill="currentColor"/>',
    cancelled: '<circle cx="5" cy="5" r="3.5" fill="none" stroke="currentColor" stroke-width="1.4"/>',
    undelivered: '<path d="M5 1l4.2 7.6H.8z" fill="currentColor"/>',
    dismissed: '<path d="M5 1.9l3.3 6H1.7z" fill="none" stroke="currentColor" stroke-width="1.3" stroke-linejoin="round"/>',
    recording: '<circle cx="5" cy="5" r="3.5" fill="none" stroke="currentColor" stroke-width="1.4" stroke-dasharray="2 1.6"/>',
    transcribing: '<circle cx="5" cy="5" r="3.5" fill="none" stroke="currentColor" stroke-width="1.4" stroke-dasharray="2 1.6"/>',
    empty: '<path d="M1.5 5h7" stroke="currentColor" stroke-width="1.8"/>',
    legacy: '<circle cx="5" cy="5" r="3.5" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-dasharray="0 2.2"/>',
  };
  function mk(state) {
    return MARK[state] || state;
  }
  // shown is the state a take shows and is counted under: 已忽略 for an
  // undelivered take the user dismissed, which keeps its state and why.
  function shown(t) {
    return t.dismissed ? "dismissed" : t.state;
  }
  function glyph(state) {
    var g = svg(10, 10, GLYPH[mk(state)] || GLYPH.cancelled);
    g.setAttribute("class", "glyph");
    return g;
  }
  // offMark is the offline notice's mark: a cross, not the 未送达 triangle.
  function offMark() {
    var g = svg(10, 10, '<path d="M2 2l6 6M8 2l-6 6" stroke="currentColor" stroke-width="1.6"/>');
    g.setAttribute("class", "glyph");
    return g;
  }
  function badge(state) {
    return h("span", { class: "state", "data-s": mk(state) }, glyph(state), WORD[state] || state);
  }
  var ICON = {
    search: '<circle cx="7" cy="7" r="5" fill="none" stroke="currentColor" stroke-width="1.6"/><path d="M11 11l3.5 3.5" stroke="currentColor" stroke-width="1.6"/>',
    back: '<path d="M10 3L5 8l5 5" fill="none" stroke="currentColor" stroke-width="1.6"/>',
    play: '<path d="M4 2.5v11l9-5.5z" fill="currentColor"/>',
    pause: '<path d="M4 2.5h3v11H4zM9 2.5h3v11H9z" fill="currentColor"/>',
    out: '<path d="M3 7L7 3M4 3h3v3" fill="none" stroke="currentColor" stroke-width="1.3"/>',
    check: '<path d="M1.5 5.5l2.5 2.5 4.5-6" fill="none" stroke="currentColor" stroke-width="1.6"/>',
    x: '<path d="M2 2l6 6M8 2l-6 6" stroke="currentColor" stroke-width="1.6"/>',
    theme: '<circle cx="8" cy="8" r="5.5" fill="none" stroke="currentColor" stroke-width="1.4"/><path d="M8 2.5a5.5 5.5 0 0 1 0 11z" fill="currentColor"/>',
  };
  function icon(name, size) {
    return svg(size || 16, size || 16, ICON[name]);
  }
  function brandMark() {
    // A waveform, its first half played: the page's scrub bar in 20 px.
    var hs = [4, 8, 12, 7, 10, 5, 9, 12, 6, 4];
    return svg(
      24,
      16,
      hs
        .map(function (v, i) {
          return '<rect x="' + i * 2.4 + '" y="' + (8 - v / 2) + '" width="1.5" height="' + v + '" rx=".7" fill="' + (i < 5 ? "var(--cobalt)" : "currentColor") + '"/>';
        })
        .join(""),
    );
  }

  // target is a take's target: its short name for the row and its full name.
  function target(t) {
    if (!t) return { short: "", full: "" };
    if (t.kind === "clipboard") return { short: tr("剪贴板", "Clipboard"), full: tr("剪贴板", "Clipboard") };
    if (t.pane) {
      var s = (t.workspace ? t.workspace + " › " : "") + t.pane;
      return { short: s, full: s + (t.title ? " · " + t.title : "") };
    }
    var app = t.app || t.bundle_id || "";
    return { short: app, full: app + (t.title ? " — " + t.title : "") };
  }

  // ------------------------------------------------------------ state

  var PAGE = 200; // rows per request: the first screen and each scroll
  // POLL is the time between looks for new takes and changed ones: just over
  // the index's second between reads of the directory, so every look reads it.
  var POLL = 1100;
  var params = new URLSearchParams(location.search);
  var S = {
    q: params.get("q") || "",
    f: params.get("state") || "",
    target: params.get("target") || "",
    device: params.get("device") || "",
    day: params.get("day") || "",
    sel: params.get("take") || "",
    auto: false, // the selection is the page's (the newest take on a wide window), not the user's
    keys: false,
  };
  // D is what the server last said: the rows loaded, newest first.
  var D = { rows: [], more: false, matched: 0, days: [], facets: null, loaded: false, skel: false, refused: false, lastOK: null, down: false };
  var gen = 0; // a response for an older query is dropped
  var WIDE = window.matchMedia("(min-width: 1081px)");

  function filtered() {
    return !!(S.f || S.target || S.device || S.day);
  }
  function query(extra) {
    var p = new URLSearchParams();
    if (S.q.trim()) p.set("q", S.q.trim());
    if (S.f) p.set("state", S.f);
    if (S.target) p.set("target", S.target);
    if (S.device) p.set("device", S.device);
    if (S.day) p.set("day", S.day);
    for (var k in extra) p.set(k, extra[k]);
    return "api/takes?" + p.toString();
  }
  function syncUrl() {
    var p = new URLSearchParams();
    if (S.q) p.set("q", S.q);
    if (S.f) p.set("state", S.f);
    if (S.target) p.set("target", S.target);
    if (S.device) p.set("device", S.device);
    if (S.day) p.set("day", S.day);
    if (S.sel && !S.auto) p.set("take", S.sel);
    var s = p.toString();
    history.replaceState(null, "", s ? "?" + s : location.pathname);
  }
  function total() {
    var n = 0;
    if (D.facets) for (var k in D.facets.states) n += D.facets.states[k];
    return n;
  }
  function failed(err) {
    if (err === Refused) {
      D.refused = true;
      render();
      return;
    }
    if (!D.down) {
      D.down = true;
      render();
    }
  }
  function ok(pg) {
    D.lastOK = new Date();
    D.refused = false;
    D.down = false;
    D.facets = pg.facets || D.facets;
    D.matched = pg.matched;
    D.days = pg.days;
  }

  // load reads the first page for the current query, in place of the list.
  // After a change of query or filter (refilter) a selected take the list
  // no longer shows is let go, from the detail and from the address.
  var loading = 0; // the gen of the load in flight
  function load(refilter) {
    var g = ++gen;
    loading = g;
    syncUrl();
    return api(query({ n: PAGE, facets: 1 })).then(function (pg) {
      if (g !== gen) return;
      loading = 0;
      ok(pg);
      D.rows = pg.takes;
      D.more = pg.more;
      if (refilter === true && S.sel && !D.rows.some(function (r) { return r.id === S.sel; })) {
        S.sel = "";
        S.auto = false;
        syncUrl();
      }
      if (!D.loaded) {
        D.loaded = true;
        if (D.skel && Date.now() - D.skelAt < 400) {
          setTimeout(render, 400 - (Date.now() - D.skelAt));
          return;
        }
      }
      render();
    }, function (err) {
      if (g === gen) loading = 0;
      failed(err);
    });
  }

  // loadMore reads the next PAGE rows older than the last one loaded.
  var loadingMore = false;
  function loadMore() {
    if (!D.more || loadingMore || !D.rows.length) return;
    loadingMore = true;
    var g = gen;
    api(query({ n: PAGE, before: D.rows[D.rows.length - 1].id }))
      .then(function (pg) {
        if (g !== gen) return;
        ok(pg);
        D.rows = D.rows.concat(pg.takes);
        D.more = pg.more;
        render();
      }, failed)
      .then(function () {
        loadingMore = false;
      });
  }

  // poll reads the newest rows of the current query and merges them: a new
  // take goes in at its place, a changed one is replaced, and one that no
  // longer matches (an undelivered take resent, under 只看未送达) leaves.
  // The list is drawn again only when something changed.
  var drawn = "";
  var polls = 0;
  function poll() {
    if (document.hidden || stopped) return;
    if (!D.loaded) {
      if (!loading) load(); // the first load got an error answer
      return;
    }
    var g = gen;
    api(query({ n: 50, facets: 1 })).then(function (pg) {
      if (g !== gen) return;
      var wasDown = D.down || D.refused;
      ok(pg);
      var got = {};
      pg.takes.forEach(function (r) {
        got[r.id] = r;
      });
      var edge = pg.more ? pg.takes[pg.takes.length - 1].id : null; // the poll saw every take newer than edge
      var rows = D.rows.filter(function (r) {
        return got[r.id] || (edge && newer(r.id, edge) > 0);
      });
      var have = {};
      rows = rows.map(function (r) {
        have[r.id] = 1;
        return got[r.id] || r;
      });
      var last = rows.length ? rows[rows.length - 1].id : null;
      pg.takes.forEach(function (r) {
        if (!have[r.id] && (!D.more || !last || newer(r.id, last) < 0)) rows.push(r);
      });
      rows.sort(function (a, b) {
        return newer(a.id, b.id);
      });
      D.rows = rows;
      var r = got[S.sel];
      if (r && DT.id === r.id && DT.take && rowSig(r) !== rowSig(DT.take)) reloadTake(r.id);
      if (wasDown || sig() !== drawn) render();
      else fresh();
    }, failed);
  }
  function rowSig(r) {
    return JSON.stringify([r.state, r.why, r.seen, r.dismissed, r.counted, r.dur_s, r.line, r.audio, !!r.labeled]);
  }
  function sig() {
    return JSON.stringify([D.rows, D.facets, D.days, D.matched, D.more]);
  }

  // ------------------------------------------------------------ top bar

  var qInput, freshEl, keysBtn;
  function topbar() {
    var timer = null;
    qInput = h("input", {
      id: "q",
      type: "search",
      placeholder: tr("搜索文字，中文英文都可以…", "Search text, Chinese or English…"),
      "aria-label": tr("搜索录音文字", "Search recordings"),
      autocomplete: "off",
      spellcheck: "false",
      oninput: function (e) {
        if (e.isComposing) return; // an input method is still composing
        changed();
      },
      oncompositionend: changed,
    });
    qInput.value = S.q;
    function changed() {
      clearTimeout(timer);
      timer = setTimeout(function () {
        if (S.q === qInput.value) return;
        S.q = qInput.value;
        load(true);
      }, 150);
    }
    freshEl = h("span", { class: "fresh", role: "status" });
    keysBtn = h("button", { class: "icon-btn", type: "button", "aria-label": tr("快捷键", "Keyboard shortcuts"), "aria-expanded": "false", onclick: toggleKeys, text: "?" });
    return h(
      "header",
      { class: "topbar" },
      h(
        "div",
        { class: "topbar-in" },
        h("a", { class: "brand", href: "./", translate: "no" }, brandMark(), h("span", null, tr("录音历史", "Recordings"))),
        h("div", { class: "search", role: "search" }, icon("search", 15), qInput, h("kbd", null, "/")),
        freshEl,
        h("div", { class: "tools" }, keysBtn, h("button", { class: "icon-btn", type: "button", "aria-label": tr("切换深浅色", "Toggle light or dark"), onclick: toggleTheme }, icon("theme"))),
      ),
    );
  }
  function fresh() {
    if (!freshEl) return;
    if (D.down && D.lastOK) {
      freshEl.textContent = tr(hm(D.lastOK) + " 之后没有更新", "No updates since " + hm(D.lastOK));
      freshEl.dataset.kind = "stale";
    } else {
      freshEl.textContent = D.lastOK ? tr("刚刚更新", "Updated just now") : "";
      delete freshEl.dataset.kind;
    }
  }
  function toggleTheme() {
    var cur = document.documentElement.dataset.theme || (window.matchMedia("(prefers-color-scheme: dark)").matches ? "dark" : "light");
    var next = cur === "dark" ? "light" : "dark";
    document.documentElement.dataset.theme = next;
    try {
      localStorage.setItem("takes.theme", next);
    } catch (e) {}
  }
  function toggleKeys() {
    S.keys = !S.keys;
    render();
  }
  var KEYS = [
    ["/", tr("搜索", "Search")],
    ["j / k", tr("下一条 / 上一条", "Next / previous take")],
    ["Enter", tr("打开这一条", "Open this take")],
    ["u", tr("只看未送达", "Show only not delivered")],
    ["c", tr("复制这一条的文字", "Copy this take's text")],
    ["r", tr("重发…", "Resend…")],
    ["i", tr("忽略这一条未送达 / 取消忽略", "Dismiss this undelivered take / undo")],
    ["t", tr("重新识别…", "Re-transcribe…")],
    ["1–9", tr("标第几版对（Shift 也标它）", "Mark version N correct (Shift adds it)")],
    ["e", tr("改待确认的正确文字", "Edit the pending correct text")],
    ["⌘↩", tr("确认为正确文字（在标注框里）", "Confirm as correct text (in the label box)")],
    [tr("空格", "Space"), tr("播放 / 暂停", "Play / pause")],
    ["← / →", tr("跳 1 秒（60 秒以上 5 秒），Shift 10 秒 / 60 秒", "Skip 1 s (5 s past 60 s), Shift 10 s / 60 s")],
    [", / .", tr("慢一档 / 快一档", "Slower / faster")],
    ["Esc", tr("清除搜索，收起，或回到列表", "Clear search, close, or back to the list")],
  ];
  function keysPanel() {
    return h(
      "div",
      { class: "keys", role: "dialog", "aria-label": tr("快捷键", "Keyboard shortcuts") },
      h("h2", null, tr("快捷键", "Keyboard shortcuts")),
      h(
        "dl",
        null,
        KEYS.map(function (r) {
          return [h("dt", null, r[0]), h("dd", null, r[1])];
        }),
      ),
    );
  }

  // ------------------------------------------------------------ list

  function setFilter(key, value) {
    S[key] = value;
    load(true);
  }

  function notice() {
    return h(
      "div",
      { class: "notice", "data-kind": "err", role: "status" },
      h("span", { class: "state" }, offMark()),
      h("b", null, tr("megavoice 未运行", "megavoice is not running")),
      h("span", null, D.lastOK ? tr("下面是 " + hm(D.lastOK) + " 的列表，它回来后会自动刷新", "The list below is from " + hm(D.lastOK) + "; it refreshes when megavoice is back") : tr("它回来后会自动刷新", "It refreshes when megavoice is back")),
    );
  }

  function listHead() {
    var out = [];
    if (D.down) out.push(notice());
    var st = D.facets ? D.facets.states : {};
    var u = st.undelivered || 0;
    if (u || S.f === "undelivered")
      out.push(
        h(
          "div",
          { class: "strip-row" },
          h(
            "button",
            { class: "notice strip", type: "button", "data-k": "strip", "aria-pressed": S.f === "undelivered" ? "true" : "false", onclick: toggleUndelivered },
            glyph("undelivered"),
            h("b", null, tr(u + " 条未送达", u + " not delivered")),
            h("span", { class: "go" }, S.f === "undelivered" ? tr("显示全部", "Show all") : tr("只看这些", "Show only these")),
          ),
          u ? h("button", { class: "btn", type: "button", "data-k": "dismiss-all", disabled: !!dismissing, onclick: dismissAll }, tr("全部忽略", "Dismiss all")) : null,
        ),
      );
    if (S.undo)
      out.push(
        h(
          "div",
          { class: "notice", "data-kind": "done", role: "status" },
          h("span", { class: "state", "data-s": "dismissed" }, glyph("dismissed")),
          h("b", null, tr("已忽略 " + S.undo.length + " 条", "Dismissed " + takesN(S.undo.length))),
          h("span", null, tr("录音和文字都还在，不再算作未送达", "Audio and text are kept; they no longer count as not delivered")),
          h("span", { class: "notice-acts" },
            h("button", { class: "btn", type: "button", "data-k": "undo-all", disabled: !!dismissing, onclick: undoAll }, tr("撤销", "Undo")),
            h("button", { class: "btn", type: "button", "data-k": "undo-close", onclick: function () { S.undo = null; render(); } }, tr("收起", "Close")),
          ),
        ),
      );
    var all = total();
    out.push(
      S.q.trim()
        ? h("h1", { class: "count-line" }, tr("找到 " + D.matched + " 条", "Found " + D.matched), h("span", { class: "of" }, tr("共 " + all, "of " + all)))
        : h("h1", { class: "count-line" }, tr(D.matched + " 条录音", takesN(D.matched)), D.matched !== all ? h("span", { class: "of" }, tr("共 " + all, "of " + all)) : null),
    );
    var pills = [["", tr("全部", "All"), all]].concat(
      ["undelivered", "sent", "pasted", "cancelled", "empty", "dismissed"]
        .filter(function (s) {
          return s !== "dismissed" || st.dismissed || S.f === s;
        })
        .map(function (s) {
          return [s, WORD[s], st[s] || 0];
        }),
    );
    function sel(label, key, values) {
      return h(
        "select",
        { "aria-label": tr("按" + label + "筛选", "Filter by " + label), "data-k": "sel:" + key, onchange: function (e) { setFilter(key, e.target.value); } },
        h("option", { value: "" }, tr("全部" + label, "All " + label + "s")),
        values.map(function (v) {
          return h("option", { value: v[0], selected: S[key] === v[0] }, v[1]);
        }),
      );
    }
    var f = D.facets || { targets: [], devices: [], days: [] };
    out.push(
      h(
        "div",
        { class: "filters" },
        h(
          "div",
          { class: "pills", role: "group", "aria-label": tr("按状态筛选", "Filter by status") },
          pills.map(function (p) {
            return h(
              "button",
              { class: "pill", type: "button", "data-k": "pill:" + p[0], "aria-pressed": S.f === p[0] ? "true" : "false", "data-zero": p[2] === 0 ? "" : null, onclick: function () { setFilter("f", p[0]); } },
              p[1],
              h("span", { class: "n" }, String(p[2])),
            );
          }),
        ),
        h(
          "div",
          { class: "selects" },
          sel(tr("目标", "target"), "target", f.targets.map(function (x) { return [x.key, x.label || x.key]; })),
          sel(tr("设备", "device"), "device", f.devices.map(function (x) { return [x.key, x.key]; })),
          sel(tr("日期", "date"), "day", f.days.map(function (x) { return [x.key, dayLabel(x.key)]; })),
        ),
      ),
    );
    return out;
  }
  function toggleUndelivered() {
    setFilter("f", S.f === "undelivered" ? "" : "undelivered");
  }

  // dismiss dismisses takes, like marking them read, or with undo takes the
  // dismiss back: body is {ids} or {all}. Nothing of a take is removed; it
  // leaves every 未送达 count and shows as 已忽略. It resolves to the ids
  // of the takes it changed, which the undo sends back; to null when the
  // request failed; to undefined, sending nothing, while one is out.
  var dismissing = false;
  function dismiss(body, undo) {
    if (dismissing) return Promise.resolve(undefined);
    dismissing = true;
    render();
    return postJSON("api/dismiss", Object.assign({ undo: !!undo }, body))
      .then(function (r) {
        return r.ids || [];
      }, function (err) {
        failedAction(err);
        return null;
      })
      .then(function (ids) {
        dismissing = false;
        load();
        if (DT.id && ids && ids.indexOf(DT.id) >= 0) reloadTake(DT.id);
        return ids;
      });
  }
  function dismissAll() {
    dismiss({ all: true }).then(function (ids) {
      if (ids && ids.length) S.undo = ids;
      render();
    });
  }
  function undoAll() {
    var ids = S.undo;
    dismiss({ ids: ids }, true).then(function (back) {
      if (back) S.undo = null;
      render();
    });
  }
  // dismissOne dismisses the take shown, or takes its dismiss back; the
  // outcome shows beside its button.
  function dismissOne(t) {
    var undo = !!t.dismissed;
    dismiss({ ids: [t.id] }, undo).then(function (ids) {
      if (ids === undefined || DT.id !== t.id) return;
      if (!ids) say("main", "err", undo ? tr("没能取消忽略", "Couldn't undo the dismiss") : tr("没能忽略", "Couldn't dismiss"));
      else if (ids.length) say("main", "ok", undo ? tr("已取消忽略 · 又算作未送达", "Undismissed · counts as not delivered again") : tr("已忽略 · 不再算作未送达", "Dismissed · no longer counts as not delivered"));
      drawDetail();
    });
  }

  // snippet is a search match: the text around the query, the query marked,
  // and a tag when the match is outside the delivered text.
  function snippet(m) {
    var src = m.source;
    var tag = null;
    if (src === "raw") tag = h("span", { class: "src" }, tr("原始", "Raw"));
    else if (/^retranscription \d+$/.test(src)) tag = h("span", { class: "src" }, tr("重新识别 #", "Re-transcription #") + src.split(" ")[1]);
    else if (/^deliver \d+$/.test(src)) tag = h("span", { class: "src" }, tr("重发 #", "Resend #") + src.split(" ")[1]);
    else if (src !== "delivered") tag = h("span", { class: "src mono" }, src);
    // the lead is clipped from its left and the rest from its right, so the
    // match itself always shows (styles.css .txt.hit)
    return [tag, h("span", { class: "pre" }, h("bdi", null, m.before)), h("mark", null, m.hit), h("span", { class: "post" }, m.after)];
  }

  function row(t) {
    var tg = target(t.target);
    var at = new Date(t.at);
    var txt;
    if (t.state === "recording") txt = h("span", { class: "txt muted" }, tr("结束后显示文字", "Text shows when it ends"));
    else if (t.state === "empty") txt = h("span", { class: "txt" }, tr("（无语音）", "(no speech)"));
    else txt = h("span", { class: t.match ? "txt hit" : "txt" }, t.why && WHY[t.why] ? h("span", { class: "why" }, WHY[t.why]) : null, t.match ? snippet(t.match) : t.line || (t.state === "transcribing" ? tr("正在识别…", "Transcribing…") : tr("（没有文字）", "(no text)")));
    var s = shown(t);
    var long = !t.match && t.dur_s > 60 && (t.state === "sent" || t.state === "pasted" || t.state === "undelivered");
    var playable = t.audio && t.state !== "recording";
    return h(
      "li",
      { class: "row", "data-s": s, "data-long": long ? "" : null, "data-play": playable ? "" : null, "aria-current": S.sel === t.id ? "true" : null },
      h(
        "a",
        {
          class: "t",
          href: "?take=" + encodeURIComponent(t.id),
          "data-id": t.id,
          "aria-label": hms(at) + " " + (WORD[s] || s) + " " + dur(t.dur_s),
          onclick: function (e) {
            if (e.metaKey || e.ctrlKey || e.shiftKey) return;
            e.preventDefault();
            select(t.id, true);
          },
        },
        hms(at),
      ),
      h("span", { class: "len" }, t.state === "recording" ? "" : dur(t.dur_s)),
      playable
        ? h(
            "button",
            {
              class: "row-play",
              type: "button",
              "aria-label": tr("播放 ", "Play ") + hms(at),
              "data-k": "rowplay:" + t.id,
              onclick: function () {
                if (P && P.id === t.id) return togglePlay();
                playWhenReady = t.id;
                select(t.id, false);
              },
            },
            icon("play", 12),
          )
        : null,
      t.state === "recording" ? h("span", { class: "state", "data-s": "recording" }, glyph("recording"), tr("录音中 ", "Recording ") + dur(t.dur_s)) : badge(s),
      h("span", { class: "tgt", title: tg.full || null }, tg.short),
      txt,
      t.labeled ? h("span", { class: "lb", role: "img", title: tr("已标注", "Labeled"), "aria-label": tr("已标注", "Labeled") }, svg(10, 10, ICON.check)) : h("span", { class: "lb" }),
    );
  }

  function list() {
    var col = h("section", { class: "list-col", id: "list", "aria-label": tr("录音列表", "Recordings"), tabindex: "-1" });
    add(col, listHead());
    if (!D.rows.length) {
      add(
        col,
        h(
          "div",
          { class: "empty" },
          h("h2", null, S.q.trim() ? tr("没有匹配「" + S.q.trim() + "」", "Nothing matches “" + S.q.trim() + "”") : tr("这个筛选下没有录音", "No takes match these filters")),
          h("button", { class: "link", type: "button", "data-k": "clear", onclick: clearFilters }, tr("清除筛选", "Clear filters")),
        ),
      );
      return col;
    }
    var days = {};
    D.days.forEach(function (d) {
      days[d.key] = d;
    });
    var groups = [];
    D.rows.forEach(function (t) {
      var g = groups[groups.length - 1];
      if (!g || g.k !== t.day) groups.push((g = { k: t.day, rows: [] }));
      g.rows.push(t);
    });
    groups.forEach(function (g) {
      var d = days[g.k] || { n: g.rows.length, undelivered: 0 };
      add(
        col,
        h(
          "section",
          { class: "day", "aria-label": dayLabel(g.k) },
          h("div", { class: "day-head" }, h("h2", null, dayLabel(g.k)), h("span", { class: "day-meta" }, tr(d.n + " 条", takesN(d.n)), d.undelivered ? [" · ", h("span", { class: "warn" }, tr(d.undelivered + " 未送达", d.undelivered + " not delivered"))] : null)),
          h("ul", { class: "rows" }, g.rows.map(row)),
        ),
      );
    });
    add(col, h("p", { class: "more", id: "more" }, D.more ? tr("已显示 " + D.rows.length + " 条 · 更早的录音随滚动载入", "Showing " + takesN(D.rows.length) + " · older ones load as you scroll") : tr("已显示全部 " + D.rows.length + " 条", D.rows.length === 1 ? "Showing the only take" : "Showing all " + D.rows.length + " takes")));
    return col;
  }
  function clearFilters() {
    S.q = "";
    qInput.value = "";
    S.f = S.target = S.device = S.day = "";
    load(true);
  }

  // ------------------------------------------------------------ detail

  // The detail column is built once and kept across the list's redraws, so
  // its scroll, the player and an open picker survive a poll. #detail holds
  // the selected take, drawn by drawDetail from DT.
  var detailCol, detailEl;
  function detail() {
    if (!detailCol) {
      detailEl = h("div", { id: "detail" });
      detailCol = h(
        "section",
        { class: "detail-col", "aria-label": tr("这一条录音", "This take") },
        h("nav", { class: "back" }, h("a", { href: "./", onclick: function (e) { e.preventDefault(); select(""); } }, icon("back", 14), tr("录音列表", "Recordings"))),
        detailEl,
      );
    }
    return detailCol;
  }

  // select shows the take id in the detail; opened says the user chose it
  // (a click, j/k, Enter, the address), which marks an undelivered take seen.
  function select(id, focusDetail, auto) {
    S.sel = id;
    S.auto = !!auto;
    if (id && !auto) opened[id] = true;
    syncUrl();
    render();
    if (id && id === DT.id) seen(id);
    if (focusDetail && !WIDE.matches) window.scrollTo(0, 0);
  }
  var opened = {};
  if (S.sel) opened[S.sel] = true;

  // DT is the take the detail shows and what is open in it; busy holds the
  // actions in flight per take, so a take left and come back to still shows
  // its button waiting, and a second click sends nothing.
  var DT = { id: "", take: null, err: null, note: null, picker: null, targets: null, dest: "", enter: false, engine: "" };
  var busy = { resend: {}, retr: {}, label: {} };
  var ENG = null; // the engines, re-read when the engine picker opens
  var playWhenReady = "";

  // openTake reads the take S.sel names when it is not the one shown, and
  // marks it seen when the user opened it and it counts as undelivered.
  function openTake() {
    if (S.sel === DT.id) return;
    stopPlayer();
    DT = { id: S.sel, take: null, err: null, note: null, picker: null, targets: null, dest: "", enter: false, engine: "" };
    drawDetail();
    if (!S.sel) return;
    var id = S.sel;
    reloadTake(id).then(function () {
      seen(id);
    });
    if (!ENG) api("api/engines").then(function (r) { ENG = r.engines || []; if (DT.take) drawDetail(); }, function () {});
  }

  // seen marks the take shown seen when the user opened it and it counts as
  // undelivered: the menu bar's count goes down by one.
  var seeing = {};
  function seen(id) {
    var t = DT.id === id && DT.take;
    if (!t || !t.counted || !opened[id] || seeing[id]) return;
    seeing[id] = true;
    postJSON("api/seen", { id: id })
      .then(function (row) {
        patchRow(row);
        reloadTake(id);
      }, failedAction)
      .then(function () {
        seeing[id] = false;
      });
  }

  // reloadTake reads the take again and draws it when anything changed.
  function reloadTake(id) {
    return api("api/take/" + encodeURIComponent(id)).then(
      function (t) {
        if (DT.id !== id) return;
        var was = DT.take;
        DT.take = t;
        DT.err = null;
        if (!was || JSON.stringify(was) !== JSON.stringify(t)) {
          if (!was || was.audio !== t.audio || was.track !== t.track || (was.state === "recording") !== (t.state === "recording")) loadPeaks(t);
          drawDetail();
        }
      },
      function (err) {
        if (err === Refused) return failed(err);
        if (DT.id !== id) return;
        DT.err = err.status === 404 ? tr("这一条录音已经不在存储里了。", "This take is no longer in the store.") : tr("读不到这一条：", "Couldn't read this take: ") + err.message;
        drawDetail();
      },
    );
  }

  // patchRow puts a row the server answered in place in the list.
  function patchRow(row) {
    if (!row || !row.id) return;
    for (var i = 0; i < D.rows.length; i++)
      if (D.rows[i].id === row.id) {
        D.rows[i] = Object.assign({}, D.rows[i], row, { match: D.rows[i].match });
        render();
        return;
      }
  }

  function failedAction(err) {
    if (err === Refused) failed(err);
  }

  // mainText is the text the detail leads with and what a resend of it
  // sends: the delivered text, else the raw one.
  function mainText(t) {
    if (t.text) return { src: "delivered", text: t.text };
    if (t.raw) return { src: "raw", text: t.raw };
    return null;
  }

  function note(where) {
    var n = DT.note;
    if (!n || n.where !== where) return null;
    return h("span", { class: "act-note", "data-kind": n.kind, role: "status" }, n.kind === "ok" ? [svg(10, 10, ICON.check), " "] : null, n.text);
  }
  function say(where, kind, text) {
    DT.note = { where: where, kind: kind, text: text };
  }

  function copyText(text, where) {
    return navigator.clipboard.writeText(text).then(
      function () {
        say(where, "ok", tr("已复制 " + Array.from(text).length + " 字", "Copied " + charsN(Array.from(text).length)));
        drawDetail();
      },
      function (err) {
        say(where, "err", tr("没能复制：", "Couldn't copy: ") + err.message);
        drawDetail();
      },
    );
  }

  function dheader(t) {
    var d = new Date(t.at);
    var tg = target(t.target);
    var eng = "";
    t.events.forEach(function (e) {
      if (e.ev === "text" && e.engine) eng = e.engine;
      else if (e.ev === "start" && e.engine && !eng) eng = e.engine;
    });
    return h(
      "header",
      { class: "d-head" },
      h("h1", null, h("span", null, tr(d.getMonth() + 1 + "月" + d.getDate() + "日 " + WD[d.getDay()] + " ", WD[d.getDay()] + ", " + MON[d.getMonth()] + " " + d.getDate() + " ")), h("span", { class: "mono" }, hms(d)), badge(shown(t)), t.why && WHY[t.why] ? h("span", { class: "d-why" }, WHY[t.why]) : null),
      h(
        "div",
        { class: "d-meta" },
        h("span", null, tr("时长 ", "Length "), h("b", { class: "num" }, dur(t.dur_s))),
        tg.short ? h("span", null, tr("原目标 ", "Target "), h("b", { title: tg.full }, tg.short)) : null,
        t.device ? h("span", null, tr("设备 ", "Device "), h("b", null, t.device)) : null,
        eng ? h("span", null, tr("引擎 ", "Engine "), h("b", { class: "mono" }, eng)) : null,
        t.track === "backup" ? h("span", null, tr("声音 ", "Audio "), h("b", null, tr("备用输入", "backup input")), tr("（主输入没有声音，播放的是备用输入）", " (the main input was silent; this plays the backup)")) : null,
      ),
    );
  }

  // ------------------------------------------------------------ texts: versions and the label

  // Tokens: each Han, kana or hangul character, and each run of other
  // letters and digits (a word, a number); spaces and punctuation are kept
  // for display but never compared: the rule labels.go's same() matches a
  // typed text to an engine with.
  var CJK = "\\p{Script=Han}\\p{Script=Hiragana}\\p{Script=Katakana}\\p{Script=Hangul}";
  var TOK = new RegExp("([" + CJK + "])|([^\\s\\p{P}\\p{S}" + CJK + "]+)|([\\s\\p{P}\\p{S}]+)", "gu");
  function tokens(s) {
    var out = [];
    String(s).replace(TOK, function (m, han, word, sep) {
      out.push({ t: m, key: sep ? null : m.toLowerCase().replace(/’/g, "'") });
      return m;
    });
    return out;
  }
  // lcsMap pairs the words of a with the words of b along their longest
  // common subsequence: token index in a → token index in b. Null for a pair
  // too long to compare.
  function lcsMap(a, b) {
    var ai = [], bi = [], map = {}, i, j;
    a.forEach(function (x, k) { if (x.key) ai.push(k); });
    b.forEach(function (x, k) { if (x.key) bi.push(k); });
    var n = ai.length, m = bi.length;
    if (n * m > 4e6) return null;
    var L = [];
    for (i = 0; i <= n; i++) L.push(new Uint16Array(m + 1));
    for (i = n - 1; i >= 0; i--) for (j = m - 1; j >= 0; j--) L[i][j] = a[ai[i]].key === b[bi[j]].key ? L[i + 1][j + 1] + 1 : Math.max(L[i + 1][j], L[i][j + 1]);
    for (i = 0, j = 0; i < n && j < m; ) {
      if (a[ai[i]].key === b[bi[j]].key) {
        map[ai[i]] = bi[j];
        i++;
        j++;
      } else if (L[i + 1][j] >= L[i][j + 1]) i++;
      else j++;
    }
    return map;
  }
  // diff marks off the words of a and b that are not on their longest
  // common subsequence. A pair too long to compare stays unmarked.
  function diff(a, b) {
    var m = lcsMap(a, b), hit = {};
    if (m) for (var k in m) hit[m[k]] = true;
    a.forEach(function (x, i) { x.off = !!(m && x.key && !(i in m)); });
    b.forEach(function (x, i) { x.off = !!(m && x.key && !hit[i]); });
  }
  // marked is a text with its off words lit: a run of them, with the spaces
  // and punctuation between, is one mark. With times (timesOf), each word
  // carries its time in the take, for a click to play from.
  function marked(toks, times) {
    var on = toks.map(function (x) { return !!(x.key && x.off); });
    for (var i = 0; i < toks.length; i++) {
      if (toks[i].key) continue;
      var p = i - 1, q = i + 1;
      while (p >= 0 && !toks[p].key) p--;
      while (q < toks.length && !toks[q].key) q++;
      on[i] = p >= 0 && q < toks.length && !!toks[p].off && !!toks[q].off;
    }
    var out = [], run = [], cur = false;
    function flush() {
      if (run.length) out.push(cur ? h("mark", { class: "dif" }, run) : run);
      run = [];
    }
    toks.forEach(function (x, k) {
      if (on[k] !== cur) {
        flush();
        cur = on[k];
      }
      var tm = times && x.key && times[k];
      run.push(tm ? h("span", { class: "w", "data-t": tm.s.toFixed(2), "data-e": tm.e.toFixed(2) }, x.t) : x.t);
    });
    flush();
    return out;
  }

  // timedWords is the take's words with their times, from the version whose
  // engine gave word timing: an answer's words, [{text, start_ms, end_ms}],
  // the shape the build adds from doubao's utterances. A word of several
  // Han characters shares its time between them. Null without timing.
  function timedWords(t) {
    var a = t.answers.filter(function (x) { return x.words && x.words.length; })[0];
    if (!a) return null;
    var out = [];
    a.words.forEach(function (w) {
      var tk = tokens(w.text).filter(function (x) { return x.key; });
      var d = (w.end_ms - w.start_ms) / Math.max(1, tk.length);
      tk.forEach(function (x, i) { out.push({ t: x.t, key: x.key, s: (w.start_ms + i * d) / 1000, e: (w.start_ms + (i + 1) * d) / 1000 }); });
    });
    return out;
  }
  // timesOf gives each word of toks its time in the take, {s, e} in
  // seconds: a word the timed engine heard too takes its time; one it did
  // not, the gap between its timed neighbours. Null without timing.
  function timesOf(t, toks) {
    var tw = timedWords(t);
    if (!tw) return null;
    var m = lcsMap(toks, tw) || {};
    var out = toks.map(function (x, i) { return i in m ? { s: tw[m[i]].s, e: tw[m[i]].e } : null; });
    var last = 0;
    for (var i = 0; i < toks.length; i++) {
      if (!toks[i].key) continue;
      if (out[i]) {
        last = out[i].e;
        continue;
      }
      var j = i + 1;
      while (j < toks.length && !(toks[j].key && out[j])) j++;
      out[i] = { s: last, e: j < toks.length ? out[j].s : t.dur_s };
    }
    return out;
  }
  // wordClick plays the take from the word clicked; a drag that selects
  // text plays nothing.
  function wordClick(e) {
    if (!window.getSelection().isCollapsed) return;
    var w = e.target.closest && e.target.closest("[data-t]");
    if (!w) return;
    var was = detailEl.querySelector(".w[data-at]");
    if (was) was.removeAttribute("data-at");
    w.setAttribute("data-at", ""); // the word playing, lit until another is clicked or the take redraws
    playSpan(+w.dataset.t, +w.dataset.e, false);
  }

  function isCloud(name) {
    var e = engineOf(name);
    return !!(e && !e.local);
  }

  // versionsOf is every text of the take, in the order the detail stacks
  // them: the take's own text (the adopted engine's), each other engine of
  // the compare record by name, each re-transcription, the raw text. mark is
  // the name a label posts for it: the engine's, or "delivered" for the text
  // of a take without a compare record; "" where a label cannot name it.
  // primary is the compare record's: the asr.engine the take was recorded
  // with.
  function versionsOf(t) {
    var m = mainText(t);
    var by = "", evEng = "";
    var cmp = t.answers.filter(function (a) { return a.source === "compare"; });
    var prim = cmp.filter(function (a) { return a.primary; }).map(function (a) { return a.engine; })[0] || "";
    cmp.forEach(function (a) { if (a.delivered) by = a.engine; });
    t.events.forEach(function (e) { if (e.ev === "text" && e.engine) evEng = e.engine; });
    var out = [];
    if (m) {
      var a0 = cmp.filter(function (a) { return a.engine === by; })[0];
      var name = by || evEng;
      out.push({
        id: "main", main: true, name: name, label: m.src === "raw" ? tr("原始", "Raw") : "", text: m.text, src: m.src,
        mark: m.src !== "delivered" ? "" : !cmp.length ? "delivered" : a0 && !a0.err && a0.text ? by : "",
        adopted: m.src === "delivered", primary: !!prim && prim === name, lat: a0 ? a0.latency_ms : 0, cloud: isCloud(name),
      });
    }
    cmp.forEach(function (a) {
      if (m && a.engine === by) return;
      out.push({ id: "c:" + a.engine, name: a.engine, text: a.text, err: a.err, src: a.engine, mark: !a.err && a.text ? a.engine : "", primary: !!a.primary, lat: a.latency_ms, cloud: isCloud(a.engine), by: by });
    });
    t.answers.forEach(function (a) {
      if (a.source === "retranscription") out.push({ id: "r" + a.n, label: tr("重新识别 #", "Re-transcription #") + a.n, name: a.engine, text: a.text, err: a.err, src: "retranscription " + a.n, lat: a.latency_ms, cloud: isCloud(a.engine), retr: true });
    });
    if (t.raw && m && m.src !== "raw") out.push({ id: "raw", label: tr("原始", "Raw"), text: t.raw, src: "raw", aside: true });
    return out;
  }
  function markNames(t) {
    if (t.track === "backup") return []; // its WAV lacks what the backup heard: the label file takes a note alone
    return versionsOf(t)
      .filter(function (v) { return v.mark; })
      .map(function (v) { return v.mark; });
  }

  // texts is the take's texts, stacked: every version a row with its engine,
  // its words that differ from the take's own text lit, 复制 and 重发…, and
  // 这版对 where a label can name it. The take's own text lights the words
  // any other engine of the compare record heard differently.
  function texts(t) {
    var open = DT.picker && DT.picker.kind === "retr";
    var inFlight = busy.retr[t.id];
    var sec = h("section", { class: "block texts", "aria-labelledby": "h-txt" }, h("h2", { id: "h-txt", class: "sr-only" }, tr("文字", "Text")));
    // 重新识别… under the stack: its answer joins the stack as a new row
    var foot =
      t.audio && t.state !== "recording"
        ? [
            h(
              "div",
              { class: "actions ver-foot" },
              h(
                "button",
                { class: "btn", type: "button", "data-k": "retr", disabled: !!inFlight, "aria-expanded": open ? "true" : "false", onclick: toggleEngines },
                inFlight ? tr("正在用 " + inFlight + " 识别…", "Transcribing with " + inFlight + "…") : t.track === "backup" ? tr("重新识别备用输入…", "Re-transcribe the backup input…") : tr("重新识别…", "Re-transcribe…"),
              ),
              note("retr"),
            ),
            open ? enginePicker(t) : null,
          ]
        : null;
    var m = mainText(t);
    if (!m) add(sec, h("p", { class: "d-text none" }, t.state === "recording" ? tr("录音中，结束后在这里显示文字。", "Recording. The text shows here when it ends.") : t.state === "transcribing" ? tr("正在识别，完成后在这里显示文字。", "Transcribing. The text shows here when it's done.") : tr("（无语音）", "(no speech)")));
    if (t.state === "recording") return sec;
    var vs = versionsOf(t);
    var base = m ? tokens(m.text) : null;
    var any = base ? base.map(function () { return false; }) : null;
    vs.forEach(function (v) {
      if (v.main || v.err || !v.text) return;
      v.toks = tokens(v.text);
      if (!base) return;
      var b = v.retr || v.aside ? tokens(m.text) : base; // a re-transcription or the raw text lights only its own words
      diff(b, v.toks);
      if (b === base) base.forEach(function (x, i) { if (x.off) any[i] = true; });
    });
    if (base) base.forEach(function (x, i) { x.off = any[i]; });
    vs.forEach(function (v) { if (v.main) v.toks = base; });
    var lab = t.label;
    var n = 0;
    if (t.track === "backup") vs.forEach(function (v) { v.mark = ""; });
    add(
      sec,
      h(
        "div",
        { class: "vers" },
        vs.map(function (v) {
          return version(t, v, lab, v.mark ? ++n : 0);
        }),
      ),
      foot,
    );
    return sec;
  }

  function version(t, v, lab, key) {
    var on = !!(v.mark && lab && (lab.correct || []).indexOf(v.mark) >= 0);
    var head = h(
      "div",
      { class: "ver-head" },
      v.label ? h("span", { class: "who" }, v.label) : null,
      v.name ? h("span", { class: "who mono", translate: "no" }, v.name) : null,
      v.adopted ? h("span", { class: "tag", title: tr("这一条的文字用的是这一版", "This take's text is this version") }, tr("采用", "Used")) : null,
      v.primary ? h("span", { class: "tag", title: tr("录这一条时的 asr.engine", "The asr.engine this take was recorded with") }, tr("主引擎", "Primary")) : null,
      v.cloud ? h("span", { class: "cloud" }, svg(10, 10, ICON.out), tr("云端", "Cloud")) : null,
      v.lat ? h("span", { class: "lat" }, tr("用时 ", "Took "), h("span", { class: "num" }, (v.lat / 1000).toFixed(2)), tr(" 秒", " s")) : null,
      h("span", { class: "grow" }),
      v.mark
        ? h(
            "button",
            {
              class: "pill mark",
              type: "button",
              "data-k": "mark:" + v.mark,
              "aria-pressed": on ? "true" : "false",
              "aria-keyshortcuts": key && key < 10 ? String(key) : null,
              disabled: !!busy.label[t.id],
              title: on ? tr("已标这一版对 · 再点清除标注 · ⇧ 点只去掉这一版", "Marked correct · click again to clear the label · ⇧-click to unmark only this one") : tr("这一版对：只标它，马上存 · ⇧ 点也标它", "This version is correct: marks it alone, saved at once · ⇧-click to add it"),
              onclick: function (e) {
                markClick(t, v.mark, e.shiftKey || e.metaKey || e.ctrlKey);
              },
            },
            on ? svg(10, 10, ICON.check) : null,
            tr("这版对", "Correct"),
          )
        : null,
    );
    var body;
    if (v.err)
      body = h(
        "div",
        { class: "alert", role: "alert" },
        h("h3", null, svg(10, 10, ICON.x), tr("识别失败 · ", "Transcription failed · "), h("span", { class: "mono" }, v.name)),
        h("p", null, h("code", null, v.err)),
        h("p", { class: "muted" }, v.retr ? tr("存下的文字都没有改动。换一个引擎再试。", "No saved text changed. Try another engine.") : v.primary && v.by ? tr("主引擎没有给出文字，这一条用了 " + v.by + " 的。", "The primary engine gave no text; this take uses " + v.by + "'s.") : tr("这个引擎这次没有给出文字，不能标它对。", "This engine gave no text this time, so it can't be marked correct.")),
      );
    else if (!v.text) body = h("p", { class: "d-text none" }, tr("（这个版本没有文字）", "(this version has no text)"));
    else {
      var times = v.toks ? timesOf(t, v.toks) : null;
      body = h("p", { class: "d-text", lang: "zh-CN", "data-timed": times ? "" : null, onclick: times ? wordClick : null }, v.toks ? marked(v.toks, times) : v.text);
    }
    var row = h("div", { class: "ver", "data-v": v.id, "data-aside": v.aside ? "" : null }, head, body);
    if (v.err || !v.text) return row;
    if (v.main && v.src === "raw") add(row, h("p", { class: "muted" }, tr("没有整理过的文字，这是原始识别结果。", "No cleaned-up text; this is the raw transcription.")));
    var popen = DT.picker && DT.picker.kind === "resend" && DT.picker.where === v.id;
    // one primary per view: 重发… on a 未送达 take, 复制 otherwise, while no
    // picker is open and no ground truth is being composed
    var lead = v.main && !DT.picker && !gtBusy(t.id);
    var undel = t.state === "undelivered" && !t.dismissed;
    add(
      row,
      h(
        "div",
        { class: "actions" },
        h("button", { class: "btn" + (lead && !undel ? " primary" : ""), type: "button", "data-k": v.main ? "copy" : "copy:" + v.id, onclick: function () { copyText(v.text, v.id); } }, tr("复制", "Copy")),
        h("button", { class: "btn" + (lead && undel ? " primary" : ""), type: "button", "data-k": v.main ? "resend" : "resend:" + v.id, "aria-expanded": popen ? "true" : "false", onclick: function () { togglePicker(v.id, v); } }, tr("重发…", "Resend…")),
        v.main && t.state === "undelivered" ? h("button", { class: "btn", type: "button", "data-k": "dismiss", disabled: !!dismissing, onclick: function () { dismissOne(t); } }, t.dismissed ? tr("取消忽略", "Undismiss") : tr("忽略", "Dismiss")) : null,
        note(v.id),
      ),
      popen ? resendPicker(t) : null,
    );
    return row;
  }

  function labelWhen(at) {
    var d = new Date(at);
    return tr(d.getMonth() + 1 + "月" + d.getDate() + "日 " + hm(d), MON[d.getMonth()] + " " + d.getDate() + " " + hm(d));
  }
  function labelWords(lab) {
    var c = lab.correct || [];
    var who = c.map(function (n) { return n === "delivered" ? tr("送达的文字", "the delivered text") : n; }).join(tr("、", ", "));
    if (lab.label_source === "click") return who + (c.length > 1 ? tr(" 都对", " are correct") : tr(" 这版对", " is correct"));
    if (lab.ref) return tr("确认过的正确文字", "Confirmed correct text") + (c.length ? tr("，和 " + who + " 一样", ", same as " + who) : "");
    return tr("只有备注", "Note only");
  }

  // ------------------------------------------------------------ ground truth

  // A label is the take's ground truth. 这版对 says a version is exactly
  // right; when none is, the pending ground truth is composed in the 标注
  // section: a base version, a reading chosen at each place the versions
  // differ, free edits on top, then 确认为正确文字 posts the text as a typed
  // ref, and the server fills correct with every engine it matches.
  //
  // GT is the draft per take: base (a version id), parts (the text in order:
  // {s}, a stretch every version agrees on or one written by hand, or
  // {s, opts, pick, a, b}, a place the versions differ, pick an index into
  // opts or "hand", a and b the base's words around it for its time), tb (the
  // base's word times), note, dirty, open, err, prev (the draft a base
  // switch replaced, for 撤销). It stays while the user moves to another take
  // and back; 收起 closes it, and saving drops it.
  var GT = {};
  function gtBusy(id) {
    return !!(GT[id] && GT[id].open && GT[id].dirty);
  }

  // gtVersions is what a ground truth is composed from: every version with
  // text, and on a labelled take the label's own text first, as 当前.
  function gtVersions(t) {
    var vs = versionsOf(t).filter(function (v) { return v.text && !v.err; });
    var lab = t.label;
    if (lab && lab.ref) vs.unshift({ id: "label", label: tr("当前", "Current"), text: lab.ref });
    return vs;
  }
  function vname(v) {
    return v.label && v.name ? v.label + " " + v.name : v.label || v.name || "";
  }
  function wordKeys(s) {
    return tokens(s)
      .filter(function (x) { return x.key; })
      .map(function (x) { return x.key; })
      .join("\u0001");
  }
  function gtText(d) {
    return d.parts.map(function (x) { return x.s; }).join("");
  }

  // compose is a fresh draft from the version baseId: the base's words that
  // every other version also has are anchors; between two anchors, a stretch
  // where every version has the same words stays the base's, and one where
  // they differ is a place, with one reading per distinct wording (the
  // versions that share it named together), the base's picked.
  function compose(t, baseId) {
    var vs = gtVersions(t);
    var base = vs.filter(function (v) { return v.id === baseId; })[0] || vs[0];
    var d = { base: base ? base.id : "", parts: [], tb: null, note: "", dirty: false, open: true, err: "" };
    if (!base) return d;
    var others = vs.filter(function (v) { return v !== base; });
    var B = tokens(base.text);
    var O = others.map(function (v) {
      var T = tokens(v.text);
      return { v: v, T: T, m: lcsMap(B, T) || {} };
    });
    var anchors = [];
    B.forEach(function (x, i) {
      if (x.key && O.every(function (o) { return i in o.m; })) anchors.push(i);
    });
    function cut(T, from, to) {
      return T.slice(from, to).map(function (x) { return x.t; }).join("");
    }
    var parts = [], pb = -1, po = O.map(function () { return -1; });
    anchors.concat([B.length]).forEach(function (a) {
      var bs = cut(B, pb + 1, a);
      var os = O.map(function (o, j) {
        var end = a === B.length ? o.T.length : o.m[a];
        var seg = cut(o.T, po[j] + 1, end);
        po[j] = end;
        return seg;
      });
      var k = wordKeys(bs);
      if (os.every(function (x) { return wordKeys(x) === k; })) {
        if (bs) parts.push({ s: bs });
      } else {
        var opts = [{ text: bs, names: [vname(base)] }];
        os.forEach(function (x, j) {
          var same = opts.filter(function (o) { return wordKeys(o.text) === wordKeys(x); })[0];
          if (same) same.names.push(vname(O[j].v));
          else opts.push({ text: x, names: [vname(O[j].v)] });
        });
        parts.push({ s: bs, opts: opts, pick: 0, a: pb, b: a < B.length ? a : -1 });
      }
      if (a < B.length) parts.push({ s: B[a].t });
      pb = a;
    });
    // neighbouring stretches are one
    parts.forEach(function (x) {
      var last = d.parts[d.parts.length - 1];
      if (last && !last.opts && !x.opts) last.s += x.s;
      else d.parts.push(x);
    });
    d.tb = timesOf(t, B);
    return d;
  }

  // gtEdit takes a free edit of the draft's text. An edit inside one place
  // makes it hand-written (its readings still replace it); inside one
  // stretch it is just text; an edit across a boundary merges what it
  // touches into one hand-written stretch, and the places in it leave the
  // list. An insertion at the edge of a place belongs to the place.
  function gtEdit(d, next) {
    var old = gtText(d);
    if (old === next) return;
    d.dirty = true;
    d.prev = null;
    if (!d.parts.length) {
      d.parts = [{ s: next }];
      return;
    }
    var p = 0;
    while (p < old.length && p < next.length && old[p] === next[p]) p++;
    var q = 0;
    while (q < old.length - p && q < next.length - p && old[old.length - 1 - q] === next[next.length - 1 - q]) q++;
    var a = p, b = old.length - q, ins = next.slice(p, next.length - q);
    var off = 0, hit = [];
    d.parts.forEach(function (x, i) {
      x.from = off;
      off += x.s.length;
      x.to = off;
      if (a < b ? x.from < b && x.to > a : x.from <= a && a <= x.to) hit.push(i);
    });
    if (a === b && hit.length > 1) {
      var place = hit.filter(function (i) { return d.parts[i].opts; })[0];
      hit = [place != null ? place : hit[0]];
    }
    var i0 = hit[0], i1 = hit[hit.length - 1];
    var first = d.parts[i0], last = d.parts[i1];
    var text = old.slice(first.from, a) + ins + old.slice(b, last.to);
    var one = i0 === i1 ? Object.assign({}, first, { s: text }) : { s: text };
    if (one.opts) one.pick = "hand";
    d.parts.splice(i0, i1 - i0 + 1, one);
  }
  function gtPick(d, i, k) {
    d.parts[i].s = d.parts[i].opts[k].text;
    d.parts[i].pick = k;
    d.dirty = true;
    d.prev = null;
  }
  function gtRange(d, i) {
    var from = 0;
    for (var k = 0; k < i; k++) from += d.parts[k].s.length;
    return [from, from + d.parts[i].s.length];
  }
  // gtWhen is a place's time in the take: from the end of the word before
  // it to the start of the word after it. Null without timing.
  function gtWhen(t, d, x) {
    if (!d.tb) return null;
    return { s: x.a >= 0 && d.tb[x.a] ? d.tb[x.a].e : 0, e: x.b >= 0 && d.tb[x.b] ? d.tb[x.b].s : t.dur_s };
  }

  // labelBlock is the take's label: what it says, and the ground truth
  // being composed. An unlabelled take opens its pending ground truth at
  // once; a labelled one shows its label and opens the draft on 改正确文字….
  // A take without a WAV cannot be labelled.
  function labelBlock(t) {
    if (!t.audio || t.state === "recording" || t.state === "transcribing") return null;
    var lab = t.label;
    var d = GT[t.id];
    var noteOnly = t.track === "backup";
    if (!lab && !d) d = GT[t.id] = noteOnly ? { base: "", parts: [], tb: null, note: "", dirty: false, open: true, err: "", noteOnly: true } : compose(t, "main");
    else if (d && d.open && !d.dirty && !d.noteOnly) GT[t.id] = d = Object.assign(compose(t, d.base), { note: d.note, prev: d.prev }); // versions may have come in since
    var open = !!(d && d.open);
    var marks = markNames(t);
    var state;
    if (!lab && noteOnly) state = [h("span", { class: "lab-word" }, tr("未标注", "Not labeled")), h("span", { class: "muted" }, tr("这一条的文字来自备用输入，录音文件里没有这段声音，文字和它对不上：只能写备注", "This take's text came from the backup input, which the audio file doesn't hold, so the text can't match it: a note only"))];
    else if (!lab) state = [h("span", { class: "lab-word" }, tr("未标注", "Not labeled")), h("span", { class: "muted" }, marks.length ? tr("哪一版全对，点它的「这版对」；都不全对，就在下面拼出正确的文字，再确认", "If one version is fully right, click its “Correct”; if none is, build the correct text below and confirm") : tr("写下正确的文字，或者只写备注，再确认", "Write the correct text, or just a note, then confirm"))];
    else state = [h("span", { class: "lab-word", "data-on": "" }, svg(10, 10, ICON.check), tr("已标注", "Labeled")), h("span", null, labelWords(lab)), h("span", { class: "muted num" }, labelWhen(lab.labeled_at))];
    var keys = noteOnly ? tr("e 写备注 · ⌘↩ 存", "e note · ⌘↩ save") : (marks.length ? (marks.length > 1 ? "1–" + Math.min(9, marks.length) : "1") + tr(" 这版对 · ", " correct · ") : "") + tr("e 改文字 · ⌘↩ 确认", "e edit · ⌘↩ confirm");
    var sec = h(
      "section",
      { class: "block lab", "aria-labelledby": "h-lab" },
      h("div", { class: "tab-head" }, h("h2", { id: "h-lab" }, tr("标注", "Label")), h("span", { class: "up-keys" }, keys)),
      h("p", { class: "lab-state", role: "status" }, state),
    );
    if (open) {
      add(sec, gtForm(t, d, lab));
      return sec;
    }
    if (lab && lab.label_source === "typed" && lab.ref) {
      var m = mainText(t);
      var rt = tokens(lab.ref);
      if (m) diff(tokens(m.text), rt);
      add(sec, h("div", { class: "lab-ref" }, h("h3", null, tr("正确文字", "Correct text")), h("p", { class: "d-text", lang: "zh-CN" }, marked(rt))));
    }
    if (lab && lab.note) add(sec, h("p", { class: "lab-note" }, h("span", { class: "k" }, tr("备注", "Note")), h("span", null, lab.note)));
    var busyL = busy.label[t.id];
    add(
      sec,
      h(
        "div",
        { class: "actions" },
        h("button", { class: "btn", type: "button", "data-k": "relabel", "aria-expanded": "false", onclick: function () { gtOpen(t, true); } }, noteOnly ? tr("写备注…", "Write a note…") : lab ? tr("改正确文字…", "Edit the correct text…") : tr("拼出正确文字…", "Build the correct text…")),
        lab ? h("button", { class: "btn", type: "button", "data-k": "unlabel", disabled: !!busyL, onclick: function () { postLabel(t, { clear: true }, "clear"); } }, busyL === "clear" ? tr("正在清除…", "Clearing…") : tr("清除标注", "Clear label")) : null,
        note("label"),
      ),
    );
    return sec;
  }

  // gtForm is the pending ground truth: the base, the text with its places
  // lit behind it, the list of places with a reading per version, the note,
  // 确认为正确文字. Typing updates the draft, the lit places and the list
  // in place, so an input method keeps its composition.
  function gtForm(t, d, lab) {
    var busyL = busy.label[t.id];
    var vs = gtVersions(t);
    var save = h("button", { class: "btn" + (d.dirty ? " primary" : ""), type: "button", "data-k": "lab-save", disabled: !!busyL, onclick: function () { gtSave(t); } }, busyL === "save" ? tr("正在存…", "Saving…") : d.noteOnly ? tr("存备注", "Save note") : tr("确认为正确文字", "Confirm as correct text"));
    var status = h("span", { class: "act-note", "data-kind": d.err ? "err" : d.dirty ? "warn" : null, role: "status" }, d.err || (d.dirty ? tr("未存", "Unsaved") : ""));
    var mirror = h("div", { class: "gt-mirror", "aria-hidden": "true" });
    var list = h("ol", { class: "gt-places", id: "gt-places", "aria-label": tr("不同的地方", "Differences") });
    var head = h("h3", { class: "gt-head" });
    function paintMirror() {
      mirror.replaceChildren();
      d.parts.forEach(function (x) {
        if (!x.opts) return add(mirror, x.s);
        // the mark covers the words; the spaces around a place stay plain
        var m = /^(\s*)([\s\S]*?)(\s*)$/.exec(x.s);
        add(mirror, m[1], m[2] ? h("mark", { class: "dif", "data-hand": x.pick === "hand" ? "" : null }, m[2]) : null, m[3]);
      });
      add(mirror, "\u200b"); // a last line the box has too
    }
    function grow() {
      ta.style.height = "auto";
      ta.style.height = ta.scrollHeight + 2 + "px";
    }
    function touched() {
      status.dataset.kind = "warn";
      status.textContent = tr("未存", "Unsaved");
      save.classList.add("primary");
      lead(t);
    }
    // the words around a place, cut to 10 characters on their far side
    function before(at) {
      return (at > 10 ? "…" : "") + gtText(d).slice(Math.max(0, at - 10), at);
    }
    function after(at) {
      var all = gtText(d);
      return all.slice(at, at + 10) + (at + 10 < all.length ? "…" : "");
    }
    function paintList() {
      var places = [];
      d.parts.forEach(function (x, i) { if (x.opts) places.push(i); });
      head.textContent = places.length ? tr("不同的地方 · " + places.length + " 处", "Differences · " + places.length) : vs.length > 1 ? tr("各版本说的字都一样", "Every version has the same words") : "";
      list.replaceChildren();
      places.forEach(function (i, n) {
        var x = d.parts[i], r = gtRange(d, i), when = gtWhen(t, d, x);
        add(
          list,
          h(
            "li",
            null,
            h("span", { class: "n num" }, String(n + 1)),
            h("span", { class: "ctx" }, before(r[0])),
            h(
              "span",
              { class: "picks", role: "group", "aria-label": tr("第 " + (n + 1) + " 处用哪一版", "Which version for difference " + (n + 1)) },
              x.opts.map(function (o, k) {
                var word = o.text.trim();
                return h(
                  "button",
                  { class: "pill", type: "button", "aria-pressed": x.pick === k ? "true" : "false", "data-k": "pick:" + n + ":" + k, onclick: function () { gtPick(d, i, k); redraw(); } },
                  h("span", { class: "who", translate: "no" }, o.names.join(tr("、", ", "))),
                  word ? word : h("span", { class: "none" }, tr("（没有字）", "(nothing)")),
                );
              }),
              h(
                "button",
                { class: "pill", type: "button", "aria-pressed": x.pick === "hand" ? "true" : "false", "data-k": "hand:" + n, title: tr("在上面的文字里选中这一处，直接改", "Select this spot in the text above to edit it"), onclick: function () { var g = gtRange(d, i); ta.focus(); ta.setSelectionRange(g[0], g[1]); } },
                tr("手改", "By hand"),
              ),
            ),
            h("span", { class: "ctx" }, after(r[1])),
            when ? h("button", { class: "icon-btn", type: "button", "aria-label": tr("听第 " + (n + 1) + " 处", "Play difference " + (n + 1)), title: tr("听这一处", "Play this spot"), "data-k": "hear:" + n, onclick: function () { playSpan(when.s, when.e, true); } }, icon("play", 12)) : null,
          ),
        );
      });
    }
    function redraw() {
      ta.value = gtText(d);
      paintMirror();
      paintList();
      grow();
      touched();
    }
    function keys(e) {
      if (e.key === "Enter" && !e.isComposing && (e.metaKey || e.ctrlKey || e.target.tagName === "INPUT")) {
        e.preventDefault();
        gtSave(t);
      } else if (e.key === "Escape") {
        e.stopPropagation();
        e.target.blur();
      }
    }
    var ta = h("textarea", {
      id: "lab-ref",
      "data-k": "lab-ref",
      rows: "1",
      lang: "zh-CN",
      spellcheck: "false",
      "aria-describedby": "gt-places",
      oninput: function (e) {
        gtEdit(d, e.target.value);
        paintMirror();
        paintList();
        grow();
        touched();
      },
      onkeydown: keys,
    });
    ta.value = gtText(d);
    paintMirror();
    paintList();
    requestAnimationFrame(grow);
    var ni = h("input", { type: "text", id: "lab-note", "data-k": "lab-note", autocomplete: "off", placeholder: tr("哪里听错了，比如 yolk 听成了 yoke…", "What was misheard, e.g. yolk heard as yoke…"), oninput: function (e) { d.note = e.target.value; d.dirty = true; touched(); }, onkeydown: keys });
    ni.value = d.note;
    if (d.noteOnly)
      return h(
        "div",
        { class: "picker lab-form", role: "group", "aria-label": tr("备注", "Note") },
        h("label", { class: "field", for: "lab-note" }, tr("备注", "Note")),
        ni,
        h("div", { class: "confirm-row" }, save, h("button", { class: "btn", type: "button", "data-k": "lab-fold", onclick: function () { gtFold(t); } }, tr("收起", "Close")), status),
      );
    return h(
      "div",
      { class: "picker lab-form", role: "group", "aria-label": tr("待确认的正确文字", "Pending correct text") },
      vs.length > 1
        ? h(
            "div",
            { class: "starts" },
            h("span", { class: "muted" }, tr("从这版开始", "Start from")),
            vs.map(function (v) {
              return h("button", { class: "pill", type: "button", "aria-pressed": d.base === v.id ? "true" : "false", "data-k": "base:" + v.id, onclick: function () { gtBase(t, v.id); } }, v.label && v.name ? [v.label, " ", h("span", { class: "mono", translate: "no" }, v.name)] : v.label || h("span", { class: "mono", translate: "no" }, v.name));
            }),
            d.prev ? h("button", { class: "link", type: "button", "data-k": "base-undo", onclick: function () { GT[t.id] = d.prev; drawDetail(); } }, tr("撤销换底", "Undo the switch")) : null,
          )
        : null,
      h("label", { class: "field", for: "lab-ref" }, tr("待确认的正确文字", "Pending correct text")),
      h("div", { class: "gt-box" }, mirror, ta),
      head,
      list,
      h("label", { class: "field", for: "lab-note" }, tr("备注", "Note")),
      ni,
      h(
        "div",
        { class: "confirm-row" },
        save,
        h("button", { class: "btn", type: "button", "data-k": "lab-fold", onclick: function () { gtFold(t); } }, tr("收起", "Close")),
        status,
      ),
    );
  }

  // lead gives the primary back and forth in place as the draft is touched:
  // the take's 复制 or 重发… while nothing is being composed, else 确认.
  function lead(t) {
    var undel = t.state === "undelivered" && !t.dismissed;
    var on = !DT.picker && !gtBusy(t.id);
    var c = detailEl.querySelector('[data-k="copy"]'), r = detailEl.querySelector('[data-k="resend"]');
    if (c) c.classList.toggle("primary", on && !undel);
    if (r) r.classList.toggle("primary", on && undel);
  }

  function gtOpen(t, focus) {
    if (!t.audio) return;
    var lab = t.label;
    var d = GT[t.id];
    if (t.track === "backup") GT[t.id] = Object.assign(d || {}, { base: "", parts: [], tb: null, note: d ? d.note || "" : (lab && lab.note) || "", open: true, err: "", noteOnly: true });
    else if (!d || !d.parts) GT[t.id] = compose(t, lab && lab.ref ? "label" : "main");
    else d.open = true;
    DT.note = null;
    drawDetail();
    var ta = focus && detailEl.querySelector(t.track === "backup" ? "#lab-note" : "#lab-ref");
    if (ta) {
      ta.focus();
      ta.setSelectionRange(ta.value.length, ta.value.length);
    }
  }
  // gtBase starts the draft again from another version; an edited draft it
  // replaces stays one 撤销换底 away.
  function gtBase(t, id) {
    var was = GT[t.id];
    var d = compose(t, id);
    d.note = was ? was.note : "";
    d.dirty = true;
    d.prev = was && was.dirty ? was : null;
    GT[t.id] = d;
    drawDetail();
  }
  function gtFold(t) {
    var d = GT[t.id];
    if (d) d.open = false;
    drawDetail();
    var b = detailEl.querySelector('[data-k="relabel"]');
    if (b) b.focus({ preventScroll: true });
  }
  function gtSave(t) {
    var d = GT[t.id];
    if (!d) return;
    var text = gtText(d);
    if (!text.trim() && !d.note.trim()) {
      d.err = d.noteOnly ? tr("写下备注，再存。", "Write a note, then save.") : tr("写下正确的文字或者备注，再确认。", "Write the correct text or a note, then confirm.");
      drawDetail();
      return;
    }
    postLabel(t, d.noteOnly ? { note: d.note } : { ref: text, note: d.note }, "save");
  }

  // markClick is a click on 这版对: it marks that version alone, or with
  // add (⇧, ⌘) marks it as well or takes it out; a plain click on a pressed
  // one clears the label, as does taking out the last one. The note stays.
  function markClick(t, name, add) {
    if (busy.label[t.id]) return;
    var lab = t.label;
    var have = lab ? lab.correct || [] : [];
    var on = have.indexOf(name) >= 0;
    var next = add ? (on ? have.filter(function (n) { return n !== name; }) : have.concat([name])) : on ? [] : [name];
    var d = GT[t.id];
    var noteText = d && d.dirty ? d.note : lab ? lab.note || "" : "";
    postLabel(t, next.length ? { correct: next, note: noteText } : { clear: true }, next.length ? "mark" : "clear");
  }

  // undoClear is the 撤销 beside 已清除标注: it posts the label the clear
  // took away, as it was made — the engines clicked, or the typed text.
  function undoClear(t, was) {
    var body = was.label_source === "click" ? { correct: was.correct || [], note: was.note || "" } : { ref: was.ref || "", note: was.note || "" };
    return h("button", { class: "link", type: "button", "data-k": "label-undo", onclick: function () { postLabel(t, body, "undo"); } }, tr("撤销", "Undo"));
  }

  // postLabel appends the take's label: POST /takes/api/label, {id, ref,
  // note, correct, clear}, answered with the row labels.jsonl got. The
  // label shows at once; a failure leaves it as it was and says why beside
  // the buttons.
  function postLabel(t, body, kind) {
    var id = t.id;
    if (busy.label[id]) return;
    busy.label[id] = kind;
    if (GT[id]) GT[id].err = "";
    DT.note = null;
    drawDetail();
    postJSON("api/label", Object.assign({ id: id }, body))
      .then(
        function (row) {
          var lab = row.label_source === "cleared" ? null : row;
          var was = t.label;
          t.label = lab;
          t.labeled = !!lab;
          if (DT.take && DT.take.id === id) DT.take = Object.assign({}, DT.take, { label: lab, labeled: !!lab });
          patchRow({ id: id, labeled: !!lab });
          delete GT[id];
          if (DT.id === id) say("label", "ok", lab ? (kind === "undo" ? tr("已恢复标注", "Label restored") : kind === "save" ? (lab.ref ? tr("已存为正确文字", "Saved as correct text") : tr("已存备注", "Note saved")) : tr("已存标注", "Label saved")) : was ? [tr("已清除标注 ", "Label cleared "), undoClear(t, was)] : tr("已清除标注", "Label cleared"));
          render();
        },
        function (err) {
          if (err === Refused) return failed(err);
          var msg = [tr("没存上：", "Not saved: "), refusal(err), tr("。标注没有改动，再试一次。", ". The label is unchanged; try again.")];
          if (GT[id] && kind === "save") GT[id].err = msg;
          else if (DT.id === id) say("label", "err", msg);
        },
      )
      .then(function () {
        busy.label[id] = "";
        if (DT.id === id) drawDetail();
      });
  }

  // togglePicker opens the resend picker under the text of where (main, or
  // a tab's id) for the text m, and reads the targets anew at every open:
  // the frontmost app (this page's browser) is left out by the server, and
  // a target gone since the last look is not offered.
  function togglePicker(where, m) {
    if (DT.picker && DT.picker.kind === "resend" && DT.picker.where === where) {
      DT.picker = null;
      drawDetail();
      return;
    }
    DT.picker = { kind: "resend", where: where, src: m.src, text: m.text, v: m };
    DT.targets = null;
    DT.enter = false;
    DT.dest = "";
    drawDetail();
    var id = DT.id;
    api("api/targets?id=" + encodeURIComponent(id)).then(
      function (r) {
        if (DT.id !== id) return;
        DT.targets = r;
        drawDetail();
      },
      function (err) {
        if (err === Refused) return failed(err);
        if (DT.id !== id) return;
        DT.targets = { err: err.message };
        drawDetail();
      },
    );
  }

  function opt(name, value, main, sub, disabled, checked, onpick) {
    return h(
      "label",
      { class: "opt", "aria-disabled": disabled ? "true" : null },
      h("input", { type: "radio", name: name, value: value, disabled: disabled, checked: checked, "data-k": name + ":" + value, onchange: onpick }),
      h("span", { class: "name" }, main),
      sub,
    );
  }

  function resendPicker(t) {
    var pk = DT.picker;
    var T = DT.targets;
    var box = h("div", { class: "picker", role: "group", "aria-label": tr("重发到", "Resend to") });
    if (!T) {
      add(box, h("p", { class: "muted" }, tr("正在读取可以发到的地方…", "Loading places to send to…")));
      return box;
    }
    if (T.err) {
      add(box, h("p", { class: "act-note", "data-kind": "err" }, tr("读不到目标：", "Couldn't load targets: ") + T.err));
      return box;
    }
    // Only the take's own target is picked for the user; anything else is
    // the user's click, since a resend types into what it reaches.
    var sendable = T.targets.filter(function (x) { return x.to; });
    var ownTo = T.targets.filter(function (x) { return x.group === "own" && x.to; })[0];
    if (!sendable.some(function (x) { return x.to === DT.dest; })) DT.dest = ownTo ? ownTo.to : "";
    var chosen = sendable.filter(function (x) { return x.to === DT.dest; })[0];
    function pick(x) {
      return function () {
        DT.dest = x.to;
        drawDetail();
      };
    }
    function o(x, label) {
      return opt("dest", x.to, label || x.label, null, false, DT.dest === x.to, pick(x));
    }
    var g = { own: [], recent: [], pane: [], app: [] };
    T.targets.forEach(function (x) {
      if (g[x.group]) g[x.group].push(x);
    });
    var own = g.own[0];
    if (own) {
      add(box, h("h3", null, tr("原目标", "Original target")));
      if (own.to) add(box, h("div", { class: "opts" }, o(own)));
      else add(box, h("p", { class: "gone" }, (own.front ? tr("原目标正显示着这个页面，发过去会贴进本页 · ", "The original target is showing this page; sending would paste into it · ") : tr("原目标已关闭 · ", "The original target is closed · ")) + own.label));
    }
    // a recent target that cannot be reached now shows greyed, not offered
    function recent(x) {
      if (x.to) return o(x);
      return opt("dest", "", x.label, h("span", { class: "sub" }, x.pane && !T.herdr ? tr("herdr 没有运行", "herdr isn't running") : tr("已关闭", "Closed")), true, false, null);
    }
    if (g.recent.length) add(box, h("h3", null, tr("最近发过", "Recent")), h("div", { class: "opts" }, g.recent.map(recent)));
    add(box, h("h3", null, tr("面板", "Panes")));
    if (!T.herdr) add(box, h("p", { class: "gone" }, tr("herdr 没有运行", "herdr isn't running")));
    else if (!g.pane.length) add(box, h("p", { class: "gone" }, tr("herdr 里没有面板", "No panes in herdr")));
    else {
      var byWs = {};
      var order = [];
      g.pane.forEach(function (p) {
        var ws = p.workspace || "";
        if (!byWs[ws]) {
          byWs[ws] = [];
          order.push(ws);
        }
        byWs[ws].push(p);
      });
      order.forEach(function (ws) {
        if (ws) add(box, h("h3", null, h("span", { class: "ws" }, ws)));
        add(box, h("div", { class: "opts" }, byWs[ws].map(function (p) { return o(p, ws ? p.label.slice(ws.length + 3) : p.label); })));
      });
    }
    if (g.app.length) add(box, h("h3", null, tr("应用", "Apps")), h("div", { class: "opts" }, g.app.map(function (x) { return o(x); })));
    add(box, h("h3", null, tr("剪贴板", "Clipboard")), h("div", { class: "opts" }, opt("dest", "clipboard", tr("只放进剪贴板", "Clipboard only"), null, false, DT.dest === "clipboard", pick({ to: "clipboard" }))));
    if (!chosen) {
      add(box, h("div", { class: "confirm" }, h("p", { class: "muted" }, tr("选一个目标，这里会写明发到哪里、怎么发。", "Pick a target; this then says where and how it is sent."))));
      return box;
    }
    var clip = chosen.to === "clipboard";
    var inFlight = busy.resend[t.id];
    var q = Array.from(pk.text);
    add(
      box,
      h(
        "div",
        { class: "confirm" },
        h("p", null, clip ? tr("放进剪贴板，不粘贴", "Copy to the clipboard, no paste") : tr("发到 " + chosen.label + "，粘贴 · ", "Send to " + chosen.label + ", paste · ") + (DT.enter ? tr("按 Enter", "press Enter") : tr("不回车", "no Enter"))),
        h("p", { class: "quote" }, tr("「", "“") + q.slice(0, 40).join("") + (q.length > 40 ? "…" : "") + tr("」", "”")),
        h(
          "div",
          { class: "confirm-row" },
          h(
            "button",
            { class: "btn primary", type: "button", "data-k": "send", disabled: !!inFlight, onclick: function () { doResend(t, pk, chosen); } },
            inFlight ? tr("发送中…", "Sending…") : clip ? tr("放进剪贴板", "Copy to clipboard") : DT.enter ? tr("粘贴并按 Enter", "Paste and press Enter") : tr("粘贴", "Paste"),
          ),
          clip
            ? null
            : h(
                "label",
                { class: "check" },
                h("input", { type: "checkbox", "data-k": "enter", checked: DT.enter, onchange: function (e) { DT.enter = e.target.checked; drawDetail(); } }),
                tr("粘贴后按 Enter", "Press Enter after pasting"),
              ),
          h("button", { class: "btn", type: "button", "data-k": "fold", onclick: function () { DT.picker = null; drawDetail(); } }, tr("收起", "Close")),
        ),
      ),
    );
    return box;
  }

  // doResend sends the picked text to the picked target. The button waits
  // while the request is out (the server waits for a delivery under way);
  // the outcome shows beside the button that opened the picker, and the
  // timeline shows the new deliver line.
  function doResend(t, pk, chosen) {
    var id = t.id;
    if (busy.resend[id]) return;
    busy.resend[id] = true;
    drawDetail();
    var where = pk.where;
    postJSON("api/resend", { id: id, to: chosen.to, send: DT.enter && chosen.to !== "clipboard", text: pk.src })
      .then(
        function (line) {
          if (DT.id !== id) return;
          var name = chosen.to === "clipboard" ? tr("剪贴板", "Clipboard") : chosen.label;
          // not_recorded: the text went out and the record lacks its line;
          // the picker closes as after a delivery, so it is not pasted twice
          if (line.ok && line.not_recorded) {
            DT.picker = null;
            say(where, "err", [tr("已发出，记录未写入 → " + name + "：", "Sent, but not recorded → " + name + ": "), reason(line.not_recorded)]);
          } else if (line.ok) {
            DT.picker = null;
            say(where, "ok", (chosen.to === "clipboard" ? tr("已放进剪贴板", "Copied to the clipboard") : (line.submit === "ok" ? tr("已发送到 ", "Sent to ") : tr("已粘贴到 ", "Pasted into ")) + name) + " · " + dur(t.dur_s));
          } else say(where, "err", [tr("送达失败 → " + name + "：", "Delivery failed → " + name + ": "), reason(line.err), line.not_recorded ? tr("；记录未写入", "; not recorded") : null]);
        },
        function (err) {
          if (err === Refused) return failed(err);
          if (DT.id === id) say(where, "err", [tr("没有发出去：", "Not sent: "), refusal(err, { v: pk.v })]);
        },
      )
      .then(function () {
        busy.resend[id] = false;
        if (DT.id === id) {
          drawDetail();
          reloadTake(id);
        }
      });
  }

  // ------------------------------------------------------------ player

  // P is the player of the take shown: its waveform is the scrub bar, and
  // its audio is fetched through api on the first play, as a blob: URL.
  var P = null;
  var RATES = [0.75, 1, 1.25, 1.5, 2];

  function loadPeaks(t) {
    stopPlayer();
    if (!t.audio || t.state === "recording") return;
    var id = t.id;
    // a waveform that does not load leaves a flat bar that still seeks and
    // plays, with the error beside the player
    function ready(pk, err) {
      if (DT.id !== id) return;
      P = makePlayer(t, pk);
      if (err) say("player", "err", tr("读不到波形：" + err.message + "。仍然可以播放。", "Couldn't load the waveform: " + err.message + ". It still plays."));
      drawDetail();
      if (playWhenReady === id) {
        playWhenReady = "";
        togglePlay();
      }
    }
    api("api/peaks/" + encodeURIComponent(id) + "?n=160").then(ready, function (err) {
      if (err === Refused) return failed(err);
      ready({ dur_s: t.dur_s, peaks: [] }, err);
    });
  }

  function stopPlayer() {
    if (!P) return;
    if (P.audio) {
      P.audio.pause();
      P.audio.removeAttribute("src");
    }
    if (P.url) URL.revokeObjectURL(P.url);
    cancelAnimationFrame(P.raf);
    P = null;
  }

  function makePlayer(t, pk) {
    var p = { id: t.id, dur: pk.dur_s || t.dur_s || 0, pos: 0, rate: 1, muted: false, playing: false, audio: null, url: null, loading: null, raf: 0 };
    var peaks = pk.peaks.length ? pk.peaks : [0];
    var top = Math.max.apply(null, peaks.concat([0.001]));
    var N = peaks.length;
    var bars = peaks
      .map(function (v, i) {
        var hh = Math.max(2, (v / top) * 52);
        return '<rect x="' + (i * 4 + 0.5) + '" y="' + (28 - hh / 2) + '" width="3" height="' + hh + '" rx="1"/>';
      })
      .join("");
    var s = svg(N * 4, 56, bars);
    s.setAttribute("preserveAspectRatio", "none");
    p.rects = s.querySelectorAll("rect");
    p.head = h("span", { class: "wave-head" });
    p.band = h("span", { class: "wave-band", hidden: true });
    p.tip = h("span", { class: "wave-tip" }, "0:00");
    function at(e) {
      var r = p.wave.getBoundingClientRect();
      return Math.max(0, Math.min(1, (e.clientX - r.left) / r.width)) * p.dur;
    }
    p.wave = h(
      "div",
      {
        class: "wave",
        role: "slider",
        tabindex: "0",
        "data-k": "wave",
        "aria-label": tr("播放位置", "Playback position"),
        "aria-valuemin": "0",
        "aria-valuemax": String(Math.round(p.dur)),
        onpointerdown: function (e) {
          p.wave.setPointerCapture(e.pointerId);
          seek(at(e));
        },
        onpointermove: function (e) {
          var r = p.wave.getBoundingClientRect();
          var x = Math.max(0, Math.min(r.width, e.clientX - r.left));
          p.tip.style.left = x + "px";
          p.tip.textContent = dur((x / r.width) * p.dur);
          if (p.wave.hasPointerCapture(e.pointerId)) seek(at(e));
        },
        onkeydown: function (e) {
          if (e.key === "Home") seek(0);
          else if (e.key === "End") seek(p.dur);
          else if (e.key === "ArrowRight" || e.key === "ArrowLeft") step(e);
          else return;
          e.preventDefault();
          e.stopPropagation();
        },
      },
      p.band,
      s,
      p.head,
      p.tip,
    );
    p.now = h("span", { class: "up-now" }, "0:00");
    p.playBtn = h("button", { class: "up-play", type: "button", "data-k": "play", "aria-label": tr("播放", "Play"), onclick: togglePlay }, icon("play"));
    p.rateBtn = h("button", { class: "up-ctl", type: "button", "data-k": "rate", onclick: function () { rate(1, true); } }, "1×");
    p.muteBtn = h("button", { class: "up-ctl", type: "button", "data-k": "mute", "aria-pressed": "false", onclick: toggleMute }, tr("静音", "Mute"));
    p.root = h(
      "div",
      { class: "up" },
      h(
        "div",
        { class: "up-bar" },
        p.playBtn,
        h("span", { class: "up-time" }, p.now, h("span", { class: "up-sep" }, "/"), h("span", { class: "up-total" }, dur(p.dur))),
        h("span", { class: "up-grow" }),
        p.rateBtn,
        p.muteBtn,
        h("span", { class: "up-keys" }, tr("空格 播放 · ← → 跳", "Space play · ← → skip")),
      ),
      p.wave,
      h("div", { class: "wave-ticks", "aria-hidden": "true" }, h("span", null, "0:00"), h("span", null, dur(p.dur / 2)), h("span", null, dur(p.dur))),
    );
    return p;
  }

  // paint shows the player's state: played bars, the playhead, the time,
  // the play button, the rate and the mute switch.
  function paint() {
    if (!P) return;
    var f = P.dur ? P.pos / P.dur : 0;
    var n = P.rects.length;
    for (var i = 0; i < n; i++) P.rects[i].classList.toggle("p", (i + 0.5) / n <= f);
    P.head.style.left = f * 100 + "%";
    P.now.textContent = dur(P.pos);
    P.wave.setAttribute("aria-valuenow", String(Math.round(P.pos)));
    P.wave.setAttribute("aria-valuetext", dur(P.pos) + " / " + dur(P.dur));
    if (P.playing) P.root.setAttribute("data-playing", "");
    else P.root.removeAttribute("data-playing");
    P.playBtn.setAttribute("aria-label", P.playing ? tr("暂停", "Pause") : tr("播放", "Play"));
    P.playBtn.replaceChildren(icon(P.playing ? "pause" : "play"));
    P.rateBtn.textContent = P.rate + "×";
    P.rateBtn.setAttribute("aria-label", tr("播放速度 " + P.rate + " 倍", "Playback speed " + P.rate + "×"));
    P.muteBtn.setAttribute("aria-pressed", P.muted ? "true" : "false");
    P.muteBtn.textContent = P.muted ? tr("已静音", "Muted") : tr("静音", "Mute");
  }

  function seek(s) {
    if (!P) return;
    P.pos = Math.max(0, Math.min(P.dur, s));
    if (P.audio && P.audio.readyState > 0) P.audio.currentTime = P.pos;
    paint();
  }
  // step moves by an arrow: 1 s in a take under 60 s and 5 s otherwise,
  // with Shift 10 s and 60 s.
  function step(e) {
    if (!P) return;
    var d = P.dur < 60 ? (e.shiftKey ? 10 : 1) : e.shiftKey ? 60 : 5;
    seek(P.pos + (e.key === "ArrowRight" ? d : -d));
  }
  function rate(d, wrap) {
    if (!P) return;
    var i = RATES.indexOf(P.rate) + d;
    P.rate = wrap ? RATES[(i + RATES.length) % RATES.length] : RATES[Math.max(0, Math.min(RATES.length - 1, i))];
    if (P.audio) P.audio.playbackRate = P.rate;
    paint();
  }
  function toggleMute() {
    if (!P) return;
    P.muted = !P.muted;
    if (P.audio) P.audio.muted = P.muted;
    paint();
  }

  function togglePlay() {
    var p = P;
    if (!p) return;
    if (p.playing) {
      p.playing = false;
      p.stopAt = 0;
      if (p.audio) p.audio.pause();
      paint();
      return;
    }
    p.playing = true;
    if (p.pos >= p.dur - 0.05) p.pos = 0;
    paint();
    // one fetch per player: a play pressed again while the audio is out
    // waits for the same answer, and an answer for a player no longer shown
    // is dropped before it becomes a blob: URL
    if (!p.audio && !p.loading)
      p.loading = api("audio/" + encodeURIComponent(p.id) + ".wav", { blob: true }).then(
        function (b) {
          p.loading = null;
          if (P !== p) return;
          p.url = URL.createObjectURL(b);
          p.audio = new Audio();
          p.audio.addEventListener("ended", function () {
            p.playing = false;
            p.pos = p.dur;
            if (P === p) paint();
          });
          p.audio.src = p.url;
        },
        function (err) {
          p.loading = null;
          throw err;
        },
      );
    (p.audio ? Promise.resolve() : p.loading)
      .then(function () {
        if (P !== p || !p.playing) return;
        p.audio.muted = p.muted;
        p.audio.playbackRate = p.rate;
        p.audio.currentTime = p.pos;
        return p.audio.play().then(function () {
          (function frame() {
            if (P !== p || !p.playing) return;
            p.pos = p.audio.currentTime;
            if (p.stopAt && p.pos >= p.stopAt) {
              p.stopAt = 0;
              p.playing = false;
              p.audio.pause();
              paint();
              return;
            }
            paint();
            p.raf = requestAnimationFrame(frame);
          })();
        });
      })
      .catch(function (err) {
        if (err === Refused) return failed(err);
        p.playing = false;
        if (P === p) {
          paint();
          say("player", "err", tr("放不出来：", "Can't play: ") + err.message);
          drawDetail();
        }
      });
  }

  // playSpan plays the take from just before s, with its span [s, e] shown
  // on the waveform; with stop it pauses just after e: a word clicked plays
  // on, a place of the ground truth plays alone.
  function playSpan(s, e, stop) {
    if (!P) return;
    P.band.hidden = false;
    P.band.style.left = (s / P.dur) * 100 + "%";
    P.band.style.width = Math.max(0.5, ((e - s) / P.dur) * 100) + "%";
    seek(Math.max(0, s - 0.15));
    P.stopAt = stop ? Math.min(P.dur, e + 0.3) : 0;
    if (!P.playing) togglePlay();
  }

  function playerBlock(t) {
    if (t.state === "recording") return h("div", { class: "up" }, h("p", { class: "up-none" }, badge("recording"), h("span", { class: "mono" }, dur(t.dur_s)), h("span", { class: "muted" }, tr("结束后可以播放", "Plays when it ends"))));
    if (!t.audio) return h("div", { class: "up" }, h("p", { class: "up-none" }, h("span", { class: "muted" }, tr("这一条没有录音文件：不到 0.3 秒的录音不保存声音。", "This take has no audio file: a take under 0.3 s keeps no audio."))));
    if (!P || P.id !== t.id) return h("div", { class: "up" }, h("p", { class: "up-none" }, h("span", { class: "muted" }, tr("正在读取波形…", "Loading the waveform…"))));
    paint();
    return [P.root, note("player") ? h("div", { class: "actions" }, note("player")) : null];
  }

  // ------------------------------------------------------------ re-transcription

  function engineOf(name) {
    return (ENG || []).filter(function (e) { return e.name === name; })[0];
  }

  function toggleEngines() {
    if (DT.picker && DT.picker.kind === "retr") {
      DT.picker = null;
      drawDetail();
      return;
    }
    DT.picker = { kind: "retr" };
    drawDetail();
    api("api/engines").then(function (r) {
      ENG = r.engines || [];
      drawDetail();
    }, failedAction);
  }

  var NOT_NAMED = tr("配置没有启用它（asr.engine，或 compare.on 和 compare.engines）", "the config doesn't enable it (asr.engine, or compare.on and compare.engines)");

  // reason is why a delivery failed, in the page's words: a deliver line's
  // err, written by herdr or the system as the record keeps it. The English
  // stays in the record (原始事件) and the log; words the page does not know
  // show as they were written, in mono.
  var REASONS = [
    [/pane_not_found|no such pane/, function () { return tr("面板已关闭", "The pane is closed"); }],
    [/^herdr (socket|status server)|^herdr .*(executable file not found|connection refused|no such file)/, function () { return tr("herdr 没有运行", "herdr isn't running"); }],
    [/^no app was frontmost/, function () { return tr("当时没有前台应用", "No app was in front"); }],
    [/^no running application with pid/, function () { return tr("应用已经退出", "The app has quit"); }],
    [/^(.+) \(pid \d+\) has quit/, function (m) { return tr(m[1] + " 已经退出", m[1] + " has quit"); }],
    [/^(.+) did not take focus within/, function (m) { return tr(m[1] + " 没能回到原来的窗口和输入框", m[1] + " didn't return to the original window and input field"); }],
    [/^(.+) lost focus before Enter/, function (m) { return tr("粘贴后 " + m[1] + " 失去了焦点", m[1] + " lost focus after the paste"); }],
    [/^another delivery is under way/, function () { return tr("另一条正在送达，等它完成再发", "Another take is being delivered; send once it's done"); }],
    [/no space left on device/, function () { return tr("磁盘满了", "The disk is full"); }],
    [/permission denied/, function () { return tr("没有写入的权限", "No permission to write"); }],
  ];
  // CODES word a request the server refused, by the code of its answer. A
  // function takes what the page asked for: {v} the version a resend sent,
  // {engine} the engine a re-transcription named. The two refusals of 422
  // read apart: a resend of a version with no text, a cloud engine the
  // config does not name.
  var CODES = {
    no_take: tr("这一条录音已经不在存储里了", "This take is no longer in the store"),
    recording: tr("还在录音", "Still recording"),
    transcribing: tr("还在识别", "Still transcribing"),
    resending: tr("这条正在重发", "This take is being resent"),
    retranscribing: tr("这条正在重新识别", "This take is being re-transcribed"),
    delivery_busy: tr("另一条正在送达，等它完成再发", "Another take is being delivered; send once it's done"),
    target_gone: tr("目标已关闭", "The target is closed"),
    no_herdr: tr("herdr 没有运行", "herdr isn't running"),
    front_app: tr("不能发到最前面的应用：会贴进本页", "Can't send to the frontmost app: it would paste into this page"),
    page_window: tr("这个应用正显示着本页：发过去会贴进本页", "This app is showing this page: sending would paste into it"),
    label_empty: tr("写下正确的文字或者备注", "Write the correct text or a note"),
    no_answer: tr("这个引擎没有给出这一条的文字，不能标它对", "This engine gave no text for this take, so it can't be marked correct"),
    backup_text: tr("这一条的文字来自备用输入，录音文件里没有这段声音：只能存备注", "This take's text came from the backup input, which the audio file doesn't hold: a note only"),
    disk_full: tr("磁盘满了", "The disk is full"),
    no_permission: tr("没有写入的权限", "No permission to write"),
    no_text: function (x) {
      var v = x && x.v;
      var why = !v ? "" : v.retr ? (v.err ? tr("重新识别 #" + v.id.slice(1) + " 失败了", "re-transcription #" + v.id.slice(1) + " failed") : tr("没有重新识别 #" + v.id.slice(1), "there is no re-transcription #" + v.id.slice(1))) : v.src === "raw" ? tr("原始文字是空的", "the raw text is empty") : v.src === "delivered" ? tr("送达的文字是空的", "the delivered text is empty") : v.err ? tr(v.name + " 这次识别失败了", v.name + " failed this time") : tr("对比记录里没有 " + v.name + " 的结果", "the compare record has no result from " + v.name);
      return tr("这一版没有文字可发", "This version has no text to send") + (why ? tr("：", ": ") + why : "");
    },
    not_named: function (x) {
      return ((x && x.engine) || tr("这个引擎", "This engine")) + tr(" 是云端引擎，", " is a cloud engine; ") + NOT_NAMED;
    },
  };
  // refusal is a refused request in the page's words, by its code; a code
  // the page does not know shows the server's words as they are.
  function refusal(err, x) {
    var c = CODES[err.code];
    if (typeof c === "function") return c(x);
    if (c) return c;
    return reason(err.message);
  }
  function reason(msg) {
    msg = String(msg || "").trim();
    if (!msg) return tr("没有说原因", "No reason given");
    for (var i = 0; i < REASONS.length; i++) {
      var m = REASONS[i][0].exec(msg);
      if (m) return REASONS[i][1](m);
    }
    if (/[\u3400-\u9fff]/.test(msg)) return msg; // the server's own Chinese (还在录音, 这条正在重发…)
    return h("span", { class: "mono" }, msg);
  }

  // enginePicker offers every engine; a cloud engine carries its mark, and
  // one the config does not run is shown, not offered, with the keys that
  // would run it.
  function enginePicker(t) {
    var es = ENG || [];
    var box = h("div", { class: "picker", role: "group", "aria-label": tr("用哪个引擎重新识别", "Engine to re-transcribe with") });
    if (!ENG) {
      add(box, h("p", { class: "muted" }, tr("正在读取引擎…", "Loading engines…")));
      return box;
    }
    var named = es.filter(function (e) { return e.named; });
    if (!named.some(function (e) { return e.name === DT.engine; })) DT.engine = named.length ? (named.filter(function (e) { return e.primary; })[0] || named[0]).name : "";
    add(
      box,
      h(
        "div",
        { class: "opts" },
        es.map(function (e) {
          var sub = e.local
            ? h("span", { class: "sub" }, e.primary ? tr("本机 · 当前引擎", "On this Mac · current engine") : tr("本机", "On this Mac"))
            : e.named
              ? h("span", { class: "cloud" }, svg(10, 10, ICON.out), tr("云端 · 音频会发到" + (e.service || "云端"), "Cloud · the audio goes to " + (e.service || "the cloud")))
              : h("span", { class: "sub" }, tr("云端 · ", "Cloud · ") + NOT_NAMED);
          return h(
            "label",
            { class: "opt", "aria-disabled": e.named ? null : "true" },
            h("input", { type: "radio", name: "engine", value: e.name, "data-k": "engine:" + e.name, disabled: !e.named, checked: DT.engine === e.name, onchange: function () { DT.engine = e.name; drawDetail(); } }),
            h("span", { class: "name mono" }, e.name),
            sub,
          );
        }),
      ),
    );
    var chosen = engineOf(DT.engine);
    add(
      box,
      h(
        "div",
        { class: "confirm-row" },
        h(
          "button",
          { class: "btn primary", type: "button", "data-k": "retr-go", disabled: !chosen || !!busy.retr[t.id], onclick: function () { doRetranscribe(t, chosen); } },
          chosen && !chosen.local ? tr("发到云端识别", "Transcribe in the cloud") : tr("开始识别", "Transcribe"),
        ),
        h("button", { class: "btn", type: "button", "data-k": "fold", onclick: function () { DT.picker = null; drawDetail(); } }, tr("收起", "Close")),
      ),
    );
    return box;
  }

  // doRetranscribe decodes the take again with the engine; while it runs
  // the button is off, so a second click sends nothing. The answer opens
  // as its own tab beside the others, a failed one with its error.
  function doRetranscribe(t, e) {
    var id = t.id;
    if (busy.retr[id]) return;
    busy.retr[id] = e.name;
    DT.picker = null;
    DT.note = null;
    drawDetail();
    postJSON("api/retranscribe", { id: id, engine: e.name })
      .then(
        function (line) {
          if (DT.id !== id) return;
          if (line.err) say("retr", "err", tr("重新识别 #" + line.n + " 失败 · ", "Re-transcription #" + line.n + " failed · ") + e.name);
          else say("retr", "ok", tr("重新识别 #", "Re-transcription #") + line.n + " · " + e.name + " · " + ((line.latency_ms || 0) / 1000).toFixed(1) + tr(" 秒", " s"));
        },
        function (err) {
          if (err === Refused) return failed(err);
          if (DT.id === id) say("retr", "err", [tr("没有识别：", "Not transcribed: "), refusal(err, { engine: e.name })]);
        },
      )
      .then(function () {
        // the take is read before the next draw, so the new tab is there
        busy.retr[id] = "";
        if (DT.id === id) reloadTake(id).then(drawDetail);
      });
  }

  // ------------------------------------------------------------ timeline

  var STOP = tr(
    { tap: "按右 Option 结束", enter: "按 Enter 结束并发送", cancel: "按 Esc 取消", auto: "自动结束", stream_end: "输入结束", write_error: "写文件出错，停下", shutdown: "被重启打断" },
    { tap: "Stopped with right Option", enter: "Stopped with Enter, to send", cancel: "Cancelled with Esc", auto: "Stopped automatically", stream_end: "Input ended", write_error: "Stopped: a file write failed", shutdown: "Cut off by a restart" },
  );
  var HOLD = tr(
    { cancelled: "已取消，文字已存下", empty: "没有听到话", deliver_failed: "未送达 · 送达失败", asr_failed: "未送达 · 识别失败", interrupted: "未送达 · 被打断", delivery_cut: "可能已粘贴 · 被打断" },
    { cancelled: "Cancelled, text kept", empty: "No speech heard", deliver_failed: "Not delivered · delivery failed", asr_failed: "Not delivered · transcription failed", interrupted: "Not delivered · interrupted", delivery_cut: "May have pasted · interrupted" },
  );
  var VIA = tr({ hotkey: "快捷键重贴 · ", page: "从历史页重发 · ", cli: "命令重发 · " }, { hotkey: "Re-pasted by hotkey · ", page: "Resent from Recordings · ", cli: "Resent by command · " });
  function evWords(e) {
    var tg = target(e.target).full || tr("当前光标", "the cursor");
    switch (e.ev) {
      case "start":
        return tr("开始 → ", "Started → ") + tg;
      case "mic_live":
        return tr("麦克风有声音", "Mic picked up sound");
      case "no_signal":
        return tr("没信号", "No signal");
      case "backup_on":
        return tr("打开备用输入", "Backup input on");
      case "stop":
        return STOP[e.kind] || tr("结束", "Stopped");
      case "send_on":
        return tr("识别时按了 Enter：识别完就发送", "Enter pressed while transcribing: sends when done");
      case "text":
        return tr("识别完成 · ", "Transcribed · ") + (e.engine || "") + " · " + tr((e.chars || 0) + " 字", charsN(e.chars || 0)) + (e.latency_ms ? " · " + (e.latency_ms / 1000).toFixed(1) + tr(" 秒", " s") : "") + (e.failed_chunks ? " · " + e.failed_chunks + tr(" 段失败", e.failed_chunks === 1 ? " chunk failed" : " chunks failed") : "") + (e.audio === "backup" ? tr(" · 用备用输入", " · from the backup input") : "");
      case "hold":
        return HOLD[e.why] || tr("未送达", "Not delivered");
      case "deliver":
        if (!e.ok) return [(VIA[e.via] || "") + tr("送达失败 → ", "Delivery failed → ") + tg + tr("：", ": "), reason(e.err)];
        return [(VIA[e.via] || "") + (e.submit === "ok" ? tr("已发送 → ", "Sent → ") : tr("已粘贴 → ", "Pasted → ")) + tg + (e.received === "whole" ? tr(" · 完整", " · complete") : e.received === "short" ? tr(" · 不完整", " · incomplete") : ""), e.submit === "err" ? [tr(" · 没有按 Enter：", " · Enter not pressed: "), reason(e.err)] : null];
      case "received":
        return tr("读回 #", "Read back #") + e.n + " · " + (e.received === "whole" ? tr("完整", "complete") : e.received === "short" ? tr("不完整", "incomplete") : tr("看不出", "unclear"));
      case "recover":
        return tr("重启后找回", "Recovered after a restart");
      case "retranscribe":
        return tr("重新识别 #", "Re-transcription #") + e.n + " · " + e.engine + (e.err ? tr(" · 失败：", " · failed: ") + e.err : " · " + tr(Array.from(e.text || "").length + " 字", charsN(Array.from(e.text || "").length)));
      case "seen":
        return tr("在历史页打开过", "Opened in Recordings");
      case "dismiss":
        return tr("忽略：不再算作未送达", "Dismissed: no longer counts as not delivered");
      case "undismiss":
        return tr("取消忽略", "Undismissed");
    }
    return e.ev;
  }
  // offset is when an event came, from the take's start: +m:ss, +h:mm:ss
  // past an hour, and the day and time past a day (a resend days later).
  function offset(ms, at) {
    var s = Math.max(0, Math.round(ms / 1000));
    if (s < 3600) return "+" + dur(s);
    if (s < 86400) return "+" + Math.floor(s / 3600) + ":" + p2(Math.floor(s / 60) % 60) + ":" + p2(s % 60);
    return at.getMonth() + 1 + "/" + at.getDate() + " " + hm(at);
  }
  function timeline(t) {
    if (!t.events.length) return h("section", { class: "block" }, h("h2", null, tr("经过", "Timeline")), h("p", { class: "muted" }, tr("早期录音：这一条在行为记录出现之前，没有经过。", "Early take: recorded before megavoice logged events, so it has no timeline.")));
    var t0 = new Date(t.events[0].at).getTime();
    return h(
      "section",
      { class: "block", "aria-labelledby": "h-tl" },
      h("h2", { id: "h-tl" }, tr("经过", "Timeline")),
      h(
        "ol",
        { class: "tl" },
        t.events.map(function (e) {
          var bad = (e.ev === "deliver" && !e.ok) || (e.ev === "hold" && e.why !== "cancelled" && e.why !== "empty") || (e.ev === "retranscribe" && e.err);
          var at = new Date(e.at);
          return h("li", null, h("span", { class: "off" }, offset(at.getTime() - t0, at)), h("span", { class: bad ? "bad" : e.ev === "deliver" ? "ok" : null }, evWords(e)));
        }),
      ),
      h(
        "details",
        { class: "more-box" },
        h("summary", null, tr("原始事件", "Raw events")),
        h("pre", null, t.events.map(function (e) { return JSON.stringify(e); }).join("\n")),
      ),
    );
  }

  function drawDetail() {
    if (!detailEl) return;
    var a = document.activeElement;
    var focusK = a && detailEl.contains(a) && a.dataset ? a.dataset.k : null;
    // a text field being written keeps its caret across a redraw
    var sel = focusK && a.setSelectionRange && typeof a.selectionStart === "number" ? [a.selectionStart, a.selectionEnd, a.scrollTop] : null;
    detailEl.dataset.take = S.sel || "";
    var t = DT.take;
    var parts;
    if (!S.sel) parts = h("p", { class: "d-empty" }, tr("选一条录音，在这里看文字、标注、播放和重发。", "Pick a take to see its text, label it, play it or resend it here."));
    else if (DT.err) parts = h("p", { class: "d-empty" }, DT.err);
    else if (!t) parts = h("p", { class: "d-empty" }, tr("正在载入…", "Loading…"));
    else
      parts = [
        dheader(t),
        texts(t),
        labelBlock(t),
        playerBlock(t),
        timeline(t),
        h("details", { class: "more-box" }, h("summary", null, tr("文件", "Files")), h("div", { class: "paths" }, t.files.map(function (p) { return h("span", { translate: "no" }, p); }))),
      ];
    detailEl.replaceChildren();
    add(detailEl, parts);
    if (focusK) {
      var f = detailEl.querySelector('[data-k="' + focusK + '"]');
      if (f) f.focus({ preventScroll: true });
      if (f && sel && f.setSelectionRange) {
        f.setSelectionRange(sel[0], sel[1]);
        f.scrollTop = sel[2];
      }
    }
  }

  // ------------------------------------------------------------ page

  var app, main, observer;
  function render() {
    fresh();
    var focusK = document.activeElement && (document.activeElement.dataset.id ? "id:" + document.activeElement.dataset.id : document.activeElement.dataset.k);
    var anchor = anchorRow();
    var next;
    if (D.refused) {
      next = h(
        "main",
        { class: "page-msg" },
        h("h1", null, tr("从菜单栏重新打开录音历史", "Reopen Recordings from the menu bar")),
        h("p", null, tr("这个标签页没有 megavoice 给的钥匙，或者钥匙已经换过。点菜单栏的 megavoice 图标，选「打开录音历史」。", "This tab doesn't have megavoice's key, or the key has changed. Click the megavoice icon in the menu bar and open the recordings from there.")),
      );
    } else if (!D.loaded) {
      // the list's column, where the list will be: the skeleton has the
      // layout it turns into
      next = h("main", { class: "page" });
      if (D.down) add(next, h("section", { class: "list-col" }, notice()));
      else if (D.skel)
        add(
          next,
          h(
            "section",
            { class: "list-col", "aria-busy": "true", "aria-label": tr("录音列表", "Recordings") },
            h("p", { class: "count-line muted" }, tr("正在载入…", "Loading…")),
            h("ul", { class: "skel", "aria-hidden": "true" }, [0, 1, 2, 3, 4, 5, 6, 7].map(function () {
              return h("li", null, h("span"), h("span"), h("span"), h("span"), h("span"));
            })),
          ),
        );
    } else if (!total() && !S.q && !filtered()) {
      next = h(
        "main",
        { class: "page", "data-empty": "" },
        h("section", { class: "list-col", id: "list" }, D.down ? notice() : null, h("div", { class: "empty" }, h("h2", null, tr("还没有录音", "No recordings yet")), h("p", null, tr("按右 Option 开始录音，再按一次结束。每一条都会出现在这里，取消的也在。", "Press right Option to start recording and again to stop. Every take shows up here, cancelled ones too.")))),
      );
    } else {
      if (!S.sel && WIDE.matches && D.rows.length) {
        S.sel = D.rows[0].id;
        S.auto = true;
      }
      // The page with its list is kept and only the list column is drawn
      // again, so the detail beside it never leaves the page.
      var lc = list();
      var view = S.sel && !WIDE.matches ? "detail" : "list";
      if (main && main.dataset.takes === "1") {
        main.replaceChild(lc, main.firstChild);
        main.dataset.view = view;
        next = main;
      } else next = h("main", { class: "page", "data-takes": "1", "data-view": view }, lc, detail());
    }
    if (!main) add(app, next);
    else if (main !== next) app.replaceChild(next, main);
    main = next;
    openTake();
    var kp = app.querySelector(".keys");
    if (kp) kp.remove();
    if (S.keys) add(app, keysPanel());
    keysBtn.setAttribute("aria-expanded", S.keys ? "true" : "false");
    drawn = sig();
    if (anchor) {
      var el = main.querySelector('a[data-id="' + anchor.id + '"]');
      if (el) window.scrollBy(0, el.closest(".row").getBoundingClientRect().top - anchor.top);
    }
    if (focusK) {
      var f = focusK.slice(0, 3) === "id:" ? main.querySelector('a[data-id="' + focusK.slice(3) + '"]') : main.querySelector('[data-k="' + focusK + '"]');
      if (f) f.focus({ preventScroll: true });
    }
    var more = document.getElementById("more");
    if (observer) observer.disconnect();
    if (more && D.more) observer.observe(more);
  }

  // anchorRow is the first row under the top bar and where it is, so that a
  // take coming in above it leaves it where it was on the screen.
  function anchorRow() {
    if (!main || window.scrollY === 0) return null;
    var top = parseFloat(getComputedStyle(document.documentElement).getPropertyValue("--top")) || 0;
    var rows = main.querySelectorAll(".row");
    for (var i = 0; i < rows.length; i++) {
      var r = rows[i].getBoundingClientRect();
      if (r.bottom > top) {
        var a = rows[i].querySelector("a[data-id]");
        return a ? { id: a.dataset.id, top: r.top } : null;
      }
    }
    return null;
  }

  function move(d) {
    var i = D.rows.findIndex(function (t) { return t.id === S.sel; });
    var n = D.rows[Math.max(0, Math.min(D.rows.length - 1, i + d))];
    if (!n) return;
    if (WIDE.matches) select(n.id);
    else render();
    var el = document.querySelector('a[data-id="' + n.id + '"]');
    if (el) {
      el.focus({ preventScroll: true });
      el.scrollIntoView({ block: "nearest" });
    }
    if (D.more && D.rows.length - D.rows.indexOf(n) < 20) loadMore();
  }

  function onKey(e) {
    var tag = (e.target.tagName || "").toLowerCase();
    var typing = tag === "input" || tag === "select" || tag === "textarea";
    if (e.key === "Escape") {
      if (typing && S.q) {
        S.q = "";
        qInput.value = "";
        load(true);
      } else if (S.keys) toggleKeys();
      else if (DT.picker) {
        DT.picker = null;
        drawDetail();
      } else if (!WIDE.matches && S.sel) select("");
      return;
    }
    var t = DT.id === S.sel ? DT.take : null;
    var m = t && mainText(t);
    // ⌘C with nothing selected on the page copies the take's text
    if (!typing && e.metaKey && e.key === "c" && m && window.getSelection().isCollapsed) {
      e.preventDefault();
      copyText(m.text, "main");
      return;
    }
    if (typing || e.metaKey || e.ctrlKey || e.altKey) return;
    // a held key repeats only the keys that move: j, k, the arrows and the
    // rate; a toggle held down is one press
    if (e.repeat && "/uc?rtei ".indexOf(e.key) >= 0) return;
    // 1–9 mark the nth version a label can name, as a click on its 这版对;
    // with Shift, as a ⇧-click
    var dm = /^Digit([1-9])$/.exec(e.code || "");
    if (dm) {
      var nm = t && !e.repeat && markNames(t)[+dm[1] - 1];
      if (nm) {
        e.preventDefault();
        markClick(t, nm, e.shiftKey);
      }
      return;
    }
    switch (e.key) {
      case "/":
        e.preventDefault();
        qInput.focus();
        break;
      case "j":
        move(1);
        break;
      case "k":
        move(-1);
        break;
      case "u":
        toggleUndelivered();
        break;
      case "?":
        toggleKeys();
        break;
      case "c":
        if (m) copyText(m.text, "main");
        break;
      case "r":
        if (m) togglePicker("main", m);
        break;
      case "e":
        if (t) {
          e.preventDefault(); // the key would land in the text field it opens
          gtOpen(t, true);
        }
        break;
      case "t":
        if (t && t.audio && t.state !== "recording" && !busy.retr[t.id]) toggleEngines();
        break;
      case "i":
        if (t && t.state === "undelivered") dismissOne(t);
        break;
      case " ":
        if (P && tag !== "button") {
          e.preventDefault();
          togglePlay();
        }
        break;
      case ",":
        rate(-1);
        break;
      case ".":
        rate(1);
        break;
      case "ArrowLeft":
      case "ArrowRight":
        if (P) {
          e.preventDefault();
          step(e);
        }
        break;
    }
  }

  function start() {
    app = document.getElementById("app");
    localize();
    add(app, topbar());
    observer = new IntersectionObserver(
      function (es) {
        if (es.some(function (x) { return x.isIntersecting; })) loadMore();
      },
      { rootMargin: "800px 0px" },
    );
    document.addEventListener("keydown", onKey);
    setTimeout(function () {
      if (!D.loaded && !D.refused) {
        D.skel = true;
        D.skelAt = Date.now();
        render();
      }
    }, 300);
    render();
    load();
    setInterval(poll, POLL);
    document.addEventListener("visibilitychange", poll);
    // a page left frees its audio; one restored from the back-forward cache
    // reads the waveform again
    addEventListener("pagehide", stopPlayer);
    addEventListener("beforeunload", function (e) {
      for (var id in GT)
        if (GT[id].dirty) {
          e.preventDefault();
          e.returnValue = "";
          return;
        }
    });
    addEventListener("pageshow", function (e) {
      if (e.persisted && DT.take) loadPeaks(DT.take);
    });
  }

  if (document.readyState === "loading") document.addEventListener("DOMContentLoaded", start);
  else start();
})();
