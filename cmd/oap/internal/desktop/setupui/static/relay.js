// The relay-animated OAP mark, shared by the setup page (index.html) and the
// install-progress page (installing.html). Served by setupui.Server at
// /relay.js; styled by relay.css.
//
// The mark with a comet band running its outline, hue handing over at every
// second corner (motion lab 2026-09-16, pick Y2). The band splits at the
// vertex it is crossing so the new hue grows out of the corner while the old
// drains into it.
//
// Usage: var relay = OAPRelay.mount(hostElement); relay.setWorking(true|false).
// setWorking toggles body[data-working] (which relay.css keys the band's
// visibility on) and honours prefers-reduced-motion by never starting the
// animation when it is set.
(function () {
  "use strict";

  var MARK_OUTLINE = "M159.186 7.59677C164.993 -2.53226 179.604 -2.53226 185.411 7.59677L342.572 281.702C348.349 291.779 341.075 304.335 329.459 304.335H15.138C3.52262 304.335 -3.75273 291.779 2.02468 281.702L159.186 7.59677Z";
  var MARK_HOLE = "M178.855 133.226C175.951 128.161 168.646 128.161 165.742 133.226L117.254 217.794C114.366 222.832 118.003 229.11 123.811 229.11H220.786C226.594 229.11 230.232 222.832 227.343 217.794L178.855 133.226Z";
  var RELAY = { lapMs: 2400, band: 12, handoverEvery: 2, tailSteps: 12, tailLength: 3.2, tailOpacity: 0.55 };
  var RELAY_HUES = [1, 2, 3, 4, 5].map(function (n) { return "var(--relay-" + n + ")"; });

  function buildRelayMark(host) {
    var ns = "http://www.w3.org/2000/svg";
    var svg = document.createElementNS(ns, "svg");
    svg.setAttribute("viewBox", "0 0 345 305");
    svg.setAttribute("class", "mark working");
    svg.setAttribute("aria-hidden", "true");
    var ink = document.createElementNS(ns, "path");
    ink.setAttribute("class", "ink");
    ink.setAttribute("d", MARK_OUTLINE + MARK_HOLE);
    svg.appendChild(ink);
    var band = document.createElementNS(ns, "g");
    band.setAttribute("class", "band");
    var segs = { t: [] };
    for (var k = 0; k < RELAY.tailSteps; k++) segs.t.push(relaySeg(ns, band, "seg t"));
    segs.a = relaySeg(ns, band, "seg a");
    segs.b = relaySeg(ns, band, "seg b");
    svg.appendChild(band);
    host.appendChild(svg);
    return segs;
  }

  function relaySeg(ns, parent, cls) {
    var p = document.createElementNS(ns, "path");
    p.setAttribute("class", cls);
    p.setAttribute("d", MARK_OUTLINE);
    p.setAttribute("pathLength", "100");
    parent.appendChild(p);
    return p;
  }

  function relayDash(el, start, len, color) {
    if (len <= 0.05) { el.style.opacity = "0"; return; }
    el.style.opacity = "1";
    el.style.stroke = color;
    el.style.strokeDasharray = len + " " + (100 - len);
    el.style.strokeDashoffset = String(-start);
  }

  // drawFrame renders the band at time `now` for a lap that started at t0.
  function drawFrame(segs, now, t0) {
    var lap = (now - t0) / RELAY.lapMs;
    var head = (lap * 100) % 100;
    var tail = (head - RELAY.band + 100) % 100;
    var crossings = Math.floor(lap * 3);
    var hueIndex = Math.floor(crossings / RELAY.handoverEvery);
    var hueNew = RELAY_HUES[hueIndex % RELAY_HUES.length];
    var hueOld = RELAY_HUES[(hueIndex - 1 + RELAY_HUES.length) % RELAY_HUES.length];
    var lastCross = ((crossings / 3) * 100) % 100;
    var sinceCross = (head - lastCross + 100) % 100;
    var splitting = crossings % RELAY.handoverEvery === 0 && sinceCross < RELAY.band;
    if (splitting) {
      relayDash(segs.a, tail, RELAY.band - sinceCross, hueOld);
      relayDash(segs.b, lastCross, sinceCross, hueNew);
    } else {
      relayDash(segs.a, tail, RELAY.band, hueNew);
      relayDash(segs.b, 0, 0, hueNew);
    }
    var tailHue = splitting ? hueOld : hueNew;
    var step = (RELAY.band * RELAY.tailLength) / RELAY.tailSteps;
    for (var k = 0; k < RELAY.tailSteps; k++) {
      relayDash(segs.t[k], (tail - step * (k + 1) + 100) % 100, step + 0.15, tailHue);
      segs.t[k].style.opacity = (RELAY.tailOpacity * Math.pow(1 - k / RELAY.tailSteps, 1.6)).toFixed(3);
    }
  }

  // mount builds the mark into host and returns its controller.
  function mount(host) {
    var segs = buildRelayMark(host);
    var raf = 0, t0 = 0;
    function tick(now) {
      drawFrame(segs, now, t0);
      raf = requestAnimationFrame(tick);
    }
    return {
      setWorking: function (on) {
        var reduced = window.matchMedia && window.matchMedia("(prefers-reduced-motion: reduce)").matches;
        if (on && !reduced) {
          if (!document.body.hasAttribute("data-working")) {
            document.body.setAttribute("data-working", "");
            t0 = performance.now();
            raf = requestAnimationFrame(tick);
          }
        } else {
          document.body.removeAttribute("data-working");
          if (raf) { cancelAnimationFrame(raf); raf = 0; }
        }
      }
    };
  }

  window.OAPRelay = { mount: mount };
})();
