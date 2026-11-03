// Tests for the pure parts of web/app.js. Run: node scripts/web_test.js
// (no packages needed). Exits non-zero on the first failure.
"use strict";
const assert = require("node:assert/strict");
const path = require("node:path");
const { poseAt, gridLines, inRange, EventFeed, carSlots, ridersInCars, carBox, elevatorEvent, carAlarms } =
  require(path.join(__dirname, "..", "web", "app.js"));

let passed = 0;
const tests = [];
const test = (name, fn) => tests.push([name, fn]);
const near = (a, b, eps = 1e-9) => assert.ok(Math.abs(a - b) < eps, `${a} != ${b}`);

// ------------------------------------------------------------------ poseAt

const track = [
  { t: 1000, x: 0, y: 0, yaw: 0, floor: 1 },
  { t: 1200, x: 2, y: 0, yaw: 0, floor: 1 },
  { t: 1400, x: 2, y: 2, yaw: Math.PI / 2, floor: 1 },
];
const moving = { v: 1, mode: "MOVING" };

test("poseAt: no samples", () => assert.equal(poseAt([], 1000, moving), null));
test("poseAt: before the first sample holds it", () => assert.deepEqual(poseAt(track, 500, moving), track[0]));
test("poseAt: on a sample", () => {
  const p = poseAt(track, 1200, moving);
  near(p.x, 2); near(p.y, 0);
});
test("poseAt: interpolates between samples", () => {
  const p = poseAt(track, 1100, moving);
  near(p.x, 1); near(p.y, 0); near(p.yaw, 0);
  const q = poseAt(track, 1300, moving);
  near(q.x, 2); near(q.y, 1); near(q.yaw, Math.PI / 4);
});
test("poseAt: yaw turns the short way across ±π", () => {
  const tr = [{ t: 0, x: 0, y: 0, yaw: 3.0, floor: 1 }, { t: 100, x: 0, y: 0, yaw: -3.0, floor: 1 }];
  const p = poseAt(tr, 50, null);
  // halfway between 3.0 and -3.0 through π, not through 0
  near(Math.abs(Math.atan2(Math.sin(p.yaw), Math.cos(p.yaw))), Math.PI, 1e-6);
});
test("poseAt: two samples at the same time give no NaN", () => {
  const tr = [{ t: 100, x: 1, y: 1, yaw: 0, floor: 1 }, { t: 100, x: 3, y: 1, yaw: 0, floor: 1 }];
  const p = poseAt(tr, 100, null);
  assert.ok(Number.isFinite(p.x) && Number.isFinite(p.y));
});
test("poseAt: past the newest sample a moving robot goes on along its heading", () => {
  const p = poseAt(track, 1500, moving); // 100 ms at 1 m/s, heading +y
  near(p.x, 2, 1e-9); near(p.y, 2.1, 1e-9);
});
test("poseAt: extrapolation stops after 250 ms", () => {
  const p = poseAt(track, 5000, moving);
  near(p.y, 2.25, 1e-9);
});
test("poseAt: a stopped or idle robot is not extrapolated", () => {
  assert.deepEqual(poseAt(track, 1500, { v: 0, mode: "MOVING" }), track[2]);
  assert.deepEqual(poseAt(track, 1500, { v: 1, mode: "IDLE" }), track[2]);
  assert.deepEqual(poseAt(track, 1500, undefined), track[2]);
});

// --------------------------------------------------------------- gridLines

test("gridLines: 1 m lines over a small range, every 5th major", () => {
  const { step, lines } = gridLines(-2.5, 7.2, 1);
  assert.equal(step, 1);
  assert.deepEqual(lines.map((l) => l.v), [-2, -1, 0, 1, 2, 3, 4, 5, 6, 7]);
  assert.deepEqual(lines.filter((l) => l.major).map((l) => l.v), [0, 5]);
});
test("gridLines: the step grows so there are at most 200 lines", () => {
  for (const span of [1e3, 2e5, 2e6, 1e17, 1e300]) {
    const { step, lines } = gridLines(-span / 2, span / 2, 1);
    assert.ok(lines.length <= 201, `${lines.length} lines for span ${span}`);
    assert.ok(lines.length >= 2, `${lines.length} lines for span ${span}`);
    assert.ok(/^[125]/.test(String(step)) || step >= 1e21, `step ${step}`);
  }
});
test("gridLines: far from the origin (float precision) still ends", () => {
  const { lines } = gridLines(1e17, 1e17 + 50, 1); // 1e17 + 1 === 1e17 in floats
  assert.ok(lines.length <= 201);
});
test("gridLines: non-finite or empty ranges give no lines", () => {
  assert.deepEqual(gridLines(NaN, 5, 1).lines, []);
  assert.deepEqual(gridLines(0, Infinity, 1).lines, []);
  assert.deepEqual(gridLines(5, 1, 1).lines, []);
});
test("inRange: map bounds ignore points beyond 1e6 m", () => {
  assert.ok(inRange({ x: 1e6, y: -1e6 }));
  assert.ok(!inRange({ x: 1e6 + 1, y: 0 }));
  assert.ok(!inRange({ x: 0, y: NaN }));
});

// --------------------------------------------------------------- EventFeed

// A fetchSince the test answers by hand.
function harness(server) {
  const shown = [], missed = [], calls = [];
  const fetchSince = (seq) => new Promise((resolve, reject) => calls.push({ seq, resolve, reject }));
  const feed = new EventFeed(fetchSince, (e) => shown.push(e.seq), (n) => missed.push(n));
  const answer = async (i = calls.length - 1, have = server) => {
    const c = calls[i];
    c.resolve(have.filter((s) => s > c.seq).map((seq) => ({ seq })));
    await new Promise((r) => setImmediate(r));
  };
  return { feed, shown, missed, calls, answer };
}
const ev = (seq) => ({ seq });

test("EventFeed: in-order events show at once without fetching", () => {
  const h = harness([]);
  h.feed.shown = 0;
  [1, 2, 3].forEach((s) => h.feed.push(ev(s)));
  assert.deepEqual(h.shown, [1, 2, 3]);
  assert.equal(h.calls.length, 0);
});
test("EventFeed: a gap is filled and shown in order (review 5-3)", async () => {
  const h = harness([1, 2, 3, 4, 5]);
  h.feed.push(ev(1)); h.feed.push(ev(2)); h.feed.push(ev(5));
  assert.deepEqual(h.shown, [1, 2]); // 5 waits for 3 and 4
  assert.equal(h.calls[0].seq, 2);
  await h.answer();
  assert.deepEqual(h.shown, [1, 2, 3, 4, 5]);
  assert.deepEqual(h.missed, []);
});
test("EventFeed: events arriving during a fill are not lost", async () => {
  const server = [1, 2, 3, 4, 5];
  const h = harness(server);
  h.feed.push(ev(1)); h.feed.push(ev(4)); // 2, 3 missing
  h.feed.push(ev(7));                    // 5, 6 missing too, arrives mid-fill
  server.push(6, 7);
  await h.answer(0, [1, 2, 3, 4, 5]);     // first fetch saw only up to 5
  assert.deepEqual(h.shown, [1, 2, 3, 4, 5]);
  assert.equal(h.calls.length, 2);        // fetched again for 6
  await h.answer(1);
  assert.deepEqual(h.shown, [1, 2, 3, 4, 5, 6, 7]);
});
test("EventFeed: duplicates from fill and socket are shown once", async () => {
  const h = harness([1, 2, 3]);
  h.feed.push(ev(1)); h.feed.push(ev(3));
  h.feed.push(ev(3));
  await h.answer();
  h.feed.push(ev(2)); h.feed.push(ev(3));
  assert.deepEqual(h.shown, [1, 2, 3]);
});
test("EventFeed: events the server no longer keeps are skipped and counted", async () => {
  const window = Array.from({ length: 11 }, (_, i) => 40 + i); // the server keeps 40..50
  const h = harness(window);
  h.feed.push(ev(1)); h.feed.push(ev(50));
  await h.answer();
  assert.deepEqual(h.shown, [1, ...window]);
  assert.deepEqual(h.missed, [38]);
});
test("EventFeed: first load shows history without a 'missed' note", async () => {
  const h = harness([300, 301]);
  h.feed.fill();
  await h.answer();
  assert.deepEqual(h.shown, [300, 301]);
  assert.deepEqual(h.missed, []);
});
test("EventFeed: a failed fetch keeps held events for the next try", async () => {
  const h = harness([1, 2, 3]);
  h.feed.push(ev(1)); h.feed.push(ev(3));
  h.calls[0].reject(new Error("offline"));
  await new Promise((r) => setImmediate(r));
  assert.deepEqual(h.shown, [1]);
  h.feed.fill();
  await h.answer();
  assert.deepEqual(h.shown, [1, 2, 3]);
});
test("EventFeed: a restarted server (lower or no events) starts the numbering over", () => {
  const h = harness([]);
  [1, 2, 3].forEach((s) => h.feed.push(ev(s)));
  assert.equal(h.feed.reconnected(3), false); // same server, nothing new
  assert.equal(h.feed.reconnected(9), false); // same server, more events
  assert.equal(h.feed.reconnected(undefined), true); // restarted, no events yet
  h.feed.push(ev(1));
  assert.deepEqual(h.shown, [1, 2, 3, 1]);
  [2, 3, 4].forEach((s) => h.feed.push(ev(s)));
  assert.equal(h.feed.reconnected(2), true); // restarted again, 2 events so far
  assert.equal(h.feed.reconnected(undefined), false); // nothing shown yet: nothing to reset
});
test("EventFeed: a new server run id starts over even if its seq is already higher", () => {
  const h = harness([]);
  assert.equal(h.feed.reconnected(undefined, "run1"), false); // first connection
  [1, 2, 3].forEach((s) => h.feed.push(ev(s)));
  assert.equal(h.feed.reconnected(3, "run1"), false);
  assert.equal(h.feed.reconnected(7, "run2"), true); // restarted and got 7 events quickly
  h.feed.push(ev(1));
  assert.deepEqual(h.shown, [1, 2, 3, 1]);
  assert.equal(h.feed.reconnected(1, "run2"), false);
});

// --------------------------------------------------------------- elevators

const rider = (robot, status, slot, exit, dest = exit) => ({ robot, status, slot, exit, dest, board: 2 - slot });
const liftView = (over = {}) => ({
  id: "E1", capacity: 3, floors: [1, 2, 3, 4, 5, 6, 7], lobby: { x: 10, y: 0 }, responding: true, waiting: [],
  state: { floor: 5, dir: "idle", door: "open", moving: false, hold: true, trip: "E1-k3-1", load: 2, door_zone: 0 },
  trip: { trip: "E1-k3-1", stage: "exiting", floor: 5, stops: [3, 5, 7], riders: [
    rider("robot_2", "inside", 2, 7), rider("robot_3", "exiting", 1, 5), rider("robot_1", "out", 0, 3)],
    lease: "2026-11-03T09:02:30Z" },
  ...over,
});
const T = Date.parse("2026-11-03T09:00:40Z");

test("carSlots: robots by slot from the door, out and waiting robots left out", () => {
  const s = carSlots(liftView());
  assert.equal(s.length, 3);
  assert.equal(s[0], null); // robot_1 is out
  assert.deepEqual(s[1], { robot: "robot_3", exit: 5, dest: 5, status: "exiting", carried: false });
  assert.equal(s[2].robot, "robot_2");
});
test("carSlots: a carried robot is marked, a bad slot number is ignored, no trip is empty", () => {
  const v = liftView();
  v.trip.riders = [rider("robot_1", "inside", 0, 5, 3), rider("robot_9", "inside", 7, 5), rider("robot_8", "waiting", 1, 5)];
  const s = carSlots(v);
  assert.ok(s[0].carried && s[0].exit === 5 && s[0].dest === 3);
  assert.deepEqual(s.slice(1), [null, null]);
  assert.deepEqual(carSlots(liftView({ trip: null })), [null, null, null]);
  assert.deepEqual(carSlots(undefined), []);
});
test("ridersInCars: who is drawn in which car, at the car's floor", () => {
  const m = ridersInCars([liftView(), liftView({ id: "E2", state: null, trip: { trip: "E2-x-1", floor: 2, riders: [rider("r9", "boarding", 0, 4)] } })]);
  assert.deepEqual([...m.keys()].sort(), ["r9", "robot_2", "robot_3"]);
  assert.deepEqual(m.get("robot_2"), { car: "E1", slot: 2, floor: 5, status: "inside" });
  assert.equal(m.get("r9").floor, 2); // no state yet: the trip's floor
  for (const st of ["unsure_in", "stuck"]) {
    const v = liftView();
    v.trip.riders = [rider("robot_1", st, 0, 3)];
    assert.ok(ridersInCars([v]).has("robot_1"), st);
  }
});
test("carBox: door next to the lobby, slot 0 nearest the door, inside the box", () => {
  const b = carBox({ x: 10, y: 0 }, 3);
  assert.equal(b.x0, 11);
  const s = [0, 1, 2].map(b.slot);
  assert.ok(s[0].x < s[1].x && s[1].x < s[2].x);
  for (const p of s) assert.ok(p.x > b.x0 && p.x < b.x1 && p.y > b.y0 && p.y < b.y1);
  // same numbers as the adapter drives the robots to (fleet_logic.car_slot)
  near(s[0].x, 11.8); near(s[2].x, 13.4);
});
test("elevatorEvent: kind, trip and text from the detail", () => {
  assert.deepEqual(elevatorEvent({ detail: "overdue: E1-k3x9ab-1: car not answering at floor 5" }),
    { kind: "overdue", trip: "E1-k3x9ab-1", text: "car not answering at floor 5" });
  assert.deepEqual(elevatorEvent({ detail: "released" }), { kind: "released", trip: "", text: "" });
  assert.equal(elevatorEvent({ detail: "stop: floor 3, 1 to get out" }).trip, "");
  assert.deepEqual(elevatorEvent({}), { kind: "", trip: "", text: "" });
});
test("carAlarms: nothing when all is well", () => {
  assert.deepEqual(carAlarms(liftView(), undefined, T), []);
});
test("carAlarms: silent car, stuck robot, blocked, lease, turn deadline, overdue of this trip only", () => {
  const v = liftView({ responding: false });
  v.trip.stage = "blocked";
  v.trip.detail = "robot_2 could not get out";
  v.trip.riders[0].status = "stuck";
  v.trip.turn = { n: 6, robot: "robot_2", action: "exit", deadline: "2026-11-03T09:00:39Z" };
  v.trip.lease = "2026-11-03T09:00:00Z";
  const got = carAlarms(v, { trip: "E1-k3-1", text: "turn 6 timed out" }, T);
  const texts = got.map((a) => a.text).join(" | ");
  for (const want of ["응답 없음", "막힘", "robot_2 갇힘", "임대", "차례 6 시간 초과", "지연: turn 6"]) {
    assert.ok(texts.includes(want), `${want} missing in ${texts}`);
  }
  assert.ok(got[0].bad);
  assert.deepEqual(carAlarms(liftView(), { trip: "E1-old-1", text: "x" }, T), []); // another trip's overdue
});
test("carAlarms: two in the door zone, backoff", () => {
  const v = liftView({ backoff_until: "2026-11-03T09:01:00Z" });
  v.state.door_zone = 2;
  const texts = carAlarms(v, undefined, T).map((a) => a.text);
  assert.ok(texts.some((t) => t.includes("문 구역에 2대")) && texts.some((t) => t.includes("쉬는 중")));
});

(async () => {
  for (const [name, fn] of tests) {
    try {
      await fn();
      passed++;
    } catch (err) {
      console.error(`FAIL  ${name}\n${err.stack}`);
      process.exit(1);
    }
  }
  console.log(`${passed}/${tests.length} web tests passed`);
})();
