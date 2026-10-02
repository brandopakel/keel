// Default-configuration probe for node-redis and ioredis: only host, port and password are set.
import { createRequire } from 'node:module';
const require = createRequire(import.meta.url);
const port = Number(process.argv[2]), server = process.argv[3];
const password = process.env.PROBE_PASSWORD || undefined;
const scenarios = ['default', 'named', 'pipeline', 'tx', 'close'];
const withTimeout = (p, ms = 5000) => Promise.race([p, new Promise((_, rej) => setTimeout(() => rej(new Error('timeout')), ms))]);
const emit = (library, scenario, err) => console.log(JSON.stringify({ library, server, scenario, ok: !err, error: err ? String(err?.message ?? err) : null }));

async function nodeRedis(pkg) {
  const { createClient } = require(pkg);
  const lib = `node-redis ${require(`${pkg}/package.json`).version}`;
  for (const scen of scenarios) {
    const key = `probe:${lib}:${scen}`;
    const opts = { socket: { host: '127.0.0.1', port, connectTimeout: 3000, reconnectStrategy: false }, password };
    if (scen === 'named') opts.name = 'probe';
    const c = createClient(opts);
    c.on('error', () => {});
    let err = null;
    try {
      await withTimeout(c.connect());
      if (scen === 'pipeline') {
        const [, v] = await withTimeout(Promise.all([c.set(key, 'v'), c.get(key)]));
        if (v !== 'v') throw new Error(`GET mismatch ${v}`);
      } else if (scen === 'tx') {
        const r = await withTimeout(c.multi().set(key, 'v').get(key).exec());
        if (r[1] !== 'v') throw new Error(`GET mismatch ${r[1]}`);
      } else {
        await withTimeout(c.set(key, 'v'));
        const v = await withTimeout(c.get(key));
        if (v !== 'v') throw new Error(`GET mismatch ${v}`);
      }
      if (scen === 'close') await withTimeout(c.quit());
    } catch (e) { err = e; }
    try { c.destroy ? c.destroy() : await c.disconnect(); } catch {}
    emit(lib, scen, err);
  }
}

async function ioredis(pkg) {
  const m = require(pkg);
  const Redis = m.default ?? m.Redis ?? m;
  const lib = `ioredis ${require(`${pkg}/package.json`).version}`;
  for (const scen of scenarios) {
    const key = `probe:${lib}:${scen}`;
    const opts = { host: '127.0.0.1', port, password, connectTimeout: 3000, maxRetriesPerRequest: 0, retryStrategy: () => null, enableOfflineQueue: true };
    if (scen === 'named') opts.connectionName = 'probe';
    const c = new Redis(opts);
    c.on('error', () => {});
    let err = null;
    try {
      if (scen === 'pipeline') {
        const r = await withTimeout(c.pipeline().set(key, 'v').get(key).exec());
        const bad = r.find(([e]) => e); if (bad) throw bad[0];
        if (r[1][1] !== 'v') throw new Error(`GET mismatch ${r[1][1]}`);
      } else if (scen === 'tx') {
        const r = await withTimeout(c.multi().set(key, 'v').get(key).exec());
        const bad = r?.find(([e]) => e); if (bad) throw bad[0];
        if (r[1][1] !== 'v') throw new Error(`GET mismatch ${r[1][1]}`);
      } else {
        await withTimeout(c.set(key, 'v'));
        const v = await withTimeout(c.get(key));
        if (v !== 'v') throw new Error(`GET mismatch ${v}`);
      }
      if (scen === 'close') await withTimeout(c.quit());
    } catch (e) { err = e; }
    try { c.disconnect(); } catch {}
    emit(lib, scen, err);
  }
}

await nodeRedis('redis');
await nodeRedis('redis4');
await ioredis('ioredis');
await ioredis('ioredis5');
process.exit(0);
