// The iPhone pairing page: a QR code a phone scans to pair with this Mac,
// and the paired phones with a revoke each. The key comes as on the Takes
// page: #k=<key> once, then the tab's sessionStorage, sent as
// X-Megavoice-Key on every request under /takes/api/remote.
"use strict";
(function () {
  // The page's language, fixed at load: Chinese when the html's lang (which
  // lang.js sets from megavoice's language) is zh, English otherwise. tr is
  // every word the page shows, in both: tr(zh, en).
  var ZH = /^zh/i.test(document.documentElement.lang || "");
  function tr(zh, en) {
    return ZH ? zh : en;
  }
  // the title; the title element precedes this script
  document.title = tr("iPhone 遥控 · megavoice", "iPhone Remote · megavoice");
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

  var hashKey = new URLSearchParams(location.hash.slice(1)).get("k");
  if (hashKey) {
    sessionStorage.setItem("megavoice-key", hashKey);
    history.replaceState(null, "", location.pathname + location.search);
  }
  var stopped = false;

  function api(path, body) {
    if (stopped) return Promise.reject(new Error("refused"));
    var init = {
      cache: "no-store",
      headers: { "X-Megavoice-Key": sessionStorage.getItem("megavoice-key") || "" },
    };
    if (body !== undefined) {
      init.method = "POST";
      init.headers["Content-Type"] = "application/json";
      init.body = JSON.stringify(body);
    }
    return fetch("/takes/api/remote" + path, init).then(function (r) {
      if (r.status === 403) {
        stopped = true;
        sessionStorage.removeItem("megavoice-key");
        throw new Error(tr("这个页面的钥匙已失效：从菜单栏 › iPhone 遥控… 重新打开", "This page's key is no longer valid: reopen it from the megavoice menu in the menu bar"));
      }
      return r.json().then(function (j) {
        if (!r.ok) throw new Error(j.error || r.status + "");
        return j;
      });
    });
  }

  function $(id) {
    return document.getElementById(id);
  }

  function when(t) {
    if (!t || t.indexOf("0001-") === 0) return tr("从未连接", "never");
    var d = new Date(t);
    return d.toLocaleString(ZH ? "zh-CN" : "en-US", { hour12: false });
  }

  function drawQR(qr) {
    var c = $("pair-qr");
    var quiet = 4, scale = 8, n = qr.size + 2 * quiet;
    c.width = c.height = n * scale;
    var g = c.getContext("2d");
    g.fillStyle = "#fff";
    g.fillRect(0, 0, c.width, c.height);
    g.fillStyle = "#000";
    for (var y = 0; y < qr.size; y++)
      for (var x = 0; x < qr.size; x++)
        if (qr.bits.charAt(y * qr.size + x) === "1") g.fillRect((x + quiet) * scale, (y + quiet) * scale, scale, scale);
  }

  var shownURL = "";
  var expires = 0;

  function render(v) {
    var why = $("pair-why");
    why.hidden = !v.why;
    why.textContent = v.why || "";
    $("pair-mac").hidden = !v.enabled;
    if (!v.enabled) return;
    $("pair-name").textContent = tr("这台 Mac：", "This Mac: ") + v.name + " · " + v.addr + tr(" · 证书 ", " · certificate ") + v.fp.slice(0, 16) + "…";
    var adv = $("pair-advertise");
    adv.hidden = !v.advertise;
    adv.textContent = v.advertise ? tr("iPhone 在网络上找不到这台 Mac（" + v.advertise + "）；扫码配对后 iPhone 仍按二维码里的地址连接。", "iPhones can't find this Mac on the network (" + v.advertise + "); once paired by QR code, the iPhone still connects to the address in the code.") : "";
    var rl = $("pair-relay");
    var r = v.relay || {};
    rl.hidden = !r.enabled;
    rl.textContent = !r.enabled ? "" : r.online ? tr("中继：在线 · 房间 ", "Relay: online · room ") + r.room_prefix + "…" : tr("中继：离线", "Relay: offline") + (r.why ? tr("（" + r.why + "）", " (" + r.why + ")") : "");
    var p = v.pairing;
    $("pair-code").hidden = !p;
    if (p) {
      expires = Date.parse(p.expires);
      if (p.url !== shownURL) {
        shownURL = p.url;
        drawQR(p.qr);
        $("pair-url").value = p.url;
      }
    } else {
      shownURL = "";
      expires = 0;
    }
    tick();
    var ul = $("pair-phones");
    ul.textContent = "";
    if (!v.phones.length) {
      var none = document.createElement("li");
      none.textContent = tr("还没有配对的 iPhone", "No paired iPhones yet");
      ul.appendChild(none);
    }
    v.phones.forEach(function (ph) {
      var li = document.createElement("li");
      var left = document.createElement("div");
      var name = document.createElement("div");
      var paths = [];
      if (ph.direct) paths.push(tr("直连", "direct"));
      if (ph.relay) paths.push(tr("中继", "relay"));
      name.textContent = ph.name + (ph.online ? tr(" · 在线（" + paths.join("、") + "）", " · online (" + paths.join(", ") + ")") : "") + (ph.watch ? tr(" · 手表已登记", " · Watch registered") : "");
      var meta = document.createElement("div");
      meta.className = "meta";
      meta.textContent = tr("配对于 ", "Paired ") + when(ph.paired) + tr(" · 最近连接 ", " · last connected ") + when(ph.last_seen) +
        (r.enabled && !ph.relay_key ? tr(" · 没有中继钥匙：重新扫码配对后才能经中继连接", " · no relay key: pair again by QR code to connect through the relay") : "");
      left.appendChild(name);
      left.appendChild(meta);
      var b = document.createElement("button");
      b.type = "button";
      b.textContent = tr("取消配对", "Unpair");
      b.addEventListener("click", function () {
        if (!confirm(tr("取消「" + ph.name + "」的配对？它之后的每次按下都会被拒绝，要重新扫码。", "Unpair “" + ph.name + "”? Every press from it will be refused until it scans a new code."))) return;
        api("/revoke", { name: ph.name }).then(render, fail);
      });
      li.appendChild(left);
      li.appendChild(b);
      ul.appendChild(li);
    });
  }

  function tick() {
    if (!expires) return;
    var left = Math.max(0, Math.round((expires - Date.now()) / 1000));
    $("pair-left").textContent = left > 0 ? tr("还剩 " + left + " 秒有效，只能配对一台", "Valid for " + left + " more s · pairs one iPhone only") : tr("已过期：重新生成", "Expired: generate a new one");
  }

  function fail(e) {
    var why = $("pair-why");
    why.hidden = false;
    why.textContent = e.message;
  }

  function refresh() {
    if (!stopped && !document.hidden) api("").then(render, fail);
  }

  addEventListener("DOMContentLoaded", function () {
    localize();
    $("pair-new").addEventListener("click", function () {
      api("/pair", {}).then(render, fail);
    });
    $("pair-copy").addEventListener("click", function () {
      var b = $("pair-copy");
      navigator.clipboard.writeText($("pair-url").value).then(
        function () {
          b.textContent = tr("已复制", "Copied");
          setTimeout(function () { b.textContent = tr("复制链接", "Copy link"); }, 2000);
        },
        function () {
          $("pair-url").select();
          b.textContent = tr("选中了，按 ⌘C", "Selected: press ⌘C");
        }
      );
    });
    refresh();
    setInterval(refresh, 2000);
    setInterval(tick, 1000);
  });
})();
