// Takes page mock: renders window.FIXTURE. Every record field is set as text,
// never as HTML. Nothing here writes the clipboard or plays audio: Copy and
// Resend show their outcome only, and the player moves a position.
(function () {
  "use strict";

  var F = window.FIXTURE;
  var app = document.getElementById("app");
  var params = new URLSearchParams(location.search);

  var PRESET = {
    undelivered: { take: 2 },
    "retrans-failed": { take: 2, tab: "r1" },
    "no-herdr": { take: 2, picker: "resend" },
    search: { q: "sourdough" },
    nomatch: { q: "火星" },
  };
  var preset = PRESET[F.name] || {};

  var S = {
    q: params.get("q") || preset.q || "",
    f: params.get("f") || "all",
    target: "",
    device: "",
    day: "",
    sel: params.get("take") || (preset.take != null && F.takes[preset.take] ? F.takes[preset.take].id : ""),
    tab: params.get("tab") || preset.tab || "raw",
    picker: params.get("picker") || preset.picker || "",
    pos: 0,
    playing: false,
    rate: 1,
    muted: false,
    note: null,
    keys: false,
  };

  var WIDE = window.matchMedia("(min-width: 1081px)");
  if (!S.sel && WIDE.matches) {
    var first = visible()[0];
    if (first) S.sel = first.id;
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
  function add(el, c) {
    if (c == null || c === false) return;
    if (Array.isArray(c)) c.forEach(function (x) { add(el, x); });
    else el.appendChild(typeof c === "string" ? document.createTextNode(c) : c);
  }
  function svg(w, hgt, body) {
    var s = document.createElementNS("http://www.w3.org/2000/svg", "svg");
    s.setAttribute("width", w);
    s.setAttribute("height", hgt);
    s.setAttribute("viewBox", "0 0 " + w + " " + hgt);
    s.setAttribute("aria-hidden", "true");
    s.innerHTML = body; // fixed markup, no record field
    return s;
  }
  function p2(n) {
    return String(n).padStart(2, "0");
  }
  function dur(s) {
    s = Math.max(0, Math.round(s));
    return Math.floor(s / 60) + ":" + p2(s % 60);
  }
  function hms(d) {
    return p2(d.getHours()) + ":" + p2(d.getMinutes()) + ":" + p2(d.getSeconds());
  }
  var WD = ["周日", "周一", "周二", "周三", "周四", "周五", "周六"];
  function dayKey(d) {
    return d.getFullYear() + "-" + (d.getMonth() + 1) + "-" + d.getDate();
  }
  function dayLabel(d) {
    var today = dayKey(d) === dayKey(F.now);
    return (today ? "今天 · " : "") + WD[d.getDay()] + " " + (d.getMonth() + 1) + "/" + d.getDate();
  }

  var WORD = { sent: "已发送", pasted: "已粘贴", cancelled: "已取消", empty: "无语音", undelivered: "未送达", recording: "录音中", transcribing: "识别中", legacy: "早期录音" };
  var WHY = { deliver_failed: "送达失败", asr_failed: "识别失败", interrupted: "被打断", partial: "不完整" };
  var GLYPH = {
    sent: '<circle cx="5" cy="5" r="4" fill="currentColor"/>',
    pasted: '<circle cx="5" cy="5" r="3.5" fill="none" stroke="currentColor" stroke-width="1.4"/><path d="M5 1.5a3.5 3.5 0 0 1 0 7z" fill="currentColor"/>',
    cancelled: '<circle cx="5" cy="5" r="3.5" fill="none" stroke="currentColor" stroke-width="1.4"/>',
    undelivered: '<path d="M5 1l4.2 7.6H.8z" fill="currentColor"/>',
    recording: '<circle cx="5" cy="5" r="3.5" fill="none" stroke="currentColor" stroke-width="1.4" stroke-dasharray="2 1.6"/>',
    transcribing: '<circle cx="5" cy="5" r="3.5" fill="none" stroke="currentColor" stroke-width="1.4" stroke-dasharray="2 1.6"/>',
    empty: '<path d="M1.5 5h7" stroke="currentColor" stroke-width="1.8"/>',
    legacy: '<circle cx="5" cy="5" r="3.5" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-dasharray="0 2.2"/>',
  };
  function glyph(s) {
    var g = svg(10, 10, GLYPH[s] || GLYPH.cancelled);
    g.setAttribute("class", "glyph");
    return g;
  }
  function badge(s) {
    return h("span", { class: "state", "data-s": s }, glyph(s), WORD[s]);
  }
  var ICON = {
    search: '<circle cx="7" cy="7" r="5" fill="none" stroke="currentColor" stroke-width="1.6"/><path d="M11 11l3.5 3.5" stroke="currentColor" stroke-width="1.6"/>',
    play: '<path d="M4 2.5v11l9-5.5z" fill="currentColor"/>',
    pause: '<path d="M4 2.5h3v11H4zM9 2.5h3v11H9z" fill="currentColor"/>',
    back: '<path d="M10 3L5 8l5 5" fill="none" stroke="currentColor" stroke-width="1.6"/>',
    theme: '<circle cx="8" cy="8" r="5.5" fill="none" stroke="currentColor" stroke-width="1.4"/><path d="M8 2.5a5.5 5.5 0 0 1 0 11z" fill="currentColor"/>',
    out: '<path d="M3 7L7 3M4 3h3v3" fill="none" stroke="currentColor" stroke-width="1.3"/>',
    check: '<path d="M1.5 5.5l2.5 2.5 4.5-6" fill="none" stroke="currentColor" stroke-width="1.6"/>',
    x: '<path d="M2 2l6 6M8 2l-6 6" stroke="currentColor" stroke-width="1.6"/>',
  };
  function icon(name, size) {
    return svg(size || 16, size || 16, ICON[name]).cloneNode(true);
  }
  function mark() {
    // A waveform, its first half played: the page's scrub bar in 20 px.
    var hs = [4, 8, 12, 7, 10, 5, 9, 12, 6, 4];
    var body = hs
      .map(function (v, i) {
        return '<rect x="' + i * 2.4 + '" y="' + (8 - v / 2) + '" width="1.5" height="' + v + '" rx=".7" fill="' + (i < 5 ? "var(--cobalt)" : "currentColor") + '"/>';
      })
      .join("");
    return svg(24, 16, body);
  }

  // ------------------------------------------------------------ data

  function fields(t) {
    var out = [{ src: "", text: t.text }, { src: "原始", text: t.raw }];
    t.versions.forEach(function (v) {
      out.push({ src: v.name, text: v.text, mono: true });
    });
    t.retrans.forEach(function (x) {
      if (x.text) out.push({ src: "重新识别 #" + x.n, text: x.text });
    });
    return out;
  }
  function matchOf(t, q) {
    if (!q) return null;
    var ql = q.toLowerCase();
    var fs = fields(t);
    for (var i = 0; i < fs.length; i++) {
      var at = (fs[i].text || "").toLowerCase().indexOf(ql);
      if (at >= 0) return { f: fs[i], at: at, len: q.length };
    }
    return null;
  }
  function visible() {
    return F.takes.filter(function (t) {
      if (S.f !== "all" && t.state !== S.f) return false;
      if (S.target && t.target.short !== S.target) return false;
      if (S.device && t.device !== S.device) return false;
      if (S.day && dayKey(t.at) !== S.day) return false;
      if (S.q && !matchOf(t, S.q)) return false;
      return true;
    });
  }
  function undelivered() {
    return F.takes.filter(function (t) {
      return t.state === "undelivered";
    });
  }
  function byId(id) {
    for (var i = 0; i < F.takes.length; i++) if (F.takes[i].id === id) return F.takes[i];
    return null;
  }
  function syncUrl() {
    var p = new URLSearchParams();
    if (F.name !== "list") p.set("state", F.name);
    if (S.q) p.set("q", S.q);
    if (S.f !== "all") p.set("f", S.f);
    if (S.sel) p.set("take", S.sel);
    history.replaceState(null, "", "?" + p.toString());
  }

  // ------------------------------------------------------------ top bar

  function topbar() {
    var input = h("input", {
      id: "q",
      type: "search",
      placeholder: "搜索文字…",
      "aria-label": "搜索录音文字",
      autocomplete: "off",
      spellcheck: "false",
      value: S.q,
      oninput: function (e) {
        S.q = e.target.value;
        render();
        document.getElementById("q").focus();
      },
    });
    input.value = S.q;
    var fresh = F.name === "offline" ? h("span", { class: "fresh", style: "color: var(--ochre)" }, "16:21 之后没有更新") : F.name === "loading" ? h("span", { class: "fresh" }) : h("span", { class: "fresh" }, "刚刚更新");
    return h(
      "header",
      { class: "topbar" },
      h(
        "div",
        { class: "topbar-in" },
        h("a", { class: "brand", href: "?", translate: "no" }, mark(), h("span", null, "录音历史")),
        h("div", { class: "search", role: "search" }, icon("search", 15), input, h("kbd", null, "/")),
        fresh,
        h(
          "div",
          { class: "tools" },
          h("button", { class: "icon-btn", type: "button", "aria-label": "快捷键", "aria-expanded": S.keys ? "true" : "false", onclick: toggleKeys, text: "?" }),
          h("button", { class: "icon-btn", type: "button", "aria-label": "切换深浅色", onclick: toggleTheme }, icon("theme")),
        ),
      ),
    );
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
  function keysPanel() {
    var rows = [
      ["/", "搜索"],
      ["j / k", "下一条 / 上一条"],
      ["Enter", "打开这一条"],
      ["c", "复制文字"],
      ["r", "重发…"],
      ["t", "重新识别…"],
      ["空格", "播放 / 暂停"],
      ["← →", "后退 / 前进，Shift 加大步长"],
      [", .", "变慢 / 变快"],
      ["u", "只看未送达"],
      ["Esc", "清除搜索，或收起"],
    ];
    return h(
      "div",
      { class: "keys", role: "dialog", "aria-label": "快捷键" },
      h("h2", null, "快捷键"),
      h(
        "dl",
        null,
        rows.map(function (r) {
          return [h("dt", null, r[0]), h("dd", null, r[1])];
        }),
      ),
    );
  }

  // ------------------------------------------------------------ list

  function listHead(vis) {
    var out = [];
    if (F.name === "offline")
      out.push(
        h("div", { class: "notice", "data-kind": "err", role: "status" }, h("span", { class: "state" }, glyph("undelivered")), h("b", null, "megavoice 未运行"), h("span", null, "下面是 16:21 的列表，它回来后会自动刷新")),
      );
    var u = undelivered();
    if (u.length || S.f === "undelivered")
      out.push(
        h(
          "button",
          {
            class: "notice strip",
            type: "button",
            "aria-pressed": S.f === "undelivered" ? "true" : "false",
            onclick: function () {
              S.f = S.f === "undelivered" ? "all" : "undelivered";
              render();
            },
          },
          glyph("undelivered"),
          h("b", null, u.length + " 条未送达"),
          h("span", { class: "go" }, S.f === "undelivered" ? "显示全部" : "只看这些"),
        ),
      );
    out.push(
      S.q
        ? h("h1", { class: "count-line" }, "找到 " + vis.length + " 条", h("span", { class: "of" }, "共 " + F.takes.length))
        : h("h1", { class: "count-line" }, (vis.length === F.takes.length ? F.takes.length : vis.length) + " 条录音", vis.length !== F.takes.length ? h("span", { class: "of" }, "共 " + F.takes.length) : null),
    );
    var counts = {};
    F.takes.forEach(function (t) {
      counts[t.state] = (counts[t.state] || 0) + 1;
    });
    var pills = [["all", "全部", F.takes.length]].concat(
      ["undelivered", "sent", "pasted", "cancelled", "empty"].map(function (s) {
        return [s, WORD[s], counts[s] || 0];
      }),
    );
    function sel(label, key, values) {
      return h(
          "select",
          {
            "aria-label": "按" + label + "筛选",
            onchange: function (e) {
              S[key] = e.target.value;
              render();
            },
          },
          h("option", { value: "" }, "全部" + label),
          values.map(function (v) {
            return h("option", { value: v[0], selected: S[key] === v[0] }, v[1]);
          }),
      );
    }
    function uniq(fn) {
      var seen = {};
      var out = [];
      F.takes.forEach(function (t) {
        var v = fn(t);
        if (!seen[v[0]]) {
          seen[v[0]] = 1;
          out.push(v);
        }
      });
      return out;
    }
    out.push(
      h(
        "div",
        { class: "filters" },
        h(
          "div",
          { class: "pills", role: "group", "aria-label": "按状态筛选" },
          pills.map(function (p) {
            return h(
              "button",
              {
                class: "pill",
                type: "button",
                "aria-pressed": S.f === p[0] ? "true" : "false",
                "data-zero": p[2] === 0 ? "" : null,
                onclick: function () {
                  S.f = p[0];
                  render();
                },
              },
              p[1],
              h("span", { class: "n" }, String(p[2])),
            );
          }),
        ),
        h(
          "div",
          { class: "selects" },
          sel("目标", "target", uniq(function (t) { return [t.target.short, t.target.short]; })),
          sel("设备", "device", uniq(function (t) { return [t.device, t.device]; })),
          sel("日期", "day", uniq(function (t) { return [dayKey(t.at), dayLabel(t.at)]; })),
        ),
      ),
    );
    return out;
  }

  function snippet(m) {
    var t = m.f.text;
    var a = Math.max(0, m.at - 14);
    var b = Math.min(t.length, m.at + m.len + 40);
    return [m.f.src ? h("span", { class: "src" + (m.f.mono ? " mono" : "") }, m.f.src) : null, a > 0 ? "…" : "", t.slice(a, m.at), h("mark", null, t.slice(m.at, m.at + m.len)), t.slice(m.at + m.len, b), b < t.length ? "…" : ""];
  }

  function row(t) {
    var m = S.q ? matchOf(t, S.q) : null;
    var txt;
    if (t.state === "recording") txt = h("span", { class: "txt muted" }, "结束后显示文字");
    else if (t.state === "empty") txt = h("span", { class: "txt" }, "（无语音）");
    else txt = h("span", { class: "txt" }, t.reason ? h("span", { class: "why" }, WHY[t.reason]) : null, m ? snippet(m) : t.text.split("\n")[0]);
    var long = !m && t.dur > 60 && (t.state === "sent" || t.state === "pasted" || t.state === "undelivered");
    return h(
      "li",
      {
        class: "row",
        "data-s": t.state,
        "data-long": long ? "" : null,
        "data-play": t.state !== "recording" ? "" : null,
        "aria-current": S.sel === t.id ? "true" : null,
      },
      h(
        "a",
        {
          class: "t",
          href: "?take=" + t.id,
          "data-id": t.id,
          "aria-label": hms(t.at) + " " + WORD[t.state] + " " + dur(t.dur),
          onclick: function (e) {
            if (e.metaKey || e.ctrlKey || e.shiftKey) return;
            e.preventDefault();
            select(t.id, true);
          },
        },
        hms(t.at),
      ),
      h("span", { class: "len" }, dur(t.dur)),
      t.state !== "recording"
        ? h(
            "button",
            {
              class: "row-play",
              type: "button",
              "aria-label": "播放 " + hms(t.at),
              onclick: function () {
                select(t.id, true);
                S.playing = true;
                tick();
                render();
              },
            },
            icon("play", 12),
          )
        : null,
      badge(t.state),
      h("span", { class: "tgt", title: t.target.full }, t.target.short),
      txt,
    );
  }

  function list() {
    var vis = visible();
    var col = h("section", { class: "list-col", id: "list", "aria-label": "录音列表", tabindex: "-1" });
    add(col, listHead(vis));
    if (!vis.length) {
      add(
        col,
        h(
          "div",
          { class: "empty" },
          h("h2", null, S.q ? "没有匹配「" + S.q + "」" : "这个筛选下没有录音"),
          h(
            "button",
            {
              class: "link",
              type: "button",
              onclick: function () {
                S.q = "";
                S.f = "all";
                S.target = S.device = S.day = "";
                render();
              },
            },
            "清除筛选",
          ),
        ),
      );
      return col;
    }
    var groups = [];
    vis.forEach(function (t) {
      var k = dayKey(t.at);
      var g = groups[groups.length - 1];
      if (!g || g.k !== k) groups.push((g = { k: k, d: t.at, rows: [] }));
      g.rows.push(t);
    });
    groups.forEach(function (g) {
      var u = g.rows.filter(function (t) { return t.state === "undelivered"; }).length;
      add(
        col,
        h(
          "section",
          { class: "day", "aria-label": dayLabel(g.d) },
          h("div", { class: "day-head" }, h("h2", null, dayLabel(g.d)), h("span", { class: "day-meta" }, g.rows.length + " 条", u ? [" · ", h("span", { class: "warn" }, u + " 未送达")] : null)),
          h("ul", { class: "rows" }, g.rows.map(row)),
        ),
      );
    });
    add(col, h("p", { class: "more" }, "已显示 " + vis.length + " 条 · 更早的录音随滚动载入"));
    return col;
  }

  // ------------------------------------------------------------ detail

  function header(t) {
    var d = t.at;
    return h(
      "header",
      { class: "d-head" },
      h("h1", null, h("span", null, d.getMonth() + 1 + "月" + d.getDate() + "日 " + WD[d.getDay()] + " ", h("span", { class: "mono", style: "font-size: inherit" }, hms(d))), badge(t.state), t.reason ? h("span", { class: "d-why" }, WHY[t.reason]) : null),
      h(
        "div",
        { class: "d-meta" },
        h("span", null, "时长 ", h("b", { class: "num" }, dur(t.dur))),
        h("span", null, "原目标 ", h("b", { title: t.target.full }, t.target.short)),
        h("span", null, "设备 ", h("b", null, t.device)),
        h("span", null, "引擎 ", h("b", { class: "mono" }, t.engine)),
      ),
    );
  }

  function note() {
    if (!S.note) return null;
    return h("span", { class: "act-note", "data-kind": S.note.kind, role: "status" }, S.note.kind === "ok" ? [svg(10, 10, ICON.check), " "] : null, S.note.text);
  }

  function textBlock(t) {
    var undel = t.state === "undelivered";
    var open = S.picker === "resend";
    return [
      t.text ? h("p", { class: "d-text", lang: "zh-CN" }, t.text) : h("p", { class: "d-text none" }, t.state === "recording" ? "录音中，结束后在这里显示文字。" : "（无语音）"),
      t.text
        ? h(
            "div",
            { class: "actions" },
            h("button", { class: "btn" + (!undel && !open ? " primary" : ""), type: "button", onclick: function () { copy(t.text); } }, "复制"),
            h("button", { class: "btn" + (undel && !open ? " primary" : ""), type: "button", "aria-expanded": open ? "true" : "false", onclick: function () { S.picker = open ? "" : "resend"; render(); } }, "重发…"),
            note(),
          )
        : null,
      open ? resendPicker(t) : null,
    ];
  }

  function copy(text) {
    S.note = { kind: "ok", text: "已复制 " + text.length + " 字" };
    render();
  }

  function opt(name, value, main, sub, disabled, checked) {
    return h(
      "label",
      { class: "opt", "aria-disabled": disabled ? "true" : null },
      h("input", { type: "radio", name: name, value: value, disabled: disabled, checked: checked, onchange: function () { S.dest = value; render(); } }),
      h("span", { class: "name" }, main),
      sub ? h("span", { class: "sub" }, sub) : null,
    );
  }

  function resendPicker(t) {
    var T = F.targets;
    var origGone = t.reason === "deliver_failed" || (!T.herdr && t.target.kind === "pane");
    var all = [];
    if (!origGone) all.push(t.target);
    T.recent.forEach(function (x) {
      if (T.herdr || x.kind !== "pane") all.push(x);
    });
    if (T.herdr) all = all.concat(T.panes);
    all = all.concat(T.apps);
    all.push({ kind: "clipboard", short: "剪贴板", full: "剪贴板" });
    if (!S.dest || !all.some(function (x) { return x.full === S.dest; })) S.dest = all[0].full;
    var chosen = all.filter(function (x) { return x.full === S.dest; })[0];
    var byWs = {};
    T.panes.forEach(function (p) {
      var ws = p.full.split(" › ")[0];
      (byWs[ws] = byWs[ws] || []).push(p);
    });
    function o(x) {
      return opt("dest", x.full, x.kind === "pane" ? x.full.split(" › ")[1] : x.full, null, false, S.dest === x.full);
    }
    var enter = !!S.enter;
    return h(
      "div",
      { class: "picker", role: "group", "aria-label": "重发到" },
      h("h3", null, "原目标"),
      origGone ? h("p", { class: "gone" }, "原目标已关闭 · " + t.target.full) : h("div", { class: "opts" }, o(t.target)),
      h("h3", null, "最近发过"),
      h("div", { class: "opts" }, T.recent.map(function (x) { return !T.herdr && x.kind === "pane" ? opt("dest", x.full, x.full, "herdr 没有运行", true, false) : o(x); })),
      h("h3", null, "面板"),
      T.herdr
        ? Object.keys(byWs).map(function (ws) {
            return [h("h3", null, h("span", { class: "ws" }, ws)), h("div", { class: "opts" }, byWs[ws].map(o))];
          })
        : h("p", { class: "gone" }, "herdr 没有运行"),
      h("h3", null, "应用"),
      h("div", { class: "opts" }, T.apps.map(o)),
      h("h3", null, "剪贴板"),
      h("div", { class: "opts" }, opt("dest", "剪贴板", "只放进剪贴板", null, false, S.dest === "剪贴板")),
      h(
        "div",
        { class: "confirm" },
        h("p", null, chosen.kind === "clipboard" ? "放进剪贴板，不粘贴" : "发到 " + chosen.full + "，粘贴 · " + (enter ? "按 Enter" : "不回车")),
        h("p", { class: "quote" }, "「" + t.text.slice(0, 40) + (t.text.length > 40 ? "…" : "") + "」"),
        h(
          "div",
          { class: "confirm-row" },
          h(
            "button",
            {
              class: "btn primary",
              type: "button",
              onclick: function () {
                S.picker = "";
                S.note = { kind: "ok", text: (chosen.kind === "clipboard" ? "已放进剪贴板" : "已粘贴到 " + chosen.short) + " · " + dur(t.dur) };
                render();
              },
            },
            chosen.kind === "clipboard" ? "放进剪贴板" : enter ? "粘贴并按 Enter" : "粘贴",
          ),
          chosen.kind !== "clipboard" ? h("label", { class: "check" }, h("input", { type: "checkbox", checked: enter, onchange: function (e) { S.enter = e.target.checked; render(); } }), "粘贴后按 Enter") : null,
          h("button", { class: "btn", type: "button", onclick: function () { S.picker = ""; render(); } }, "收起"),
        ),
      ),
    );
  }

  function player(t) {
    if (t.state === "recording" || t.state === "transcribing")
      return h("div", { class: "up" }, h("p", { class: "up-none" }, badge(t.state), h("span", { class: "mono" }, dur(t.dur)), h("span", { class: "muted" }, "结束后可以播放")));
    var N = t.peaks.length;
    var W = N * 4;
    var played = S.pos / t.dur;
    var bars = t.peaks
      .map(function (v, i) {
        var hh = Math.max(2, v * 52);
        return '<rect x="' + (i * 4 + 0.5) + '" y="' + (28 - hh / 2) + '" width="3" height="' + hh + '" rx="1"' + ((i + 0.5) / N <= played ? ' class="p"' : "") + "/>";
      })
      .join("");
    var s = svg(W, 56, bars);
    s.setAttribute("preserveAspectRatio", "none");
    var tip = h("span", { class: "wave-tip" }, "0:00");
    var wave = h(
      "div",
      {
        class: "wave",
        role: "slider",
        tabindex: "0",
        "aria-label": "播放位置",
        "aria-valuemin": "0",
        "aria-valuemax": String(Math.round(t.dur)),
        "aria-valuenow": String(Math.round(S.pos)),
        "aria-valuetext": dur(S.pos) + " / " + dur(t.dur),
        onpointerdown: function (e) {
          wave.setPointerCapture(e.pointerId);
          seekTo(e, wave, t);
        },
        onpointermove: function (e) {
          var r = wave.getBoundingClientRect();
          var x = Math.max(0, Math.min(r.width, e.clientX - r.left));
          tip.style.left = x + "px";
          tip.textContent = dur((x / r.width) * t.dur);
          if (wave.hasPointerCapture(e.pointerId)) seekTo(e, wave, t);
        },
        onkeydown: function (e) {
          var small = t.dur < 60 ? 1 : 5;
          var big = t.dur < 60 ? 10 : 60;
          var step = e.shiftKey ? big : small;
          if (e.key === "ArrowRight") S.pos = Math.min(t.dur, S.pos + step);
          else if (e.key === "ArrowLeft") S.pos = Math.max(0, S.pos - step);
          else if (e.key === "Home") S.pos = 0;
          else if (e.key === "End") S.pos = t.dur;
          else return;
          e.preventDefault();
          render();
          document.querySelector(".wave").focus();
        },
      },
      s,
      h("span", { class: "wave-head", style: "left:" + played * 100 + "%" }),
      tip,
    );
    return h(
      "div",
      { class: "up", "data-playing": S.playing ? "" : null },
      h(
        "div",
        { class: "up-bar" },
        h("button", { class: "up-play", type: "button", "aria-label": S.playing ? "暂停" : "播放", onclick: togglePlay }, icon(S.playing ? "pause" : "play")),
        h("span", { class: "up-time" }, h("span", { class: "up-now" }, dur(S.pos)), h("span", { class: "up-sep" }, "/"), h("span", { class: "up-total" }, dur(t.dur))),
        h("span", { class: "up-grow" }),
        h("button", { class: "up-ctl", type: "button", "aria-label": "播放速度 " + S.rate + " 倍", onclick: function () { rate(1); } }, S.rate + "×"),
        h("button", { class: "up-ctl", type: "button", "aria-pressed": S.muted ? "true" : "false", onclick: function () { S.muted = !S.muted; render(); } }, S.muted ? "已静音" : "静音"),
        h("span", { class: "up-keys" }, "空格 播放 · ← → 跳"),
      ),
      wave,
      h("div", { class: "wave-ticks", "aria-hidden": "true" }, h("span", null, "0:00"), h("span", null, dur(t.dur / 2)), h("span", null, dur(t.dur))),
    );
  }
  function seekTo(e, wave, t) {
    var r = wave.getBoundingClientRect();
    S.pos = Math.max(0, Math.min(1, (e.clientX - r.left) / r.width)) * t.dur;
    render();
  }
  var timer = null;
  function togglePlay() {
    S.playing = !S.playing;
    tick();
    render();
  }
  function tick() {
    clearInterval(timer);
    if (!S.playing) return;
    timer = setInterval(function () {
      var t = byId(S.sel);
      if (!t) return;
      S.pos += 0.1 * S.rate;
      if (S.pos >= t.dur) {
        S.pos = t.dur;
        S.playing = false;
        clearInterval(timer);
      }
      render();
    }, 100);
  }
  var RATES = [0.75, 1, 1.25, 1.5, 2];
  function rate(d) {
    var i = RATES.indexOf(S.rate) + d;
    S.rate = RATES[(i + RATES.length) % RATES.length];
    render();
  }

  function versions(t) {
    var tabs = [{ id: "raw", label: "原始", text: t.raw }];
    t.versions.forEach(function (v) {
      tabs.push({ id: v.name, label: v.name, text: v.text, cloud: v.cloud, mono: true });
    });
    t.retrans.forEach(function (x) {
      tabs.push({ id: "r" + x.n, label: "重新识别 #" + x.n, sub: x.engine, text: x.text, err: x.err });
    });
    if (!tabs.some(function (x) { return x.id === S.tab; })) S.tab = "raw";
    var cur = tabs.filter(function (x) { return x.id === S.tab; })[0];
    var open = S.picker === "retrans";
    return h(
      "section",
      { class: "block", "aria-labelledby": "h-ver" },
      h("div", { class: "tab-head" }, h("h2", { id: "h-ver" }, "其他版本"), h("button", { class: "btn", type: "button", "aria-expanded": open ? "true" : "false", onclick: function () { S.picker = open ? "" : "retrans"; render(); } }, "重新识别…")),
      open ? engines() : null,
      h(
        "div",
        { class: "tabs", role: "tablist" },
        tabs.map(function (x) {
          return h(
            "button",
            { type: "button", role: "tab", "aria-selected": x.id === S.tab ? "true" : "false", onclick: function () { S.tab = x.id; render(); } },
            x.err ? h("span", { class: "err-dot" }, svg(10, 10, ICON.x)) : null,
            x.mono ? h("span", { class: "mono" }, x.label) : x.label,
            x.sub ? h("span", { class: "mono muted" }, x.sub) : null,
            x.cloud ? h("span", { class: "cloud" }, "云端") : null,
          );
        }),
      ),
      h(
        "div",
        { role: "tabpanel" },
        cur.err
          ? h("div", { class: "alert", role: "alert" }, h("h3", null, svg(10, 10, ICON.x), "识别失败 · " + cur.sub), h("p", null, h("code", null, cur.err)), h("p", { class: "muted" }, "存下的文字都没有改动。换一个引擎再试。"))
          : [
              h("p", { class: "d-text" }, cur.text),
              h(
                "div",
                { class: "actions" },
                h("button", { class: "btn", type: "button", onclick: function () { copy(cur.text); } }, "复制"),
                h("button", { class: "btn", type: "button", onclick: function () { S.picker = "resend"; render(); window.scrollTo(0, 0); } }, "重发…"),
              ),
            ],
      ),
    );
  }

  function engines() {
    var chosen = S.engine || "sensevoice";
    var e = F.engines.filter(function (x) { return x.name === chosen; })[0];
    return h(
      "div",
      { class: "picker", role: "group", "aria-label": "用哪个引擎重新识别" },
      h(
        "div",
        { class: "opts" },
        F.engines.map(function (x) {
          return h(
            "label",
            { class: "opt" },
            h("input", { type: "radio", name: "engine", value: x.name, checked: chosen === x.name, onchange: function () { S.engine = x.name; render(); } }),
            h("span", { class: "name mono" }, x.name),
            x.local ? h("span", { class: "sub" }, x.current ? "本机 · 当前引擎" : "本机") : h("span", { class: "cloud" }, svg(10, 10, ICON.out), "云端 · 音频会发到" + x.vendor),
          );
        }),
      ),
      h(
        "div",
        { class: "confirm-row" },
        h("button", { class: "btn primary", type: "button", onclick: function () { S.picker = ""; S.note = { kind: "ok", text: "已排队：用 " + chosen + " 重新识别" }; render(); } }, e && !e.local ? "发到云端识别" : "开始识别"),
        h("button", { class: "btn", type: "button", onclick: function () { S.picker = ""; render(); } }, "收起"),
      ),
    );
  }

  var EV = {
    start: function (e) { return "开始 → " + e.target; },
    mic_live: function () { return "麦克风有声音"; },
    no_signal: function () { return "没信号"; },
    stop: function (e) { return { tap: "按右 Option 结束", enter: "按 Enter 结束并发送", cancel: "按 Esc 取消", auto: "自动结束", shutdown: "被重启打断" }[e.kind]; },
    text: function (e) { return "识别完成 · " + e.engine + " · " + e.chars + " 字 · " + (e.latency_ms / 1000).toFixed(1) + " 秒"; },
    hold: function (e) { return { cancelled: "已取消，文字已存下", empty: "没有听到话", deliver_failed: "未送达 · 送达失败", asr_failed: "未送达 · 识别失败", interrupted: "未送达 · 被打断" }[e.why]; },
    deliver: function (e) { return e.ok ? (e.submit === "ok" ? "已发送 → " : "已粘贴 → ") + e.target + (e.received === "whole" ? " · 完整" : "") : "送达失败 → " + e.target + "：" + e.err; },
    retranscribe: function (e) { return "重新识别 #" + e.n + " · " + e.engine + (e.err ? " · 失败：" + e.err : ""); },
  };
  function timeline(t) {
    if (!t.events.length) return h("section", { class: "block" }, h("h2", null, "经过"), h("p", { class: "muted" }, "早期录音：这一条在行为记录出现之前，没有经过。"));
    return h(
      "section",
      { class: "block", "aria-labelledby": "h-tl" },
      h("h2", { id: "h-tl" }, "经过"),
      h(
        "ol",
        { class: "tl" },
        t.events.map(function (e) {
          var bad = (e.ev === "deliver" && !e.ok) || e.ev === "hold" && e.why !== "cancelled" && e.why !== "empty" || (e.ev === "retranscribe" && e.err);
          return h("li", null, h("span", { class: "off" }, "+" + dur(e.at)), h("span", { class: bad ? "bad" : e.ev === "deliver" ? "ok" : null }, EV[e.ev] ? EV[e.ev](e) : e.ev));
        }),
      ),
      h(
        "details",
        { class: "more-box" },
        h("summary", null, "原始事件"),
        h(
          "pre",
          null,
          t.events
            .map(function (e) {
              var o = { v: 1, at: new Date(t.at.getTime() + e.at * 1000).toISOString() };
              for (var k in e) if (k !== "at") o[k] = e[k];
              return JSON.stringify(o);
            })
            .join("\n"),
        ),
      ),
    );
  }

  function detail() {
    var t = byId(S.sel);
    var col = h("section", { class: "detail-col", "aria-label": "这一条录音" });
    add(col, h("nav", { class: "back" }, h("a", { href: "?", onclick: function (e) { e.preventDefault(); S.sel = ""; render(); } }, icon("back", 14), "录音列表")));
    if (!t) {
      add(col, h("p", { class: "d-empty" }, "选一条录音，在这里看文字、播放和重发。"));
      return col;
    }
    add(col, header(t));
    add(col, textBlock(t));
    add(col, player(t));
    if (t.text) add(col, versions(t));
    add(col, timeline(t));
    add(
      col,
      h(
        "details",
        { class: "more-box" },
        h("summary", null, "文件"),
        h("div", { class: "paths" }, ["~/.local/share/megavoice/utterances/" + t.id + ".wav", "~/.local/share/megavoice/utterances/" + t.id + ".txt", "~/.local/share/megavoice/utterances/" + t.id + ".events.jsonl"].map(function (p) { return h("span", { translate: "no" }, p); })),
      ),
    );
    add(col, h("p", { class: "d-foot" }, h("a", { href: "#" }, "在对比页标注")));
    return col;
  }

  function select(id, focusDetail) {
    if (S.sel !== id) {
      S.sel = id;
      S.pos = 0;
      S.playing = false;
      S.note = null;
      S.picker = "";
      S.tab = "raw";
      var t = byId(id);
      if (t && t.state === "undelivered") t.seen = true;
    }
    render();
    if (focusDetail && !WIDE.matches) window.scrollTo(0, 0);
  }

  // ------------------------------------------------------------ page

  function render() {
    var y = window.scrollY;
    var active = document.activeElement && document.activeElement.dataset ? document.activeElement.dataset.id : null;
    app.textContent = "";
    if (F.name === "refused") {
      add(app, h("main", { class: "page-msg" }, h("h1", null, "从菜单栏重新打开录音历史"), h("p", null, "这个标签页没有 megavoice 给的钥匙，或者钥匙已经换过。点菜单栏的 megavoice 图标，选「打开录音历史」。")));
      return;
    }
    add(app, topbar());
    var main = h("main", { class: "page", "data-view": S.sel ? "detail" : "list", "data-empty": F.takes.length === 0 || F.name === "loading" ? "" : null });
    if (F.name === "loading") {
      add(
        main,
        h(
          "section",
          { class: "list-col", "aria-busy": "true", "aria-label": "录音列表" },
          h("p", { class: "count-line muted" }, "正在载入…"),
          h("ul", { class: "skel", "aria-hidden": "true" }, [0, 1, 2, 3, 4, 5, 6, 7].map(function (i) {
            return h("li", null, h("span"), h("span"), h("span"), h("span"), h("span", { style: "width:" + (40 + ((i * 37) % 50)) + "%" }));
          })),
        ),
      );
    } else if (!F.takes.length) {
      add(main, h("section", { class: "list-col", id: "list" }, h("div", { class: "empty" }, h("h2", null, "还没有录音"), h("p", null, "按右 Option 开始录音，再按一次结束。每一条都会出现在这里，取消的也在。"))));
    } else {
      add(main, list());
      add(main, detail());
    }
    add(app, main);
    if (S.keys) add(app, keysPanel());
    syncUrl();
    window.scrollTo(0, y);
    if (active) {
      var el = document.querySelector('a[data-id="' + active + '"]');
      if (el) el.focus({ preventScroll: true });
    }
  }

  function move(d) {
    var vis = visible();
    var i = vis.findIndex(function (t) { return t.id === S.sel; });
    var n = vis[Math.max(0, Math.min(vis.length - 1, i + d))];
    if (!n) return;
    if (WIDE.matches) select(n.id);
    else {
      S.sel = "";
      render();
    }
    var el = document.querySelector('a[data-id="' + n.id + '"]');
    if (el) {
      el.focus({ preventScroll: true });
      el.scrollIntoView({ block: "nearest" });
    }
  }

  document.addEventListener("keydown", function (e) {
    var tag = (e.target.tagName || "").toLowerCase();
    var typing = tag === "input" || tag === "select" || tag === "textarea";
    if (e.key === "Escape") {
      if (typing && S.q) {
        S.q = "";
        render();
      } else if (S.keys) toggleKeys();
      else if (S.picker) {
        S.picker = "";
        render();
      } else if (!WIDE.matches && S.sel) {
        S.sel = "";
        render();
      }
      return;
    }
    if (typing || e.metaKey || e.ctrlKey || e.altKey) return;
    var t = byId(S.sel);
    switch (e.key) {
      case "/":
        e.preventDefault();
        document.getElementById("q").focus();
        break;
      case "j":
        move(1);
        break;
      case "k":
        move(-1);
        break;
      case "u":
        S.f = S.f === "undelivered" ? "all" : "undelivered";
        render();
        break;
      case "?":
        toggleKeys();
        break;
      case "c":
        if (t && t.text) copy(t.text);
        break;
      case "r":
        if (t && t.text) {
          S.picker = S.picker === "resend" ? "" : "resend";
          render();
        }
        break;
      case "t":
        if (t && t.text) {
          S.picker = S.picker === "retrans" ? "" : "retrans";
          render();
        }
        break;
      case " ":
        if (t && tag !== "button") {
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
    }
  });

  render();
})();
