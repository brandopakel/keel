"use k6 with k6/x/redis = v0.4.3";

// A k6 load test of one server that speaks Redis's protocol, Keel or Redis,
// through five scenarios run one after another:
//
// - steady: GET-heavy load on 10,000 keys with 64-byte values;
// - big_values: writes and reads of 512 KiB values;
// - hot_key: INCR on one key from many virtual users at once;
// - expiry: keys set with a 200 ms TTL must be gone 400 ms later;
// - eviction: 16 KiB writes over far more keys than maxmemory holds.
//
// Each scenario's checks must pass at least 99% of the time (thresholds), and
// its iteration time is reported per scenario. The workflow
// (.github/workflows/k6.yml) runs it against Keel and then Redis with the same
// maxmemory and policy, tagged server=keel or server=redis, and streams the
// results to Grafana Cloud. It is not a speed comparison: both servers share
// one unpinned runner, and the paired benchmarks compare speed.
//
// REDIS_URL names the server. SCALE (default 1) multiplies every scenario's
// length, so a local run can be short.
import redis from 'k6/x/redis';
import { check, sleep } from 'k6';
import { Counter } from 'k6/metrics';

const client = new redis.Client(__ENV.REDIS_URL || 'redis://127.0.0.1:6379');
const scale = Number(__ENV.SCALE || 1);
const seconds = (n) => `${Math.max(1, Math.round(n * scale))}s`;
const refused = new Counter('keel_refused_commands');
let warned = 0;

// Scenarios follow one another, each starting when the one before ends.
let start = 0;
function after(duration, scenario) {
  const begins = `${start}s`;
  start += Number(duration.slice(0, -1));
  return { ...scenario, duration, startTime: begins };
}

export const options = {
  scenarios: {
    steady: after(seconds(60), { executor: 'constant-vus', vus: 20, exec: 'steady' }),
    big_values: after(seconds(30), { executor: 'constant-vus', vus: 4, exec: 'bigValues' }),
    hot_key: after(seconds(30), { executor: 'constant-vus', vus: 30, exec: 'hotKey' }),
    expiry: after(seconds(30), { executor: 'constant-vus', vus: 10, exec: 'expiry' }),
    eviction: after(seconds(45), { executor: 'constant-vus', vus: 10, exec: 'eviction' }),
  },
  thresholds: Object.fromEntries(
    ['steady', 'big_values', 'hot_key', 'expiry', 'eviction'].map((s) => [`checks{scenario:${s}}`, ['rate>0.99']]),
  ),
};

const small = 'v'.repeat(64);
const big = 'b'.repeat(512 * 1024);
const medium = 'm'.repeat(16 * 1024);
const pick = (n) => Math.floor(Math.random() * n);

// A command the server refused is counted by its error, so that a missing
// command shows as such rather than only as a failed check.
async function attempt(fn) {
  try {
    return { ok: true, value: await fn() };
  } catch (error) {
    // This client reports a missing key as the error "redis: nil".
    if (String(error).includes('redis: nil')) {
      return { ok: true, value: null };
    }
    refused.add(1, { error: String(error).slice(0, 60) });
    if (warned++ === 0) {
      console.warn(`first refusal on VU ${__VU}: ${error}`);
    }
    return { ok: false, value: undefined };
  }
}

export async function steady() {
  const key = `k6:steady:${pick(10000)}`;
  if (Math.random() < 0.2) {
    const set = await attempt(() => client.set(key, small, 0));
    check(set, { 'SET answers OK': (r) => r.ok && r.value === 'OK' });
  } else {
    const get = await attempt(() => client.get(key));
    check(get, { 'GET answers, a value or nil': (r) => r.ok && (r.value === null || r.value === small) });
  }
}

export async function bigValues() {
  const key = `k6:big:${__VU}`;
  const set = await attempt(() => client.set(key, big, 0));
  const get = await attempt(() => client.get(key));
  check(get, { 'a 512 KiB value reads back whole': (r) => set.ok && r.ok && r.value.length === big.length });
}

export async function hotKey() {
  const incr = await attempt(() => client.incr('k6:hot'));
  check(incr, { 'INCR on a contended key counts up': (r) => r.ok && r.value > 0 });
}

export async function expiry() {
  const key = `k6:expiry:${__VU}:${__ITER}`;
  const set = await attempt(() => client.sendCommand('SET', key, small, 'PX', 200));
  const present = await attempt(() => client.exists(key));
  sleep(0.4);
  const gone = await attempt(() => client.exists(key));
  check(gone, {
    'a key is there before its TTL and gone after it': (r) => set.ok && present.value === 1 && r.ok && r.value === 0,
  });
}

export async function eviction() {
  const set = await attempt(() => client.set(`k6:evict:${pick(200000)}`, medium, 0));
  check(set, { 'writes past maxmemory still answer OK': (r) => r.ok && r.value === 'OK' });
}
