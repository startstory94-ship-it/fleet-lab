"use strict";
// fleetd dashboard. Plain JavaScript, no build step, no external files.
//
// Data:
//   /ws/live  position frames (snapshot first, then deltas of changed robots,
//             heartbeats when nothing changes)
//   /ws       events (connection, mode, task, battery); /events?since= fills gaps
//   /tasks    polled every second and after every task event
//
// Robots are drawn RENDER_DELAY_MS behind the server clock and interpolated
// between the positions received, so they move smoothly although positions
// arrive in ~150 ms batches. The first part of this file is pure (no DOM) and
// is tested with `node scripts/web_test.js`.

const RENDER_DELAY_MS = 350;   // two frames and a bit: there is almost always a sample ahead
const EXTRAPOLATE_MS = 250;    // past the newest sample, keep moving with v/yaw this long at most
const LIVE_SILENCE_MS = 5000;  // the server sends a heartbeat every 2 s; silence longer = dead
const MAX_COORD = 1e6;         // metres; the server refuses anything farther
const MAX_GRID_LINES = 200;    // per axis
const MAX_EVENTS = 200;
const TASK_POLL_MS = 1000;
const UNDO_MS = 3000;
const ACTIVE = new Set(["QUEUED", "ASSIGNED", "RUNNING"]);
const MOVING_MODES = new Set(["MOVING", "DOCKING", "BOARDING", "EXITING"]);

// ------------------------------------------------------------ pure helpers

// Position at time t (ms, server clock) from a track of samples {t, x, y,
// yaw, floor}, interpolated between samples. Past the newest sample a robot
// that is moving (motion = {v, mode}) goes on along its heading for at most
// EXTRAPOLATE_MS, then holds.
function poseAt(track, t, motion) {
  if (!track.length) return null;
  if (t <= track[0].t) return track[0];
  const last = track[track.length - 1];
  if (t >= last.t) {
    if (!motion || !MOVING_MODES.has(motion.mode) || !(Math.abs(motion.v) > 0.01)) return last;
    const dt = Math.min(t - last.t, EXTRAPOLATE_MS) / 1000;
    return { ...last, x: last.x + motion.v * Math.cos(last.yaw) * dt, y: last.y + motion.v * Math.sin(last.yaw) * dt };
  }
  for (let i = track.length - 1; i > 0; i--) {
    const a = track[i - 1], b = track[i];
    if (t >= a.t && t <= b.t) {
      const k = b.t > a.t ? (t - a.t) / (b.t - a.t) : 1;
      let dyaw = b.yaw - a.yaw;
      dyaw = Math.atan2(Math.sin(dyaw), Math.cos(dyaw)); // shortest turn
      return { x: a.x + (b.x - a.x) * k, y: a.y + (b.y - a.y) * k, yaw: a.yaw + dyaw * k, floor: b.floor };
    }
  }
  return last;
}

// Grid lines between lo and hi (metres). The step starts at minStep and grows
// 1-5-10-50-100... until there are at most maxLines, and lines are computed
// from an integer index, so far-away or huge ranges cannot hang the loop.
function gridLines(lo, hi, minStep, maxLines = MAX_GRID_LINES) {
  if (!isFinite(lo) || !isFinite(hi) || hi < lo) return { step: minStep, lines: [] };
  let step = minStep;
  while ((hi - lo) / step > maxLines) step *= String(step)[0] === "1" ? 5 : 2;
  // Count with k: far from the origin i + 1 can equal i in floating point.
  const lines = [];
  const first = Math.ceil(lo / step), n = Math.min(Math.floor(hi / step) - first, maxLines + 1);
  for (let k = 0; k <= n; k++) {
    const i = first + k;
    if (lines.length && lines[lines.length - 1].v === i * step) continue;
    lines.push({ v: i * step, major: i % 5 === 0 });
  }
  return { step, lines };
}

const inRange = (p) => isFinite(p.x) && isFinite(p.y) && Math.abs(p.x) <= MAX_COORD && Math.abs(p.y) <= MAX_COORD;

// EventFeed shows events in seq order without losing any. Events from the
// WebSocket that do not follow the last one shown are held; the missing ones
// are fetched (fetchSince(seq) -> events after seq, oldest first) and merged.
// Only what the server no longer keeps is skipped (onMissed(n)). Overlapping
// fills are coalesced: a push during a fill makes it fetch again.
class EventFeed {
  constructor(fetchSince, show, onMissed = () => {}) {
    Object.assign(this, { fetchSince, show, onMissed });
    this.shown = 0;          // highest seq shown; all before it shown or skipped
    this.held = new Map();   // seq -> event waiting for the ones before it
    this.filling = null;
    this.again = false;
  }

  push(e) {
    if (e.seq <= this.shown || this.held.has(e.seq)) return;
    this.held.set(e.seq, e);
    this.drain();
    if (this.held.size) this.fill();
  }

  drain() {
    while (this.held.has(this.shown + 1)) {
      const e = this.held.get(this.shown + 1);
      this.held.delete(e.seq);
      this.shown = e.seq;
      this.show(e);
    }
  }

  // After a reconnect, with the server's run id (X-Fleetd-Boot) and newest
  // event seq (undefined if none). A new run id, or a newest seq below what
  // was shown, means the server restarted and numbering began again: start
  // over. Returns whether it did.
  reconnected(latest, boot) {
    const newRun = this.boot !== undefined && boot && boot !== this.boot;
    const went = this.shown > 0 && (latest === undefined || latest < this.shown);
    if (boot) this.boot = boot;
    if (!newRun && !went) return false;
    this.shown = 0;
    this.held.clear();
    return true;
  }

  fill() {
    if (this.filling) { this.again = true; return this.filling; }
    this.filling = (async () => {
      do {
        this.again = false;
        const base = this.shown;
        let evs;
        try { evs = await this.fetchSince(base); } catch (err) { return; } // next push or reconnect retries
        for (const e of evs) if (e.seq > this.shown && !this.held.has(e.seq)) this.held.set(e.seq, e);
        if (evs.length && evs[0].seq > base + 1 && this.shown === base) {
          // The server dropped base+1 .. evs[0].seq-1 from its window.
          if (base > 0) this.onMissed(evs[0].seq - base - 1);
          this.shown = evs[0].seq - 1;
        }
        this.drain();
        if (this.held.size && !evs.length) {
          // Nothing newer on the server, yet we hold later events: give up on the gap.
          const first = Math.min(...this.held.keys());
          this.onMissed(first - this.shown - 1);
          this.shown = first - 1;
          this.drain();
        } else if (this.held.size) {
          this.again = true; // arrived after this fetch; fetch once more
        }
      } while (this.again);
    })().finally(() => { this.filling = null; });
    return this.filling;
  }
}

if (typeof module !== "undefined") module.exports = { poseAt, gridLines, inRange, EventFeed, MOVING_MODES };

// ------------------------------------------------------------------ the page

if (typeof document !== "undefined") {

const MODE_LABEL = {
  IDLE: "대기", MOVING: "이동", DOCKING: "도킹", CHARGING: "충전", ERROR: "오류",
  WAITING_ELEVATOR: "엘리베이터 대기", BOARDING: "탑승", RIDING: "탑승 중", EXITING: "하차",
};
const CONN_LABEL = { ONLINE: "온라인", STALE: "끊김", OFFLINE: "오프라인", IN_ELEVATOR: "엘리베이터" };
const TASK_LABEL = {
  QUEUED: "대기", ASSIGNED: "배정", RUNNING: "수행", DONE: "완료", FAILED: "실패", CANCELLED: "취소",
};

const state = {
  robots: new Map(), // name -> {v: latest view, track: [{t, x, y, yaw, floor}]}
  docks: new Map(),  // name -> {x, y, floor}, where the robot was seen CHARGING
  tasks: [],
  floor: null,       // selected floor
  draft: [],         // shift-click waypoints
  bounds: null,      // {floor, minX, minY, maxX, maxY}; grows, reset on floor change
  clockOffset: null, // server clock - local clock, ms
  liveSeq: 0,
  up: {},            // which -> connected; missing until the first attempt ends
  view: null,        // world -> canvas transform of the last render
};

const $ = (id) => document.getElementById(id);
const css = (name) => getComputedStyle(document.documentElement).getPropertyValue(name).trim();
const esc = (s) => String(s ?? "").replace(/[&<>"']/g, (c) => `&#${c.charCodeAt(0)};`);

// ---------------------------------------------------------------- network

// connect keeps a WebSocket open, reconnecting with backoff. With silenceMs,
// a socket that receives nothing for that long is closed and reopened (the
// server's heartbeats keep a healthy one busy).
function connect(path, onMessage, onUp, silenceMs) {
  let delay = 500;
  const open = () => {
    const ws = new WebSocket((location.protocol === "https:" ? "wss://" : "ws://") + location.host + path);
    let last = Date.now(), watch = null;
    ws.onopen = () => {
      delay = 500;
      last = Date.now();
      onUp(true);
      if (silenceMs) {
        watch = setInterval(() => {
          if (Date.now() - last > silenceMs) { onUp(false, "응답 없음"); ws.close(); }
        }, 1000);
      }
    };
    ws.onmessage = (m) => { last = Date.now(); onMessage(JSON.parse(m.data)); };
    ws.onerror = () => ws.close();
    ws.onclose = () => {
      clearInterval(watch);
      onUp(false);
      setTimeout(open, delay * (0.75 + Math.random() * 0.5)); // backoff with jitter
      delay = Math.min(delay * 2, 10000);
    };
  };
  open();
}

async function api(method, path, body) {
  const res = await fetch(path, {
    method,
    headers: body ? { "Content-Type": "application/json" } : {},
    body: body ? JSON.stringify(body) : undefined,
  });
  const data = await res.json().catch(() => ({}));
  if (!res.ok) throw new Error(data.error || `${res.status}`);
  return data;
}

function setUp(which, up, why) {
  if (state.up[which] === up && !why) return;
  state.up[which] = up;
  const el = $(which === "live" ? "conn-live" : "conn-events");
  el.textContent = (which === "live" ? "위치 " : "이벤트 ") + (up ? "연결됨" : why || "끊김");
  el.className = "conn " + (up ? "ok" : "bad");
  $("banner").hidden = !Object.values(state.up).includes(false);
}

// ------------------------------------------------------------ live frames

function onFrame(f) {
  const t = Date.parse(f.time);
  // The smallest delay seen is the best guess of the clock offset; let it
  // drift 1 ms per frame so a changed offset is picked up eventually.
  const off = t - Date.now();
  state.clockOffset = state.clockOffset === null ? off : Math.max(off, state.clockOffset - 1);
  if (f.type === "heartbeat") return; // only says the feed is alive

  if (f.type === "snapshot") {
    const names = new Set(f.robots.map((r) => r.name));
    for (const name of [...state.robots.keys()]) if (!names.has(name)) state.robots.delete(name);
  } else if (f.seq !== state.liveSeq + 1) {
    console.warn("live frame gap", state.liveSeq, f.seq); // the server prevents this
  }
  state.liveSeq = f.seq;
  for (const v of f.robots) applyRobot(v, t);
  for (const name of f.removed || []) state.robots.delete(name);
  if (state.floor === null) {
    const first = f.robots.find((r) => r.has_pose);
    if (first) selectFloor(first.floor);
  }
  renderRobotList();
  renderFloors();
}

function applyRobot(v, t) {
  let r = state.robots.get(v.name);
  if (!r) state.robots.set(v.name, (r = { track: [] }));
  const prev = r.v;
  r.v = v;
  if (v.mode === "CHARGING" && v.has_pose) state.docks.set(v.name, { x: v.x, y: v.y, floor: v.floor });
  if (!v.has_pose) return;
  const last = r.track[r.track.length - 1];
  if (last && last.floor !== v.floor) r.track = []; // changed floors: jump
  else if (last && last.x === v.x && last.y === v.y && last.yaw === v.yaw &&
           prev && prev.v === v.v && prev.mode === v.mode) return; // not a move
  else if (last && t - last.t > 400) {
    // It stood still since `last`: hold there until just before this sample,
    // instead of crawling across the whole pause.
    r.track.push({ ...last, t: t - 150 });
  }
  r.track.push({ t, x: v.x, y: v.y, yaw: v.yaw, floor: v.floor });
  const keepFrom = renderTime() - 3000;
  while (r.track.length > 2 && r.track[1].t < keepFrom) r.track.shift();
}

function renderTime() {
  return Date.now() + (state.clockOffset ?? 0) - RENDER_DELAY_MS;
}

// ----------------------------------------------------------------- events

function describe(e) {
  const arrow = (a, b) => `${a || "–"} → ${b || "–"}`;
  switch (e.type) {
    case "seen": return `${e.robot} 처음 보임`;
    case "conn": return `${e.robot} 연결 ${arrow(CONN_LABEL[e.from] || e.from, CONN_LABEL[e.to] || e.to)}${e.detail ? ` (${e.detail})` : ""}`;
    case "mode": return `${e.robot} 모드 ${arrow(MODE_LABEL[e.from] || e.from, MODE_LABEL[e.to] || e.to)}`;
    case "error": return `${e.robot} 오류 ${e.to ? e.to : "해제"}`;
    case "task": return `${e.task} ${e.robot ? `[${e.robot}] ` : ""}${e.detail || ""}`;
    case "battery": return `${e.robot} 배터리 ${e.detail || ""}`;
    case "adapter": return `어댑터 ${e.detail || ""}`;
    case "elevator_overdue": return `${e.robot} 엘리베이터 예정 시간 초과`;
    default: return `${e.robot || ""} ${e.type} ${e.detail || ""}`;
  }
}

const TYPE_COLOR = { conn: "--moving", mode: "--idle", error: "--error", task: "--task", battery: "--charging",
  adapter: "--offline", elevator_overdue: "--elevator", local: "--draft" };

function addEvent(e) {
  const li = document.createElement("li");
  const time = new Date(e.time || Date.now()).toLocaleTimeString("ko-KR", { hour12: false });
  const label = e.type === "local" ? "화면" : e.type;
  li.innerHTML = `<time>${esc(time)}</time><span class="type" style="color:var(${TYPE_COLOR[e.type] || "--text"})">${esc(label)}</span> ${esc(e.text || describe(e))}`;
  const list = $("events");
  list.prepend(li);
  while (list.children.length > MAX_EVENTS) list.lastChild.remove();
}

const note = (text) => addEvent({ type: "local", text });

const feed = new EventFeed(
  (seq) => api("GET", `/events?since=${seq}`),
  (e) => { addEvent(e); if (e.type === "task") refreshTasksSoon(); },
  (n) => note(`이벤트 ${n}개는 서버에서 이미 지워져 보이지 않음`),
);

async function onEventsUp() {
  try {
    const res = await fetch("/events?n=1");
    const [latest] = (await res.json()) || [];
    const boot = res.headers.get("X-Fleetd-Boot");
    const restarted = feed.reconnected(latest?.seq, boot);
    console.info("events connected", { boot, latest: latest?.seq, restarted });
    if (restarted) note("서버가 다시 시작됨: 이벤트를 처음부터 다시 받음");
  } catch (err) { console.warn("events: reconnect check failed", err); return; }
  feed.fill();
}

// ------------------------------------------------------------------ tasks

let taskTimer = null;
function refreshTasksSoon() {
  clearTimeout(taskTimer);
  taskTimer = setTimeout(refreshTasks, 100);
}

async function refreshTasks() {
  clearTimeout(taskTimer);
  try {
    state.tasks = await api("GET", "/tasks");
    renderTasks();
    renderFloors();
  } catch (err) { /* shown by the connection badges */ }
  taskTimer = setTimeout(refreshTasks, TASK_POLL_MS);
}

async function submitTask(waypoints) {
  try {
    const t = await api("POST", "/tasks", { waypoints });
    note(`작업 ${t.id} 보냄 (${waypoints.length}개 지점)`);
    refreshTasksSoon();
    return t;
  } catch (err) {
    note(`작업 보내기 실패: ${err.message}`);
    return null;
  }
}

async function cancelTask(id) {
  try {
    await api("POST", `/tasks/${encodeURIComponent(id)}/cancel`, {});
    note(`작업 ${id} 취소 요청`);
  } catch (err) {
    note(`작업 ${id} 취소 실패: ${err.message}`);
  }
  refreshTasksSoon();
}

// A plain click sends a task at once; this toast offers to take it back.
let toastTimer = null;
function undoToast(t) {
  const el = $("toast");
  el.innerHTML = `작업 ${esc(t.id)} 보냄 <button data-cancel="${esc(t.id)}">취소</button>`;
  el.hidden = false;
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => { el.hidden = true; }, UNDO_MS);
}

function renderTasks() {
  const tasks = state.tasks;
  const active = tasks.filter((t) => ACTIVE.has(t.state));
  const finished = tasks.filter((t) => !ACTIVE.has(t.state)).slice(-15).reverse();
  $("count-tasks").textContent = `작업 ${active.length}개 진행 / 전체 ${tasks.length}개`;
  $("tasks").tBodies[0].innerHTML = [...active, ...finished].map((t) => `
    <tr class="${ACTIVE.has(t.state) ? "" : "finished"}" title="${esc(t.error || "")}">
      <td>${esc(t.id)}</td>
      <td>${esc(TASK_LABEL[t.state] || t.state)}</td>
      <td>${esc(t.robot || "–")}</td>
      <td class="num">${Math.min(t.leg + (t.state === "DONE" ? 0 : 1), t.waypoints.length)}/${t.waypoints.length}</td>
      <td>${ACTIVE.has(t.state) ? `<button data-cancel="${esc(t.id)}">취소</button>` : ""}</td>
    </tr>`).join("");
}

// ----------------------------------------------------------------- robots

function robotColor(v) {
  if (v.conn === "OFFLINE") return css("--offline");
  if (v.conn === "IN_ELEVATOR") return css("--elevator");
  if (v.conn === "STALE") return css("--idle");
  if (v.mode === "ERROR" || v.error) return css("--error");
  if (v.mode === "MOVING") return css("--moving");
  if (v.mode === "DOCKING" || v.mode === "CHARGING") return css("--charging");
  if (["WAITING_ELEVATOR", "BOARDING", "RIDING", "EXITING"].includes(v.mode)) return css("--elevator");
  return css("--idle");
}

function renderRobotList() {
  const robots = [...state.robots.values()].map((r) => r.v).sort((a, b) => a.name.localeCompare(b.name));
  const online = robots.filter((v) => v.conn === "ONLINE").length;
  $("count-robots").textContent = `로봇 ${robots.length}대 (온라인 ${online})`;
  $("robots").tBodies[0].innerHTML = robots.map((v) => {
    const pct = Math.max(0, Math.min(100, v.battery));
    const battery = v.battery_known
      ? `<span class="battery ${v.needs_charge ? "low" : ""}"><b style="width:${pct}%"></b></span> ${pct.toFixed(0)}%`
      : "–";
    const mark = v.needs_charge ? `<span class="charge-mark" title="충전 필요: 새 작업을 받지 않음">⚡충전 필요</span>` : "";
    const where = v.has_pose ? `${v.floor}층` : "위치 모름";
    return `<tr title="${esc(`${v.host || "?"} · ${where}${v.error ? " · 오류 " + v.error : ""}`)}">
      <td><span class="dot" style="background:${robotColor(v)}"></span>${esc(v.name)}</td>
      <td>${esc(CONN_LABEL[v.conn] || v.conn)} <span class="state">${esc(MODE_LABEL[v.mode] || v.mode || "")}</span></td>
      <td class="num">${battery}${mark}</td>
      <td>${esc(v.task_id ? v.task_id.split(".")[0] : "")}</td>
    </tr>`;
  }).join("");
}

// ------------------------------------------------------------------ floors

function floorsSeen() {
  const floors = new Set();
  for (const r of state.robots.values()) if (r.v.has_pose) floors.add(r.v.floor);
  for (const t of state.tasks) if (ACTIVE.has(t.state)) for (const w of t.waypoints) floors.add(w.floor);
  if (state.floor !== null) floors.add(state.floor);
  return [...floors].sort((a, b) => a - b);
}

function renderFloors() {
  const floors = floorsSeen();
  const key = floors.join(",") + "|" + state.floor;
  const el = $("floors");
  if (el.dataset.key === key) return;
  el.dataset.key = key;
  el.innerHTML = floors.map((f) => `<button data-floor="${f}" class="${f === state.floor ? "on" : ""}">${f}층</button>`).join(" ");
}

function selectFloor(f) {
  if (state.floor === f) return;
  state.floor = f;
  state.bounds = null;
  renderFloors();
}

// -------------------------------------------------------------------- map

// Points that must be on screen for the current floor. Points beyond
// MAX_COORD are left out: one bad value must not shrink the map to nothing.
function interestingPoints() {
  const pts = [];
  for (const r of state.robots.values()) {
    const last = r.track[r.track.length - 1];
    if (last && last.floor === state.floor) pts.push(last);
  }
  for (const d of state.docks.values()) if (d.floor === state.floor) pts.push(d);
  for (const t of state.tasks) {
    if (ACTIVE.has(t.state)) for (const w of t.waypoints) if (w.floor === state.floor) pts.push(w);
  }
  for (const w of state.draft) pts.push(w);
  return pts.filter(inRange);
}

// Bounds only grow while the floor stays the same, so the map does not
// jump around as robots move.
function updateBounds() {
  const PAD = 2, MIN_SPAN = 10;
  let b = state.bounds;
  if (!b || b.floor !== state.floor) b = state.bounds = { floor: state.floor, minX: Infinity, minY: Infinity, maxX: -Infinity, maxY: -Infinity };
  for (const p of interestingPoints()) {
    b.minX = Math.min(b.minX, p.x - PAD); b.maxX = Math.max(b.maxX, p.x + PAD);
    b.minY = Math.min(b.minY, p.y - PAD); b.maxY = Math.max(b.maxY, p.y + PAD);
  }
  if (!isFinite(b.minX)) return { minX: -MIN_SPAN / 2, maxX: MIN_SPAN / 2, minY: -MIN_SPAN / 2, maxY: MIN_SPAN / 2 };
  const grow = (lo, hi) => {
    const extra = Math.max(0, MIN_SPAN - (hi - lo)) / 2;
    return [lo - extra, hi + extra];
  };
  const [minX, maxX] = grow(b.minX, b.maxX), [minY, maxY] = grow(b.minY, b.maxY);
  return { minX, maxX, minY, maxY };
}

function makeView(canvas, b) {
  const w = canvas.clientWidth, h = canvas.clientHeight;
  const scale = Math.min(w / (b.maxX - b.minX), h / (b.maxY - b.minY));
  const cx = (b.minX + b.maxX) / 2, cy = (b.minY + b.maxY) / 2;
  return {
    scale, w, h,
    toPx: (x, y) => [w / 2 + (x - cx) * scale, h / 2 - (y - cy) * scale], // y up
    toWorld: (px, py) => [cx + (px - w / 2) / scale, cy - (py - h / 2) / scale],
  };
}

function drawGrid(ctx, v) {
  const [x0, y1] = v.toWorld(0, 0), [x1, y0] = v.toWorld(v.w, v.h);
  const minStep = v.scale >= 18 ? 1 : 5;
  const label = (m) => `${Math.abs(m) >= 1000 ? `${m / 1000} km` : `${m} m`}`;
  ctx.lineWidth = 1;
  ctx.font = "11px system-ui, sans-serif";
  ctx.fillStyle = css("--muted");
  for (const { v: x, major } of gridLines(x0, x1, minStep).lines) {
    const [px] = v.toPx(x, 0);
    ctx.strokeStyle = css(major ? "--grid-major" : "--grid");
    ctx.beginPath(); ctx.moveTo(px + 0.5, 0); ctx.lineTo(px + 0.5, v.h); ctx.stroke();
    if (major) ctx.fillText(label(x), px + 3, v.h - 4);
  }
  for (const { v: y, major } of gridLines(y0, y1, minStep).lines) {
    const [, py] = v.toPx(0, y);
    ctx.strokeStyle = css(major ? "--grid-major" : "--grid");
    ctx.beginPath(); ctx.moveTo(0, py + 0.5); ctx.lineTo(v.w, py + 0.5); ctx.stroke();
    if (major) ctx.fillText(label(y), 3, py - 3);
  }
}

function drawWaypoint(ctx, v, w, label, color, faded) {
  const [px, py] = v.toPx(w.x, w.y);
  ctx.globalAlpha = faded ? 0.35 : 1;
  ctx.fillStyle = color;
  ctx.beginPath(); ctx.arc(px, py, 7, 0, 2 * Math.PI); ctx.fill();
  ctx.fillStyle = "#fff";
  ctx.font = "bold 10px system-ui, sans-serif";
  ctx.textAlign = "center"; ctx.textBaseline = "middle";
  ctx.fillText(label, px, py + 0.5);
  ctx.textAlign = "start"; ctx.textBaseline = "alphabetic";
  ctx.globalAlpha = 1;
}

function drawPath(ctx, v, points, color, dash) {
  if (points.length < 2) return;
  ctx.strokeStyle = color; ctx.lineWidth = 2; ctx.setLineDash(dash);
  ctx.beginPath();
  points.forEach((p, i) => { const [px, py] = v.toPx(p.x, p.y); i ? ctx.lineTo(px, py) : ctx.moveTo(px, py); });
  ctx.stroke(); ctx.setLineDash([]);
}

function drawTasks(ctx, v, poses) {
  const color = css("--task");
  for (const t of state.tasks) {
    if (!ACTIVE.has(t.state)) continue;
    const rest = t.waypoints.slice(t.leg).filter((w) => w.floor === state.floor);
    const pose = t.robot && poses.get(t.robot);
    const from = pose && pose.floor === state.floor && t.waypoints[t.leg]?.floor === state.floor ? [pose] : [];
    drawPath(ctx, v, [...from, ...rest], color, t.state === "QUEUED" ? [6, 5] : []);
    t.waypoints.forEach((w, i) => {
      if (w.floor === state.floor) drawWaypoint(ctx, v, w, String(i + 1), color, i < t.leg);
    });
    const head = rest[0];
    if (head) {
      const [px, py] = v.toPx(head.x, head.y);
      ctx.fillStyle = color; ctx.font = "11px system-ui, sans-serif";
      ctx.fillText(t.id, px + 9, py - 8);
    }
  }
  const draft = css("--draft");
  drawPath(ctx, v, state.draft, draft, [4, 4]);
  state.draft.forEach((w, i) => drawWaypoint(ctx, v, w, String(i + 1), draft, false));
}

function drawRobot(ctx, v, r, p) {
  const [px, py] = v.toPx(p.x, p.y);
  const rad = Math.max(7, 0.35 * v.scale);
  const color = robotColor(r);
  ctx.globalAlpha = r.conn === "STALE" ? 0.5 : 1;
  ctx.fillStyle = color;
  ctx.beginPath(); ctx.arc(px, py, rad, 0, 2 * Math.PI); ctx.fill();
  if (r.conn === "STALE") {
    ctx.fillStyle = css("--panel"); ctx.fill();
    ctx.strokeStyle = color; ctx.lineWidth = 2; ctx.setLineDash([4, 3]); ctx.stroke(); ctx.setLineDash([]);
  }
  // heading
  ctx.strokeStyle = r.conn === "STALE" ? color : css("--text");
  ctx.lineWidth = 2;
  ctx.beginPath(); ctx.moveTo(px, py);
  ctx.lineTo(px + Math.cos(p.yaw) * rad * 1.7, py - Math.sin(p.yaw) * rad * 1.7);
  ctx.stroke();
  if (r.conn === "OFFLINE") {
    const d = rad * 0.6;
    ctx.strokeStyle = "#fff";
    ctx.beginPath(); ctx.moveTo(px - d, py - d); ctx.lineTo(px + d, py + d);
    ctx.moveTo(px + d, py - d); ctx.lineTo(px - d, py + d); ctx.stroke();
  }
  ctx.globalAlpha = 1;
  ctx.fillStyle = css("--text");
  ctx.font = "12px system-ui, sans-serif";
  ctx.textAlign = "center";
  ctx.fillText(r.name + (r.needs_charge ? " ⚡" : ""), px, py + rad + 13);
  ctx.textAlign = "start";
}

function drawDocks(ctx, v) {
  ctx.strokeStyle = css("--charging"); ctx.lineWidth = 1.5;
  for (const d of state.docks.values()) {
    if (d.floor !== state.floor) continue;
    const [px, py] = v.toPx(d.x, d.y);
    ctx.strokeRect(px - 9, py - 9, 18, 18);
  }
}

function render() {
  const canvas = $("map");
  const dpr = window.devicePixelRatio || 1;
  const w = canvas.clientWidth, h = canvas.clientHeight;
  if (canvas.width !== Math.round(w * dpr) || canvas.height !== Math.round(h * dpr)) {
    canvas.width = Math.round(w * dpr); canvas.height = Math.round(h * dpr);
  }
  const ctx = canvas.getContext("2d");
  ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
  ctx.clearRect(0, 0, w, h);
  const v = (state.view = makeView(canvas, updateBounds()));
  drawGrid(ctx, v);
  drawDocks(ctx, v);

  const t = renderTime();
  const poses = new Map();
  for (const [name, r] of state.robots) {
    const p = poseAt(r.track, t, r.v);
    if (p) poses.set(name, p);
  }
  drawTasks(ctx, v, poses);
  for (const [name, p] of poses) {
    if (p.floor === state.floor) drawRobot(ctx, v, state.robots.get(name).v, p);
  }
  requestAnimationFrame(render);
}

// ------------------------------------------------------------ interaction

function worldAt(ev) {
  const rect = $("map").getBoundingClientRect();
  const [x, y] = state.view.toWorld(ev.clientX - rect.left, ev.clientY - rect.top);
  return { x: Math.round(x * 10) / 10, y: Math.round(y * 10) / 10 };
}

function renderDraft() {
  const n = state.draft.length;
  $("draft-send").hidden = $("draft-clear").hidden = n === 0;
  $("draft-send").className = "primary";
  $("draft-send").textContent = `작업 보내기 (${n}개 지점)`;
  $("draft-info").textContent = n
    ? `Shift+클릭으로 지점 추가 중: ${state.draft.map((w) => `(${w.x}, ${w.y})`).join(" → ")}`
    : "지도를 클릭하면 그 지점으로 작업을 보냅니다(3초 안에 취소 가능). Shift+클릭으로 여러 지점을 모읍니다.";
}

function setupInput() {
  const canvas = $("map");
  canvas.addEventListener("click", async (ev) => {
    if (!state.view || state.floor === null) return;
    const wp = { ...worldAt(ev), floor: state.floor };
    if (ev.shiftKey) {
      state.draft.push(wp);
      renderDraft();
      return;
    }
    const t = await submitTask([wp]);
    if (t) undoToast(t);
  });
  canvas.addEventListener("mousemove", (ev) => {
    if (!state.view) return;
    const p = worldAt(ev);
    $("cursor").textContent = `${state.floor ?? "?"}층 x ${p.x.toFixed(1)} m, y ${p.y.toFixed(1)} m`;
  });
  $("draft-send").addEventListener("click", async () => {
    if (await submitTask(state.draft)) { state.draft = []; renderDraft(); }
  });
  $("draft-clear").addEventListener("click", () => { state.draft = []; renderDraft(); });
  $("floors").addEventListener("click", (ev) => {
    const f = ev.target.dataset.floor;
    if (f !== undefined) selectFloor(Number(f));
  });
  for (const id of ["tasks", "toast"]) {
    $(id).addEventListener("click", (ev) => {
      const task = ev.target.dataset.cancel;
      if (!task) return;
      if (id === "toast") $("toast").hidden = true;
      cancelTask(task);
    });
  }
}

// ------------------------------------------------------------------ start

setupInput();
renderDraft();
connect("/ws/live", onFrame, (up, why) => setUp("live", up, why), LIVE_SILENCE_MS);
connect("/ws", (e) => feed.push(e), (up) => { setUp("events", up); if (up) onEventsUp(); });
refreshTasks();
requestAnimationFrame(render);

}
