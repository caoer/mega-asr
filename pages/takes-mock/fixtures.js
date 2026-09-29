// A fabricated take store for the Takes page mock. Every text is invented;
// none is a take anyone spoke. The shapes follow the Takes API: a take list, a
// take's detail with its versions and events, the targets and the engines.
(function () {
  "use strict";

  function rng(seed) {
    return function () {
      seed |= 0;
      seed = (seed + 0x6d2b79f5) | 0;
      var t = Math.imul(seed ^ (seed >>> 15), 1 | seed);
      t = (t + Math.imul(t ^ (t >>> 7), 61 | t)) ^ t;
      return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
    };
  }
  var r = rng(20200311);
  function pick(a) {
    return a[Math.floor(r() * a.length)];
  }

  var SENTENCES = [
    "sourdough 的酵头昨晚忘了喂，今天面团发得很慢。",
    "图书馆那本讲鸟类迁徙的书下周三到期，记得续借。",
    "Check whether the derailleur cable needs replacing before the long ride.",
    "阳台上的罗勒又被虫咬了几片叶子。",
    "The forecast says rain after four, so bring the laundry in early.",
    "这周末的路线大概四十公里，中途在水库边歇一下。",
    "烤箱预热到两百二十摄氏度，先放一盘水进去制造蒸汽。",
    "Ask the neighbour if they still want the spare tomato seedlings.",
    "猫粮快吃完了，顺便买一袋猫砂。",
    "Proofing the sourdough overnight in the fridge gives a more sour loaf.",
    "旧自行车的链条锈得厉害，干脆整条换掉。",
    "明早六点出门看日出，闹钟定五点二十。",
    "The library closes early on public holidays.",
    "鱼缸的过滤棉该洗了，水有点浑。",
    "Twelve kilometres uphill is harder than it looks on the map.",
    "面包出炉后至少晾一个小时再切。",
  ];

  // The raw text is the engine's answer before corrections: no punctuation,
  // one term heard wrong.
  function rawOf(t) {
    return t
      .replace(/[，。、,.：]/g, " ")
      .replace("sourdough", "sour doe")
      .replace("derailleur", "the rail ear")
      .replace(/\s+/g, " ")
      .trim();
  }
  function doubaoOf(t) {
    return t.replace("公里", "km").replace("摄氏度", "°C");
  }

  var PANES = [
    { id: "t1.0", ws: "ws-a", wsName: "lighthouse", proc: "nvim", folder: "/srv/lighthouse" },
    { id: "t2.0", ws: "ws-b", wsName: "quilt", proc: "zsh", folder: "~/quilt" },
    { id: "t2.1", ws: "ws-b", wsName: "quilt", proc: "claude", folder: "~/quilt/api" },
    { id: "t2.2", ws: "ws-b", wsName: "quilt", proc: "kimi", folder: "~/quilt/web" },
    { id: "t5.0", ws: "ws-e", wsName: "orchard", proc: "claude", folder: "/opt/orchard" },
  ];
  var APPS = [
    { pid: 3120, app: "TextEdit", title: "shopping list.txt" },
    { pid: 5877, app: "Finder", title: "Downloads" },
  ];
  function paneTarget(p) {
    return { kind: "pane", short: p.proc + " › " + p.folder.split("/").pop(), full: p.ws + " " + p.wsName + " › " + p.proc + " · " + p.folder + " · " + p.id, pane: p.id };
  }
  function appTarget(a) {
    return { kind: "app", short: a.app, full: a.app + " — " + a.title, pid: a.pid };
  }
  var TARGETS = PANES.map(paneTarget).concat(APPS.map(appTarget));
  var DEVICES = ["Stage Interface", "Built-in Microphone", "Podium Condenser"];

  function peaks(seed, dur) {
    var g = rng(seed);
    var n = Math.max(40, Math.min(160, Math.round(dur * 4)));
    var out = [];
    var env = 0.4;
    for (var i = 0; i < n; i++) {
      env += (g() - 0.5) * 0.35;
      env = Math.max(0.12, Math.min(1, env));
      var pause = g() < 0.08 ? 0.15 : 1;
      out.push(Math.max(0.06, Math.min(1, env * pause * (0.55 + g() * 0.45))));
    }
    return out;
  }

  var NOW = new Date(2020, 7, 18, 16, 38, 25);
  function stamp(d) {
    function p(n) {
      return String(n).padStart(2, "0");
    }
    return d.getFullYear() + p(d.getMonth() + 1) + p(d.getDate()) + "-" + p(d.getHours()) + p(d.getMinutes()) + p(d.getSeconds());
  }

  function build(n) {
    var takes = [];
    var t = new Date(NOW.getTime() - 95 * 1000);
    for (var i = 0; i < n; i++) {
      var roll = r();
      var dur = roll < 0.1 ? 0.4 + r() * 1.2 : roll < 0.85 ? 3 + r() * 30 : 45 + r() * 120;
      var text = pick(SENTENCES);
      if (dur > 60) text = text + pick(SENTENCES) + pick(SENTENCES) + pick(SENTENCES) + pick(SENTENCES);
      var target = r() < 0.6 ? TARGETS[Math.floor(r() * PANES.length)] : TARGETS[PANES.length + Math.floor(r() * APPS.length)];
      var s = r();
      var state = s < 0.62 ? "sent" : s < 0.84 ? "pasted" : s < 0.96 ? "cancelled" : "empty";
      if (dur < 1) state = "empty";
      var take = {
        id: stamp(t),
        at: new Date(t.getTime()),
        dur: dur,
        state: state,
        reason: null,
        target: target,
        device: pick(DEVICES),
        engine: "funasr",
        text: state === "empty" ? "" : text,
        raw: state === "empty" ? "" : rawOf(text),
        versions: [],
        retrans: [],
        seen: true,
      };
      if (state !== "empty" && r() < 0.35) take.versions.push({ name: "doubao", cloud: true, text: doubaoOf(text) });
      takes.push(take);
      t = new Date(t.getTime() - (60 + r() * 900) * 1000);
      // Nights are quiet.
      if (t.getHours() < 8) t = new Date(t.getFullYear(), t.getMonth(), t.getDate() - 1, 23, Math.floor(r() * 50), Math.floor(r() * 60));
    }
    // Two undelivered takes, one of them unseen and long.
    takes[2].state = "undelivered";
    takes[2].reason = "deliver_failed";
    takes[2].seen = false;
    takes[2].dur = 87.6;
    takes[2].text = "三件事，按顺序来：\n" + SENTENCES[6] + SENTENCES[11] + SENTENCES[0];
    takes[2].raw = rawOf(takes[2].text);
    takes[2].target = TARGETS[1];
    takes[2].versions = [{ name: "doubao", cloud: true, text: doubaoOf(takes[2].text) }];
    takes[2].retrans = [{ n: 1, engine: "sensevoice", err: "找不到 sensevoice 的模型目录，请先在设置里下载" }];
    takes[31].state = "undelivered";
    takes[31].reason = "interrupted";
    takes[31].dur = 312.9;
    takes[31].text = SENTENCES[9] + SENTENCES[2] + SENTENCES[14] + SENTENCES[5];
    takes[31].raw = rawOf(takes[31].text);
    // A take the local engine heard wrong; only Doubao's answer holds the term.
    takes[8].state = "sent";
    takes[8].text = "后面那个的 rail 了还是咔咔响，周末送去车行。";
    takes[8].raw = "后面那个的 rail 了还是咔咔响 周末送去车行";
    takes[8].versions = [{ name: "doubao", cloud: true, text: "后面那个 derailleur 还是咔咔响，周末送去车行。" }];
    // Three takes from before the record existed.
    for (var j = n - 3; j < n; j++) {
      takes[j].state = "legacy";
      if (!takes[j].text) {
        takes[j].text = SENTENCES[j % SENTENCES.length];
        takes[j].raw = rawOf(takes[j].text);
      }
    }
    takes.forEach(function (k, idx) {
      k.peaks = peaks(idx + 7, k.dur);
      k.events = eventsOf(k);
    });
    return takes;
  }

  function eventsOf(k) {
    var ev = [];
    if (k.state === "legacy") return ev;
    ev.push({ at: 0, ev: "start", target: k.target.short, trigger: "tap" });
    ev.push({ at: 0.2, ev: "mic_live" });
    var stopKind = k.state === "cancelled" ? "cancel" : k.reason === "interrupted" ? "shutdown" : k.state === "sent" && k.dur > 20 ? "enter" : "tap";
    ev.push({ at: k.dur, ev: "stop", kind: stopKind });
    if (k.state === "empty") {
      ev.push({ at: k.dur + 0.3, ev: "hold", why: "empty" });
      return ev;
    }
    ev.push({ at: k.dur + 0.8, ev: "text", engine: k.engine, chars: k.text.length, latency_ms: 780 });
    if (k.state === "sent" || k.state === "pasted") ev.push({ at: k.dur + 0.9, ev: "deliver", via: "auto", ok: true, submit: k.state === "sent" ? "ok" : "none", target: k.target.short, received: "whole" });
    if (k.state === "cancelled") ev.push({ at: k.dur + 0.8, ev: "hold", why: "cancelled" });
    if (k.reason === "deliver_failed") {
      ev.push({ at: k.dur + 0.9, ev: "deliver", via: "auto", ok: false, err: "面板已关闭", target: k.target.short });
      ev.push({ at: k.dur + 0.9, ev: "hold", why: "deliver_failed" });
      ev.push({ at: k.dur + 211, ev: "retranscribe", n: 1, engine: "sensevoice", err: "模型文件不存在" });
    }
    if (k.reason === "interrupted") ev.push({ at: k.dur, ev: "hold", why: "interrupted" });
    return ev;
  }

  var ENGINES = [
    { name: "funasr", local: true, current: true },
    { name: "sensevoice", local: true },
    { name: "whisper-large-v3", local: true },
    { name: "doubao", local: false, vendor: "火山引擎" },
  ];

  var params = new URLSearchParams(location.search);
  var fixture = params.get("state") || "list";
  var takes = fixture === "empty" ? [] : build(160);
  if (fixture === "recording") {
    takes.unshift({
      id: stamp(new Date(NOW.getTime() - 23000)),
      at: new Date(NOW.getTime() - 23000),
      dur: 23,
      state: "recording",
      target: TARGETS[0],
      device: "Podium Condenser",
      engine: "funasr",
      text: "",
      raw: "",
      versions: [],
      retrans: [],
      peaks: [],
      events: [{ at: 0, ev: "start", target: TARGETS[0].short, trigger: "tap" }, { at: 0.2, ev: "mic_live" }],
      seen: true,
    });
  }

  window.FIXTURE = {
    name: fixture,
    now: NOW,
    takes: takes,
    targets: {
      herdr: fixture !== "no-herdr",
      panes: PANES.map(paneTarget),
      apps: APPS.map(appTarget),
      recent: [TARGETS[3], TARGETS[PANES.length + 1]],
    },
    engines: ENGINES,
  };
})();
