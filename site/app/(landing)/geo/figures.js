/* Hand-authored illustrations, not captured product media. Per the site's
 * real-not-drift rule (site/AGENTS.md): these figures are drawn, not recorded,
 * and are ported verbatim from the landing-page design study. Keep the
 * geometry faithful to that study; GeoFigure.tsx is the only consumer. */

/* Geometric concept figures for the landing page.
 *
 * Ported from the design-lab studies (claudes-fort scratch/design-lab/oap/geometric).
 * The vocabulary is the logo: an O is an identity (a hex hole marks an agent),
 * an A is authority and the gates it guards, a P is a session. One accent per
 * figure, --lp-state, and it marks the answer. Everything else is stone line
 * work at two weights; dashed lines are axes and references, never content.
 *
 * Each figure is { svg(): string, anim?(svg, t), t0? }. svg() is deterministic
 * so it can be server-rendered; anim() mutates the live SVG for clock t.
 * Gradient fills go in style=, because the line classes set fill:none and CSS
 * beats a fill= attribute. Plain JS (allowJs, not type-checked) to keep the
 * geometry readable; GeoFigure.tsx is the typed boundary. */

const S = (body, defs = "") =>
  `<svg viewBox="0 0 400 400" xmlns="http://www.w3.org/2000/svg" aria-hidden="true"><defs>${defs}</defs>${body}</svg>`;
const lin = (id, x1, y1, x2, y2, o1 = 0, o2 = 1) =>
  `<linearGradient id="${id}" x1="${x1}" y1="${y1}" x2="${x2}" y2="${y2}"><stop offset="0" style="stop-color:var(--accent);stop-opacity:${o1}"/><stop offset="1" style="stop-color:var(--accent);stop-opacity:${o2}"/></linearGradient>`;
const rad = (id, o = 0.5) =>
  `<radialGradient id="${id}"><stop offset="0" style="stop-color:var(--accent);stop-opacity:${o}"/><stop offset="1" style="stop-color:var(--accent);stop-opacity:0"/></radialGradient>`;
const hatch = (id) =>
  `<pattern id="${id}" width="6" height="6" patternUnits="userSpaceOnUse" patternTransform="rotate(45)"><line x1="0" y1="0" x2="0" y2="6" style="stroke:var(--hatch, var(--ink-faint))" stroke-width="1"/></pattern>`;
const T = (x, y, s, cls = "", anchor = "start") =>
  `<text class="lbl ${cls}" x="${x}" y="${y}" text-anchor="${anchor}">${s}</text>`;
const Tk = (k, x, y, s, cls = "", anchor = "start", op = 1) =>
  `<text data-k="${k}" class="lbl ${cls}" x="${x}" y="${y}" text-anchor="${anchor}" opacity="${op}">${s}</text>`;
const lerp = (a, b, t) => [a[0] + (b[0] - a[0]) * t, a[1] + (b[1] - a[1]) * t];
const P = (p) => p.map((n) => n.toFixed(1)).join(",");
const poly = (pts) => pts.map(P).join(" ");
const clamp = (x, a = 0, b = 1) => Math.max(a, Math.min(b, x));
const sm = (x) => {
  x = clamp(x);
  return x * x * (3 - 2 * x);
};
const q = (r, k) => r.querySelector(`[data-k="${k}"]`);
const ACC = "var(--accent)";
/* The relay, from the logo's motion: a comet band runs the A's outline and the
   hue hands over. Here each thing that passes through one A (a user, a tool
   call, a channel, a session) takes the next hue, starting from the figure's
   own. hueAt sets --accent on the svg, so lines, fills and labels follow. */
const hueAt = (r, k) => {
  if (r._b === undefined) r._b = Number(r.parentElement && r.parentElement.dataset.hue) || 0;
  const v = `var(--h${(r._b + k) % 5})`;
  if (r._hv !== v) {
    r._hv = v;
    r.style.setProperty("--accent", v);
  }
};
const TAIL = 4;
const band = (k, d) =>
  Array.from({ length: TAIL + 1 }, (_, j) => `<path data-k="${k}${j}" class="a" d="${d}" style="stroke-width:2.4" opacity="0"/>`).join("");
// Head at p laps round the outline; the tail is short dashes fading behind it.
const runBand = (r, k, p, op) => {
  for (let j = 0; j <= TAIL; j++) {
    const el = q(r, k + j), L = len(el), w = L * (j ? 0.045 : 0.12);
    const end = p * L - (j ? L * (0.12 + (j - 1) * 0.045) : 0);
    el.style.strokeDasharray = `${w} ${L - w}`;
    el.style.strokeDashoffset = w - end;
    el.setAttribute("opacity", op <= 0 ? 0 : op * (j ? 0.55 * Math.pow(1 - (j - 1) / TAIL, 1.6) : 1));
  }
};
const len = (el) => el._L || (el._L = el.getTotalLength()); // geometry of these paths never changes
const setText = (el, s) => {
  if (el._s !== s) {
    el._s = s;
    el.textContent = s;
  }
};

// The three primitives, straight from the logo.
const hex = (cx, cy, r) =>
  [0, 1, 2, 3, 4, 5].map((k) => {
    const a = ((-90 + 60 * k) * Math.PI) / 180;
    return [cx + r * Math.cos(a), cy + r * Math.sin(a)];
  });
// The OAP icon itself (the A with its cut-out), for places a figure names the platform.
const MARK = "M159.186 7.59677C164.993 -2.53226 179.604 -2.53226 185.411 7.59677L342.572 281.702C348.349 291.779 341.075 304.335 329.459 304.335H15.138C3.52262 304.335 -3.75273 291.779 2.02468 281.702L159.186 7.59677ZM178.855 133.226C175.951 128.161 168.646 128.161 165.742 133.226L117.254 217.794C114.366 222.832 118.003 229.11 123.811 229.11H220.786C226.594 229.11 230.232 222.832 227.343 217.794L178.855 133.226Z";
/* Every triangle in the figures is this exact shape: the logo's outer path and
   its cut-out, refit so the apex sits at base - h. 304.335 is the mark's height,
   172.3 its apex x. */
const [MARK_OUT, MARK_IN] = MARK.split("Z").filter(Boolean).map((d) => d + "Z");
const fitMark = (d, cx, base, h) => {
  const k = h / 304.335, ox = cx - 172.3 * k, oy = base - 304.335 * k;
  return d.replace(/([MLCH])([^MLCHZ]+)/g, (_, c, a) => {
    const n = a.trim().split(/\s+/).map(Number);
    return c + n.map((v, i) => (c !== "H" && i % 2 ? oy + v * k : ox + v * k).toFixed(1)).join(" ");
  });
};
const triD = (cx, base, h) => fitMark(MARK_OUT, cx, base, h);
const holeD = (cx, base, h) => fitMark(MARK_IN, cx, base, h);
const mark = (x, y, w) => `<path d="${MARK}" fill-rule="evenodd" transform="translate(${x} ${y}) scale(${(w / 345).toFixed(4)})" style="fill:var(--ink)"/>`;
const agent = (cx, cy, r) =>
  `<circle class="i" cx="${cx}" cy="${cy}" r="${r}" style="fill:var(--card)"/><polygon class="f" points="${poly(hex(cx, cy, r * 0.45))}"/>`;
const rh = (cx, y, w, k = 0.5) => ({ L: [cx - w, y], T: [cx, y - w * k], R: [cx + w, y], B: [cx, y + w * k] });
const poly4 = (g) => [g.L, g.T, g.R, g.B].map(P).join(" ");

const FIGS = {};

/* Problem: one agent holding everyone's access, each call needing one slice. */
{
  const D = 76, R = 46, users = ["maya", "jon", "priya", "sam", "leo"];
  const at = (k, d = D) => {
    const a = ((-90 + k * 72) * Math.PI) / 180;
    return [200 + d * Math.cos(a), 200 + d * Math.sin(a)];
  };
  FIGS.union = {
    t0: 0.9,
    svg: () => {
      let b = "";
      users.forEach((u, k) => {
        const [x, y] = at(k);
        b += `<circle cx="${x}" cy="${y}" r="${R}" style="fill:url(#geo-h-union)"/>`;
      });
      users.forEach((u, k) => {
        const [x, y] = at(k), [lx, ly] = at(k, D + R + 16);
        b += `<circle data-k="uf${k}" cx="${x}" cy="${y}" r="${R}" style="fill:url(#geo-g-union)" opacity="0"/><circle data-k="u${k}" class="i" cx="${x}" cy="${y}" r="${R}"/>` + T(lx, ly + 4, u, "", "middle");
      });
      b += `<line data-k="ln" class="a" x1="200" y1="200" x2="200" y2="200"/>` + agent(200, 200, 13);
      b += T(20, 30, "agent holds ∪ users") + Tk("need", 200, 366, "this call needs user:maya", "ac", "middle");
      return S(b, hatch("geo-h-union") + rad("geo-g-union", 0.75));
    },
    anim: (r, t) => {
      const n = Math.floor(t / 1.8) % 5, f = (t % 1.8) / 1.8, env = clamp(f / 0.2) * (1 - clamp((f - 0.8) / 0.2));
      hueAt(r, n);
      users.forEach((_, k) => {
        q(r, "uf" + k).setAttribute("opacity", k === n ? env : 0);
        q(r, "u" + k).style.stroke = k === n && env > 0.3 ? ACC : "";
      });
      const [x, y] = at(n), ln = q(r, "ln"), dr = sm(f / 0.3);
      ln.setAttribute("x1", x); ln.setAttribute("y1", y);
      ln.setAttribute("x2", x + (200 - x) * dr); ln.setAttribute("y2", y + (200 - y) * dr);
      ln.setAttribute("opacity", env);
      setText(q(r, "need"), "this call needs user:" + users[n]);
    },
  };
}

/* Define: an AgentClass in git stamps out sessions, each with its own pod, torn down when done.
   The class is the OAP triangle; each session is a smaller triangle cut from it. */
{
  const xs = [90, 200, 310], base = 300, sh = 70;
  FIGS.define = {
    t0: 1.2,
    svg: () => {
      let b = "";
      xs.forEach((x, k) => (b += `<line data-k="ln${k}" class="f" x1="200" y1="160" x2="${x}" y2="${base - sh}"/>`));
      b += `<path class="i" d="${triD(200, 160, 100)}" style="fill:var(--card)"/><path class="f" d="${holeD(200, 160, 100)}"/>` + band("rb", triD(200, 160, 100));
      xs.forEach((x, k) => {
        const d = triD(x, base, sh);
        b += `<path data-k="sf${k}" d="${d}" style="fill:url(#geo-g-define)" opacity="0"/><path data-k="s${k}" class="i" d="${d}"/><path class="f" d="${holeD(x, base, sh)}"/>`;
      });
      b += T(250, 122, "agentclass") + T(250, 140, "in git") + Tk("lbl", 200, 326, "session", "ac", "middle");
      b += T(200, 370, "own pod · budget · memory", "", "middle");
      return S(b, lin("geo-g-define", 0, 0, 0, 1, 0.7, 0.05));
    },
    anim: (r, t) => {
      const p = (t % 6.6) / 6.6, k = Math.floor(p * 3), f = p * 3 - k;
      hueAt(r, k);
      runBand(r, "rb", sm(f / 0.42), clamp(f / 0.04) * (1 - clamp((f - 0.36) / 0.08)));
      xs.forEach((x, j) => {
        const s = q(r, "s" + j), on = j === k, L = len(s);
        const live = on ? 1 - clamp((f - 0.78) / 0.18) : 0;
        s.style.strokeDasharray = on ? L : "";
        s.style.strokeDashoffset = on ? L * (1 - sm(f / 0.3)) : "";
        s.style.stroke = on && live > 0.4 ? ACC : "";
        s.style.opacity = on ? 1 : 0.35;
        q(r, "sf" + j).setAttribute("opacity", on ? clamp((f - 0.2) / 0.15) * live : 0);
        q(r, "ln" + j).style.stroke = on && f < 0.4 ? ACC : "";
      });
      const l = q(r, "lbl");
      l.setAttribute("x", xs[k]);
      l.setAttribute("opacity", 1 - clamp((f - 0.78) / 0.18));
      setText(l, f > 0.8 ? "torn down" : "session " + (k + 1));
    },
  };
}

/* Constrain / Decide: every tool call passes through the A. Allowed calls take
   the figure's accent and reach the tool; denied ones turn red and stop at an
   X on the way out. */
{
  const DENY = "var(--deny)";
  const calls = [["git clone", 1], ["gh pr view", 1], ["git push", 0], ["gh pr checks", 1], ["gh pr comment", 0], ["gh api", 1]];
  FIGS.gate = {
    t0: 1.4,
    svg: () => {
      let b = `<line class="d" x1="30" y1="200" x2="370" y2="200"/>`;
      b += `<line data-k="out" class="a" x1="214" y1="200" x2="214" y2="200"/>`;
      b += `<path class="i" d="${triD(200, 250, 128)}" style="fill:var(--card)"/><line class="d" x1="190" y1="200" x2="210" y2="200"/><path data-k="hole" class="i" d="${holeD(200, 250, 128)}"/>` + band("rb", triD(200, 250, 128));
      b += `<rect data-k="tool" class="dot" x="334" y="194" width="12" height="12"/>` + agent(60, 200, 15);
      b += `<path data-k="x" d="M264 192l16 16M280 192l-16 16" style="stroke:${DENY};stroke-width:2;stroke-linecap:round;fill:none" opacity="0"/>`;
      b += `<circle data-k="m" class="pf" r="4.5" cx="-10" cy="-10"/>`;
      b += Tk("nm", 200, 104, "git clone", "", "middle") + Tk("vd", 200, 278, "", "", "middle");
      b += T(60, 234, "agent", "", "middle") + T(340, 230, "tool", "", "middle") + T(200, 360, "checked, then run", "", "middle");
      return S(b);
    },
    anim: (r, t) => {
      const n = Math.floor(t / 1.9) % calls.length, f = (t % 1.9) / 1.9, [nm, ok] = calls[n];
      const no = !ok && f >= 0.42 && f < 0.95;
      runBand(r, "rb", sm((f - 0.3) / 0.42), ok ? clamp((f - 0.3) / 0.05) * (1 - clamp((f - 0.66) / 0.08)) : 0);
      const m = q(r, "m"), out = q(r, "out"), vd = q(r, "vd");
      setText(q(r, "nm"), nm);
      setText(vd, f >= 0.42 ? (ok ? "allow" : "deny") : "");
      vd.setAttribute("class", "lbl" + (ok ? " ac" : ""));
      vd.style.fill = ok ? "" : DENY;
      q(r, "x").setAttribute("opacity", no ? clamp((f - 0.45) / 0.06) * (1 - clamp((f - 0.88) / 0.07)) : 0);
      let x = 200, y = 200, op = 1;
      if (f < 0.4) x = 76 + 124 * sm(f / 0.4);
      else if (f >= 0.55) {
        if (ok) x = 200 + 134 * sm((f - 0.55) / 0.3);
        else { y = 200 + 48 * sm((f - 0.55) / 0.3); op = 1 - clamp((f - 0.55) / 0.3); }
      }
      if (f > 0.9) op = Math.min(op, 1 - (f - 0.9) / 0.1);
      m.setAttribute("cx", x); m.setAttribute("cy", y); m.setAttribute("opacity", op);
      m.style.fill = f >= 0.42 ? (ok ? ACC : DENY) : "";
      q(r, "hole").style.stroke = f >= 0.42 && f < 0.95 ? (ok ? ACC : DENY) : "";
      out.setAttribute("x2", ok && f >= 0.55 ? Math.max(214, x) : 214);
      out.setAttribute("opacity", ok ? 1 - clamp((f - 0.9) / 0.1) : 0);
      const tl = q(r, "tool");
      tl.style.stroke = tl.style.fill = ok && f > 0.84 && f < 0.97 ? ACC : "";
    },
  };
}

/* Broker: the key goes from the broker straight to the tool at the moment of use. */
{
  const ag = [84, 150], tl = [312, 150], br = [200, 318], kD = `M200,268 C218,200 292,214 308,160`;
  FIGS.broker = {
    t0: 1.7,
    svg: () => {
      let b = `<line class="d" x1="200" y1="40" x2="200" y2="360"/>`;
      b += `<line class="f" x1="${ag[0]}" y1="${ag[1]}" x2="${tl[0]}" y2="${tl[1]}"/><line class="f" x1="${br[0]}" y1="${br[1] - 40}" x2="${ag[0]}" y2="${ag[1]}"/>`;
      b += `<path data-k="key" class="a" d="${kD}"/>`;
      b += `<path class="i" d="${triD(br[0], br[1], 54)}" style="fill:var(--card)"/><path class="f" d="${holeD(br[0], br[1], 54)}"/>`;
      b += agent(ag[0], ag[1], 22) + `<circle class="dot" cx="${ag[0] + 30}" cy="${ag[1] + 30}" r="5"/>`;
      b += `<rect data-k="tool" class="dot" x="${tl[0] - 10}" y="${tl[1] - 10}" width="20" height="20"/>`;
      b += `<circle data-k="call" class="pf" r="4" cx="-10" cy="-10"/><circle data-k="kd" class="af" r="5" cx="-10" cy="-10"/>`;
      b += T(ag[0], ag[1] - 34, "agent", "", "middle") + T(ag[0] + 40, ag[1] + 34, "handle") + T(tl[0], tl[1] - 22, "tool", "", "middle");
      b += T(br[0], br[1] + 24, "broker", "", "middle") + T(290, 252, "key at use", "ac", "middle");
      return S(b);
    },
    anim: (r, t) => {
      const p = (t % 3.4) / 3.4, call = q(r, "call"), key = q(r, "key"), kd = q(r, "kd"), L = len(key);
      const c = lerp([ag[0] + 24, ag[1]], [tl[0] - 14, tl[1]], sm(p / 0.3));
      call.setAttribute("cx", c[0]); call.setAttribute("cy", c[1]); call.setAttribute("opacity", p < 0.85 ? 1 : 0);
      const kf = sm((p - 0.3) / 0.3), fade = 1 - clamp((p - 0.82) / 0.12);
      key.style.strokeDasharray = L; key.style.strokeDashoffset = L * (1 - kf); key.setAttribute("opacity", fade);
      const kp = key.getPointAtLength(L * kf);
      kd.setAttribute("cx", kp.x); kd.setAttribute("cy", kp.y); kd.setAttribute("opacity", p > 0.3 && p < 0.62 ? 1 : 0);
      const tb = q(r, "tool");
      tb.style.stroke = tb.style.fill = p > 0.6 && p < 0.84 ? ACC : "";
    },
  };
}

/* Decide: with a person in the loop the check sees the overlap; unattended, the agent's own service identity. */
{
  const ax = 252, r1 = 78, d0 = 98, d1 = 196;
  const lens = (d) => {
    const a = d / 2, h = Math.sqrt(Math.max(0, r1 * r1 - a * a)), x = ax - d + a;
    return `M${x.toFixed(1)},${(200 - h).toFixed(1)} A${r1},${r1} 0 0 1 ${x.toFixed(1)},${(200 + h).toFixed(1)} A${r1},${r1} 0 0 1 ${x.toFixed(1)},${(200 - h).toFixed(1)} Z`;
  };
  FIGS.lens = {
    t0: 1.5,
    svg: () => {
      let b = `<line class="d" x1="30" y1="200" x2="370" y2="200"/>`;
      b += `<circle data-k="af" cx="${ax}" cy="200" r="${r1}" style="fill:url(#geo-g-lens)" opacity="0"/><path data-k="lens" d="${lens(d0)}" style="fill:url(#geo-g-lens)" class="a"/>`;
      b += `<g data-k="user"><circle class="i" cx="${ax - d0}" cy="200" r="${r1}"/><circle class="f" cx="${ax - d0}" cy="200" r="50"/><circle class="f" cx="${ax - d0}" cy="200" r="24"/><circle class="dot" cx="${ax - d0}" cy="200" r="3.5"/>` + T(ax - d0, 108, "person", "", "middle") + `</g>`;
      b += `<circle data-k="ag" class="i" cx="${ax}" cy="200" r="${r1}"/><polygon class="f" points="${poly(hex(ax, 200, 30))}"/><circle class="dot" cx="${ax}" cy="200" r="3.5"/>` + T(ax, 108, "agent class", "", "middle");
      b += Tk("subj", 200, 318, "class ∩ user:maya", "ac", "middle");
      return S(b, lin("geo-g-lens", 0, 0, 0, 1, 0.75, 0.1));
    },
    anim: (r, t) => {
      const p = (t % 8) / 8;
      const e = p < 0.38 ? 0 : p < 0.5 ? sm((p - 0.38) / 0.12) : p < 0.88 ? 1 : 1 - sm((p - 0.88) / 0.12);
      const d = d0 + (d1 - d0) * e, u = q(r, "user");
      u.setAttribute("transform", `translate(${(d0 - d).toFixed(1)} 0)`);
      u.setAttribute("opacity", 1 - 0.55 * clamp((d - 160) / 30));
      const L = q(r, "lens");
      if (d < 2 * r1 - 2) { L.setAttribute("d", lens(d)); L.setAttribute("opacity", 1); } else L.setAttribute("opacity", 0);
      const alone = clamp((d - 160) / 30);
      q(r, "af").setAttribute("opacity", alone);
      q(r, "ag").style.stroke = alone > 0.2 ? ACC : "";
      setText(q(r, "subj"), d < 158 ? "class ∩ user:maya" : "class ∩ service:nightly");
    },
  };
}

/* Carry: a scheduled run, a GitHub webhook, a Slack thread. Each wakes a session. */
{
  const src = [[70, 108, "cron"], [70, 200, "github"], [70, 292, "slack"]], seq = [0, 1, 0, 2], tx = 300, tb = 236, th = 104;
  const cD = (y) => `M86,${y} C170,${y} 200,188 ${tx - th / 2 / Math.sqrt(3) - 4},188`;
  FIGS.channels = {
    t0: 1.3,
    svg: () => {
      let b = "";
      src.forEach(([, y], k) => (b += `<path data-k="c${k}" class="f" d="${cD(y)}"/>`));
      b += `<path data-k="sf" d="${triD(tx, tb, th)}" style="fill:url(#geo-g-chan)" opacity="0"/><path data-k="bowl" class="i" d="${triD(tx, tb, th)}"/><path class="f" d="${holeD(tx, tb, th)}"/>` + band("rb", triD(tx, tb, th));
      src.forEach(([x, y, nm], k) => (b += `<circle data-k="s${k}" class="i" cx="${x}" cy="${y}" r="16" style="fill:var(--card)"/>` + T(x, y + 34, nm, "", "middle")));
      b += `<line data-k="hand" class="i" x1="70" y1="108" x2="70" y2="97"/><circle class="pf" cx="70" cy="108" r="2"/>`;
      b += `<polygon class="f" points="${poly(hex(70, 200, 7))}"/><circle class="f" cx="70" cy="292" r="6"/>`;
      b += `<circle data-k="pu" class="af" r="4" cx="-10" cy="-10"/>`;
      b += T(tx, tb + 28, "session", "", "middle") + Tk("fired", tx, tb - th - 16, "cron", "ac", "middle");
      return S(b, lin("geo-g-chan", 0, 0, 0, 1, 0.7, 0.05));
    },
    anim: (r, t) => {
      const n = Math.floor(t / 2.2), k = seq[n % 4], f = (t % 2.2) / 2.2, pu = q(r, "pu"), c = q(r, "c" + k);
      hueAt(r, k);
      runBand(r, "rb", sm((f - 0.42) / 0.4), clamp((f - 0.42) / 0.05) * (1 - clamp((f - 0.76) / 0.1)));
      for (let j = 0; j < 3; j++) {
        const on = j === k && f < 0.8;
        q(r, "c" + j).style.stroke = on ? ACC : "";
        q(r, "s" + j).style.stroke = on ? ACC : "";
      }
      if (f < 0.45) {
        const pt = c.getPointAtLength(len(c) * sm(f / 0.45));
        pu.setAttribute("cx", pt.x); pu.setAttribute("cy", pt.y); pu.setAttribute("opacity", 1);
      } else pu.setAttribute("opacity", 0);
      const env = clamp((f - 0.42) / 0.1) * (1 - clamp((f - 0.8) / 0.2));
      q(r, "sf").setAttribute("opacity", env);
      q(r, "bowl").style.stroke = env > 0.3 ? ACC : "";
      const fd = q(r, "fired");
      setText(fd, src[k][2]); fd.setAttribute("opacity", env);
      const a = t * 2, h = q(r, "hand");
      h.setAttribute("x2", 70 + 11 * Math.sin(a)); h.setAttribute("y2", 108 - 11 * Math.cos(a));
    },
  };
}

/* Remember: the session is committed to one slot. Reading another slot's memory is refused at the gate. */
{
  const ag = [64, 200], gate = 166, slabs = [[300, 112, "acme"], [300, 200, "globex"], [300, 288, "initech"]], seq = [0, 1, 0, 2];
  FIGS.memory = {
    t0: 0.9,
    svg: () => {
      let b = `<line class="d" x1="30" y1="200" x2="230" y2="200"/>`;
      slabs.forEach(([x, y], k) => (b += `<line data-k="l${k}" class="f" x1="${gate + 14}" y1="200" x2="${x - 62}" y2="${y}"/>`));
      slabs.forEach(([x, y, nm], k) => {
        const g = rh(x, y, 62, 0.4);
        b += `<polygon data-k="sl${k}" class="i" points="${poly4(g)}" style="fill:var(--card)"/><polygon class="f" points="${poly4(rh(x, y + 6, 62, 0.4))}" style="opacity:.5"/>`;
        b += Tk("sn" + k, x, y + 4, nm, k === 0 ? "ac" : "", "middle");
      });
      b += `<path class="i" d="${triD(gate, 238, 84)}" style="fill:var(--card)"/><path data-k="hole" class="i" d="${holeD(gate, 238, 84)}"/>`;
      b += agent(ag[0], ag[1], 17) + `<circle data-k="m" class="pf" r="4" cx="-10" cy="-10"/>`;
      b += T(ag[0] - 36, ag[1] - 32, "company:acme", "ac", "start") + Tk("vd", gate, 270, "", "", "middle");
      b += T(200, 372, "reading memory is a check", "", "middle");
      return S(b);
    },
    anim: (r, t) => {
      const n = Math.floor(t / 2), k = seq[n % 4], f = (t % 2) / 2, ok = k === 0, m = q(r, "m");
      const [sx, sy] = slabs[k], gp = [gate, 200], end = [sx - 62, sy];
      let p = lerp([ag[0] + 20, ag[1]], gp, sm(f / 0.35)), op = 1;
      if (f >= 0.5) {
        if (ok) p = lerp(gp, end, sm((f - 0.5) / 0.3));
        else op = 1 - clamp((f - 0.5) / 0.2);
      }
      if (f > 0.9) op = Math.min(op, 1 - (f - 0.9) / 0.1);
      m.setAttribute("cx", p[0]); m.setAttribute("cy", p[1]); m.setAttribute("opacity", op);
      m.style.fill = ok && f > 0.35 ? ACC : "";
      q(r, "hole").style.stroke = ok && f > 0.35 && f < 0.9 ? ACC : "";
      const vd = q(r, "vd");
      setText(vd, f > 0.35 && f < 0.92 ? (ok ? "allow" : "refused") : "");
      vd.setAttribute("class", "lbl" + (ok ? " ac" : ""));
      slabs.forEach((_, j) => {
        const on = ok && j === 0 && f > 0.78 && f < 0.97;
        const s = q(r, "sl" + j);
        s.style.stroke = on ? ACC : "";
        q(r, "l" + j).style.stroke = ok && j === 0 && f > 0.5 && f < 0.95 ? ACC : "";
      });
    },
  };
}

/* Area: every action authorized. A fence around the pod, or a gate on every edge. */
{
  const Lc = [110, 200], Rc = [290, 200], rr = 60, rg = 30, seq = [0, 3, 1, 4, 2, 5], allow = [1, 1, 0, 1, 0, 1];
  const at = (c, r, k) => {
    const a = ((-90 + k * 60) * Math.PI) / 180;
    return [c[0] + r * Math.cos(a), c[1] + r * Math.sin(a)];
  };
  FIGS.peraction = {
    t0: 0.8,
    svg: () => {
      let b = `<line class="d" x1="200" y1="56" x2="200" y2="344"/><circle class="i" cx="${Lc[0]}" cy="${Lc[1]}" r="82"/>`;
      for (let k = 0; k < 6; k++) {
        const [x, y] = at(Lc, rr, k), [X, Y] = at(Rc, rr, k), [gx, gy] = at(Rc, rg, k);
        b += `<line class="f" x1="${Lc[0]}" y1="${Lc[1]}" x2="${x}" y2="${y}"/><rect class="dot" x="${x - 5}" y="${y - 5}" width="10" height="10"/>`;
        b += `<line data-k="rl${k}" class="f" x1="${Rc[0]}" y1="${Rc[1]}" x2="${X}" y2="${Y}"/>`;
        b += `<path data-k="gt${k}" class="i" d="${triD(gx, gy + 4.5, 14)}" style="fill:var(--card)"/><path class="f" d="${holeD(gx, gy + 4.5, 14)}"/>`;
        b += `<rect data-k="rr${k}" class="dot" x="${X - 5}" y="${Y - 5}" width="10" height="10"/>`;
      }
      b += agent(Lc[0], Lc[1], 11) + agent(Rc[0], Rc[1], 11);
      b += `<circle data-k="ml" class="pf" r="3.5" cx="-10" cy="-10"/><circle data-k="mr" class="pf" r="3.5" cx="-10" cy="-10"/>`;
      b += T(110, 104, "per-environment", "", "middle") + T(290, 104, "per-action", "ac", "middle");
      b += T(110, 316, "one fence", "", "middle") + T(290, 316, "a check per call", "ac", "middle");
      return S(b);
    },
    anim: (r, t) => {
      const n = Math.floor(t / 1.1), f = (t % 1.1) / 1.1, k = seq[n % 6], ok = allow[k];
      const [lx, ly] = at(Lc, rr, seq[(n + 2) % 6]), e = sm(f < 0.5 ? f / 0.5 : (1 - f) / 0.5), ml = q(r, "ml");
      ml.setAttribute("cx", Lc[0] + (lx - Lc[0]) * e); ml.setAttribute("cy", Lc[1] + (ly - Lc[1]) * e);
      const G = at(Rc, rg, k), X = at(Rc, rr, k), mr = q(r, "mr");
      let p, op = 1;
      if (f < 0.35) p = lerp(Rc, G, sm(f / 0.35));
      else if (f < 0.5 || !ok) { p = G; if (!ok) op = 1 - clamp((f - 0.45) / 0.25); }
      else p = lerp(G, X, sm((f - 0.5) / 0.3));
      mr.setAttribute("cx", p[0]); mr.setAttribute("cy", p[1]); mr.setAttribute("opacity", op);
      mr.style.fill = ok && f >= 0.35 ? ACC : "";
      for (let j = 0; j < 6; j++) {
        const on = j === k && ok;
        q(r, "gt" + j).style.stroke = on && f >= 0.35 ? ACC : "";
        q(r, "rl" + j).style.stroke = on && f >= 0.5 ? ACC : "";
        const rs = q(r, "rr" + j);
        rs.style.stroke = rs.style.fill = on && f >= 0.8 ? ACC : "";
      }
    },
  };
}

/* Area: injected instructions can't widen access. The session's slots sit inside the OAP triangle;
   a tool result can carry an instruction, but a redirect to push main stops at the wall. */
{
  const apex = [200, 74], base = 302, ag = [200, 264], src = [362, 264], tgt = [330, 352];
  const half = (y) => (y - apex[1]) / Math.sqrt(3);
  const tHit = (base - ag[1]) / (tgt[1] - ag[1]), hit = lerp(ag, tgt, tHit);
  const slots = [[186, 142, "slot: pr #42", "end"], [214, 142, "spec: read-only", "start"]];
  FIGS.injection = {
    t0: 2.2,
    svg: () => {
      let b = `<path class="i" d="${triD(apex[0], base, base - apex[1])}"/>`;
      b += `<line class="f" x1="${src[0]}" y1="${src[1]}" x2="${ag[0]}" y2="${ag[1]}"/>`;
      slots.forEach(([x, y, s, an], k) => {
        const lx = an === "end" ? 200 - half(y) - 10 : 200 + half(y) + 10;
        b += `<line class="f" x1="${ag[0]}" y1="${ag[1]}" x2="${x}" y2="${y}"/><rect data-k="s${k}" class="af" x="${x - 7}" y="${y - 7}" width="14" height="14"/>` + T(lx, y + 5, s, "ac", an);
      });
      b += `<line data-k="atk" class="f" x1="${ag[0]}" y1="${ag[1]}" x2="${ag[0]}" y2="${ag[1]}"/><circle data-k="rip" class="i" r="0" cx="${hit[0]}" cy="${hit[1]}" opacity="0"/>`;
      b += agent(ag[0], ag[1], 13) + `<rect class="dot" x="${src[0] - 6}" y="${src[1] - 6}" width="12" height="12"/><rect class="dot" x="${tgt[0] - 6}" y="${tgt[1] - 6}" width="12" height="12" style="stroke:var(--ink-faint)"/>`;
      b += `<circle data-k="inj" class="pf" r="4" cx="-10" cy="-10"/>`;
      b += T(src[0], src[1] - 16, "pr diff", "", "middle") + T(tgt[0] - 14, tgt[1] + 5, "push main", "", "end");
      b += T(200, 40, "access stays what was granted", "ac", "middle");
      return S(b);
    },
    anim: (r, t) => {
      const p = (t % 4) / 4, inj = q(r, "inj"), atk = q(r, "atk"), rip = q(r, "rip");
      const ip = lerp(src, ag, sm(p / 0.3));
      inj.setAttribute("cx", ip[0]); inj.setAttribute("cy", ip[1]); inj.setAttribute("opacity", p < 0.32 ? 1 : 0);
      const a = lerp(ag, tgt, tHit * sm((p - 0.36) / 0.2)), fade = 1 - clamp((p - 0.8) / 0.15);
      atk.setAttribute("x2", a[0]); atk.setAttribute("y2", a[1]); atk.setAttribute("opacity", p > 0.36 ? fade : 0);
      const rp = clamp((p - 0.56) / 0.3);
      rip.setAttribute("r", 3 + 18 * rp); rip.setAttribute("opacity", p > 0.56 ? 1 - rp : 0);
      slots.forEach((_, k) => q(r, "s" + k).setAttribute("opacity", p > 0.56 && p < 0.86 ? 0.75 + 0.25 * Math.cos(rp * 12) : 1));
    },
  };
}

/* Area: data reaches only its audience. Data keeps its origin tag; a receiver outside that audience is refused. */
{
  const src = [60, 200], gate = 196, aud = [[318, 128, 46, "hr"], [318, 280, 62, "all-hands"]], seq = [0, 1];
  FIGS.leakage = {
    t0: 1.0,
    svg: () => {
      let b = `<line class="d" x1="30" y1="200" x2="236" y2="200"/>`;
      aud.forEach(([x, y, r], k) => (b += `<line data-k="l${k}" class="f" x1="${gate + 14}" y1="200" x2="${x - r}" y2="${y}"/>`));
      aud.forEach(([x, y, r, nm], k) => {
        b += `<circle data-k="af${k}" cx="${x}" cy="${y}" r="${r}" style="fill:url(#geo-g-leak)" opacity="0"/><circle data-k="a${k}" class="i" cx="${x}" cy="${y}" r="${r}"/>`;
        b += `<circle class="f" cx="${x}" cy="${y}" r="${r * 0.55}"/>` + T(x, y + r + 20, nm, "", "middle");
      });
      b += `<path class="i" d="${triD(gate, 238, 84)}" style="fill:var(--card)"/><path data-k="hole" class="i" d="${holeD(gate, 238, 84)}"/>`;
      b += `<rect class="dot" x="${src[0] - 9}" y="${src[1] - 9}" width="18" height="18"/>`;
      b += `<g data-k="m"><circle class="pf" r="4"/><circle data-k="tag" class="a" r="8"/></g>`;
      b += T(src[0], src[1] + 34, "salary.csv", "", "middle") + T(src[0], src[1] - 22, "origin:hr", "ac", "middle") + Tk("vd", 140, 274, "", "", "middle");
      return S(b, rad("geo-g-leak", 0.6));
    },
    anim: (r, t) => {
      const n = Math.floor(t / 2.2), k = seq[n % 2], f = (t % 2.2) / 2.2, ok = k === 0, m = q(r, "m");
      const [ax, ay] = aud[k], gp = [gate, 200];
      let p = lerp([src[0] + 14, src[1]], gp, sm(f / 0.35)), op = 1;
      if (f >= 0.5) {
        if (ok) p = lerp(gp, [ax, ay], sm((f - 0.5) / 0.3));
        else op = 1 - clamp((f - 0.5) / 0.2);
      }
      if (f > 0.9) op = Math.min(op, 1 - (f - 0.9) / 0.1);
      m.setAttribute("transform", `translate(${p[0].toFixed(1)} ${p[1].toFixed(1)})`);
      m.setAttribute("opacity", op);
      q(r, "hole").style.stroke = ok && f > 0.35 && f < 0.9 ? ACC : "";
      const vd = q(r, "vd");
      setText(vd, f > 0.35 && f < 0.92 ? (ok ? "same audience" : "held for owner") : "");
      vd.setAttribute("class", "lbl" + (ok ? " ac" : ""));
      aud.forEach((_, j) => {
        const on = ok && j === 0 && f > 0.72 && f < 0.97;
        q(r, "af" + j).setAttribute("opacity", on ? 1 : 0);
        q(r, "a" + j).style.stroke = on ? ACC : "";
        q(r, "l" + j).style.stroke = ok && j === 0 && f > 0.5 && f < 0.95 ? ACC : "";
      });
    },
  };
}

/* Area: least privilege. Each sub-agent is its own session with its own class and grants, never the
   parent's; the whole tree is bounded by a session budget (maxDelegatedAgents), so the next spawn is refused. */
{
  const nodes = [[200, 128, 74, -1], [124, 228, 56, 0], [276, 228, 56, 0], [236, 322, 44, 2]], extra = [316, 322, 44, 2], BUDGET = 4;
  const apexOf = ([x, b, h]) => [x, b - h];
  FIGS.delegation = {
    t0: 5.3,
    svg: () => {
      let b = "";
      [...nodes, extra].forEach((n, k) => {
        if (n[3] < 0) return;
        const [px, pb] = nodes[n[3]], [ax, ay] = apexOf(n);
        b += `<line data-k="e${k}" class="f" x1="${px}" y1="${pb}" x2="${ax}" y2="${ay}"${k === 4 ? ' opacity="0"' : ""}/>`;
      });
      nodes.forEach(([x, base, h], k) => (b += `<path data-k="tf${k}" d="${triD(x, base, h)}" style="fill:url(#geo-g-del)" opacity="0"/><path data-k="t${k}" class="i" d="${triD(x, base, h)}"/><path class="f" d="${holeD(x, base, h)}"/>`));
      b += `<path data-k="t4" class="f" d="${triD(extra[0], extra[1], extra[2])}" opacity="0"/>`;
      b += Tk("count", 380, 34, "1 / 4 sessions", "", "end") + T(200, 372, "own grants, never the parent's", "ac", "middle");
      b += Tk("cap", 200, 350, "tree budget: spawn refused", "", "middle", 0);
      return S(b, lin("geo-g-del", 0, 0, 0, 1, 0.7, 0.05));
    },
    anim: (r, t) => {
      const p = (t % 8) / 8, out = 1 - clamp((p - 0.92) / 0.08);
      let newest = -1;
      nodes.forEach((n, k) => {
        const s0 = 0.04 + k * 0.16, e = q(r, "t" + k), L = len(e);
        e.style.strokeDasharray = L; e.style.strokeDashoffset = L * (1 - sm((p - s0) / 0.1));
        if (p > s0) newest = k;
        const ed = q(r, "e" + k);
        if (ed) ed.setAttribute("opacity", p > s0 ? out : 0);
      });
      nodes.forEach((_, k) => {
        const on = k === newest;
        q(r, "t" + k).style.stroke = on ? ACC : "";
        q(r, "t" + k).style.opacity = out;
        q(r, "tf" + k).setAttribute("opacity", on ? out : 0);
      });
      setText(q(r, "count"), `${newest + 1} / ${BUDGET} sessions`);
      q(r, "count").setAttribute("opacity", out);
      const rf = p > 0.72 && p < 0.9, fl = rf ? 0.45 + 0.35 * Math.sin(t * 14) : 0;
      q(r, "t4").setAttribute("opacity", fl); q(r, "e4").setAttribute("opacity", fl);
      q(r, "cap").setAttribute("opacity", rf ? 1 : 0);
    },
  };
}

/* Area: revocation. Per-action stops at the very next call; a credential in the environment keeps passing. */
{
  const xr = 214, xs = [60, 88, 116, 144, 172, 200, 228, 256, 284, 312, 340], yA = 140, yB = 272;
  FIGS.revoke = {
    t0: 4.4,
    svg: () => {
      let b = `<line class="d" x1="${xr}" y1="92" x2="${xr}" y2="300"/>`;
      b += `<rect x="${xr}" y="${yB - 7}" width="${352 - xr}" height="14" style="fill:url(#geo-h-rev)"/>`;
      b += `<line class="i" x1="44" y1="${yA}" x2="356" y2="${yA}"/><line class="i" x1="44" y1="${yB}" x2="356" y2="${yB}"/>`;
      b += `<line class="a" x1="44" y1="${yA}" x2="${xr}" y2="${yA}" style="stroke-width:3"/><line class="i" x1="352" y1="${yB - 12}" x2="352" y2="${yB + 12}"/>`;
      xs.forEach((x, i) => (b += `<circle data-k="a${i}" class="dot" cx="${x}" cy="${yA}" r="4.5" opacity="0"/><circle data-k="b${i}" class="pf" cx="${x}" cy="${yB}" r="3.5" opacity="0"/>`));
      b += `<line data-k="ph" class="f" x1="40" y1="104" x2="40" y2="304"/>`;
      b += mark(44, yA - 30, 16) + T(66, yA - 18, "OAP", "ac") + T(44, yB - 18, "shared credential") + T(xr, 82, "revoke grant", "", "middle");
      b += T(xr + 8, yA + 28, "next call refused") + T(xr + 6, yB + 32, "still passing") + T(352, yB - 22, "redeploy", "", "middle");
      return S(b, hatch("geo-h-rev"));
    },
    anim: (r, t) => {
      const p = (t % 6.5) / 6.5, x = 40 + 320 * clamp(p / 0.82), fade = 1 - clamp((p - 0.92) / 0.08), ph = q(r, "ph");
      ph.setAttribute("x1", x); ph.setAttribute("x2", x); ph.setAttribute("opacity", fade);
      xs.forEach((cx, i) => {
        const on = x >= cx, a = q(r, "a" + i), ok = cx < xr;
        a.setAttribute("opacity", on ? fade : 0);
        q(r, "b" + i).setAttribute("opacity", on ? fade : 0);
        a.style.fill = ok ? ACC : "";
        a.style.stroke = ok ? ACC : "";
      });
    },
  };
}

/* Area: isolation in the platform. Runner, operator and sanitizer in separate pods; a breakout meets the pod wall. */
{
  const pods = [[132, 192, "runner"], [200, 192, "operator"], [268, 192, "sanitizer"]], pw = 30;
  FIGS.pods = {
    t0: 1.0,
    svg: () => {
      let b = `<polygon class="i" points="${poly4(rh(200, 196, 172, 0.5))}" style="fill:var(--card)"/>`;
      pods.forEach(([x, y, nm], k) => {
        const g = rh(x, y, pw, 0.5);
        b += `<polygon data-k="p${k}" class="i" points="${poly4(g)}" style="fill:var(--card)"/>` + agent(x, y, 9) + T(x, k === 1 ? y - 22 : y + 32, nm, "", "middle");
      });
      b += `<circle data-k="rip" class="a" r="0" cx="0" cy="0" opacity="0"/><circle data-k="eg" class="af" r="3.5" cx="-10" cy="-10"/>`;
      b += T(200, 92, "separate pods", "", "middle") + Tk("wall", 200, 352, "the pod wall holds", "ac", "middle", 0);
      return S(b);
    },
    anim: (r, t) => {
      const n = Math.floor(t / 1.8) % 3, f = (t % 1.8) / 1.8, [x, y] = pods[n];
      const h = pw / 4, tgt = n === 2 ? [x - pw / 2, y - h] : n === 0 ? [x + pw / 2, y + h] : [x - pw / 2, y + h];
      const out = f < 0.45 ? sm(f / 0.45) : f < 0.55 ? 1 : 1 - sm((f - 0.55) / 0.45);
      const e = lerp([x, y], tgt, out * 0.94), eg = q(r, "eg");
      eg.setAttribute("cx", e[0]); eg.setAttribute("cy", e[1]);
      const rp = clamp((f - 0.45) / 0.35), rip = q(r, "rip");
      rip.setAttribute("cx", tgt[0]); rip.setAttribute("cy", tgt[1]);
      rip.setAttribute("r", 2 + 14 * rp); rip.setAttribute("opacity", f > 0.45 ? 1 - rp : 0);
      pods.forEach((_, k) => (q(r, "p" + k).style.stroke = k === n && f > 0.42 && f < 0.85 ? ACC : ""));
      q(r, "wall").setAttribute("opacity", f > 0.42 && f < 0.9 ? 1 : 0.0);
    },
  };
}

/* Close: defense in depth. An attack (a neutral dot) falls through the stack;
   each layer it breaks turns red. Each layer
   it gets past cracks along a seam and its halves drift apart; the next layer
   holds (the accent). Each cycle one more layer fails, so every layer gets to
   be the one that holds. */
{
  const names = ["plan", "slot", "tool spec", "call check", "sandbox"];
  const ys = [92, 144, 196, 248, 300], W = 110, CX = 170, K = 0.36;
  const BROKE = "var(--deny)";
  const halves = (y) => {
    const g = rh(CX, y, W, K), h = W * K;
    const seam = [g.T, [CX - 7, y - h * 0.45], [CX + 6, y - h * 0.05], [CX - 5, y + h * 0.4], g.B];
    return { left: [g.L, ...seam].map(P).join(" "), right: [g.R, ...seam].map(P).join(" ") };
  };
  FIGS.depth = {
    t0: 1.9,
    svg: () => {
      let b = `<line class="d" x1="${CX}" y1="28" x2="${CX}" y2="372"/>`;
      ys.slice().reverse().forEach((y, ri) => {
        const k = ys.length - 1 - ri, g = rh(CX, y, W, K), hv = halves(y);
        b += `<polygon data-k="L${k}" points="${hv.left}" style="fill:var(--card);stroke:none"/><polygon data-k="R${k}" points="${hv.right}" style="fill:var(--card);stroke:none"/>`;
        b += `<polygon data-k="pg${k}" points="${poly4(g)}" style="fill:url(#geo-g-depth)" opacity="0"/><polygon data-k="p${k}" class="i" points="${poly4(g)}"/>`;
      });
      names.forEach((nm, k) => (b += Tk("n" + k, 290, ys[k] + 4, nm)));
      b += `<circle data-k="rip" class="a" r="0" cx="${CX}" cy="0" opacity="0"/>`;
      b += `<line data-k="trail" x1="${CX}" y1="0" x2="${CX}" y2="0" style="stroke:var(--ink-faint);stroke-width:2;stroke-linecap:round" opacity="0"/>`;
      b += `<circle data-k="atk" r="5" cx="${CX}" cy="20" style="fill:var(--ink)"/>`;
      return S(b, lin("geo-g-depth", 0, 0, 1, 1, 0.65, 0.05));
    },
    anim: (r, t) => {
      const n = Math.floor(t / 3.4), f = (t % 3.4) / 3.4, fail = n % 4, hold = fail + 1;
      const y = 20 + (ys[hold] - 20) * clamp(f / 0.6), reset = clamp((f - 0.88) / 0.12);
      const atk = q(r, "atk"), trail = q(r, "trail"), rip = q(r, "rip");
      atk.setAttribute("cy", y); atk.setAttribute("opacity", 1 - reset);
      trail.setAttribute("y1", Math.max(20, y - 34)); trail.setAttribute("y2", y);
      trail.setAttribute("opacity", f < 0.6 ? 0.45 : 0);
      const held = f > 0.6 && f < 0.92;
      ys.forEach((ly, k) => {
        const brk = k <= fail ? clamp((y - ly) / 26) * (1 - reset) : 0, on = k === hold && held;
        const L = q(r, "L" + k), R = q(r, "R" + k), lbl = q(r, "n" + k);
        L.setAttribute("transform", `translate(${(-8 * brk).toFixed(1)} ${(4 * brk).toFixed(1)})`);
        R.setAttribute("transform", `translate(${(8 * brk).toFixed(1)} ${(-4 * brk).toFixed(1)})`);
        L.style.stroke = R.style.stroke = brk > 0.02 ? BROKE : "none";
        L.style.strokeWidth = R.style.strokeWidth = "1.2";
        L.style.opacity = R.style.opacity = String(1 - 0.35 * brk);
        q(r, "p" + k).setAttribute("opacity", 1 - brk);
        q(r, "p" + k).style.stroke = on ? ACC : "";
        q(r, "pg" + k).setAttribute("opacity", on ? 1 : 0);
        lbl.setAttribute("class", "lbl" + (on ? " ac" : ""));
        lbl.style.fill = brk > 0.5 ? BROKE : "";
      });
      const rp = clamp((f - 0.6) / 0.3);
      rip.setAttribute("cy", ys[hold]); rip.setAttribute("r", 3 + 24 * rp); rip.setAttribute("opacity", f > 0.6 ? 1 - rp : 0);
    },
  };
}

export { FIGS };
