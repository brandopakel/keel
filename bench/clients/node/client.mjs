import fs from 'node:fs';
import assert from 'node:assert/strict';
import { createClient, RESP_TYPES } from 'redis';
import IORedis from 'ioredis';

const [library, path] = process.argv.slice(2);
const fixture = JSON.parse(fs.readFileSync(path, 'utf8'));
const [host, rawPort] = process.env.KEEL_COMPAT_ADDR.split(':');
const password = process.env.KEEL_COMPAT_PASSWORD || undefined;
const port = Number(rawPort);
const argument = x => typeof x === 'object' ? Buffer.from(x.hex, 'hex') : String(x);
const decoder = new TextDecoder('utf-8', { fatal: true });
function normalize(x) {
  if (Buffer.isBuffer(x)) {
    try { return decoder.decode(x); } catch { return { hex: x.toString('hex') }; }
  }
  return Array.isArray(x) ? x.map(normalize) : x;
}
async function connect() {
  if (library === 'node-redis') {
    const client = createClient({ RESP: 2, socket: { host, port, connectTimeout: 3000, reconnectStrategy: false }, password })
      .withTypeMapping({ [RESP_TYPES.BLOB_STRING]: Buffer });
    client.on('error', error => process.stderr.write(`${error.message}\n`));
    await client.connect();
    return { call: args => client.sendCommand(args.map(argument)),
      pipeline: key => Promise.all(Array.from({ length: 257 }, () => client.incr(key))),
      close: () => client.close() };
  }
  assert.equal(library, 'ioredis');
  const client = new IORedis({ host, port, password, protocol: 2, lazyConnect: true, retryStrategy: null,
    connectTimeout: 3000, commandTimeout: 3000 });
  client.on('error', error => process.stderr.write(`${error.message}\n`));
  await client.connect();
  return { call: args => client.callBuffer(...args.map(argument)),
    pipeline: async key => {
      const pipeline = client.pipeline();
      for (let i = 0; i < 257; i++) pipeline.incr(key);
      const results = await pipeline.exec();
      return results.map(([error, result]) => { if (error) throw error; return result; });
    }, close: () => client.disconnect() };
}
let client = await connect();
async function checkCase(test) {
  let result;
  try { result = normalize(await client.call(test.args)); }
  catch (error) { assert.ok(test.error && error.message.includes(test.error), `${test.name}: ${error}`); return; }
  assert.ok(!test.error, test.name);
  if (test.range) assert.ok(result >= test.range[0] && result <= test.range[1], test.name);
  else assert.deepEqual(result, test.expected, test.name);
}
try {
  if (!fixture.verify_only) {
    for (const test of fixture.commands) await checkCase(test);
    const deadline = Date.now() + 3000;
    while (await client.call(['GET', fixture.expiry_key]) !== null) {
      assert.ok(Date.now() < deadline, 'expiry did not occur');
      await new Promise(resolve => setTimeout(resolve, 10));
    }
    assert.deepEqual(await client.pipeline(fixture.counter_key), Array.from({ length: 257 }, (_, i) => i+1));
    assert.equal(normalize(await client.call(['GET', fixture.counter_key])), '257');
    const found = new Set(); let cursor = '0';
    do {
      const result = normalize(await client.call(['SCAN', cursor, 'MATCH', fixture.prefix+'*', 'COUNT', '7', 'TYPE', 'STRING']));
      cursor = String(result[0]); result[1].forEach(x => found.add(x));
    } while (cursor !== '0');
    assert.deepEqual([...found].sort(), fixture.scan_keys.toSorted());
  }
  for (const test of fixture.verification) await checkCase(test);
  await client.close(); client = await connect();
  assert.deepEqual(normalize(await client.call(['GET', fixture.marker_key])), fixture.marker_value, 'reconnect');
  const pkg = library === 'node-redis' ? 'redis' : 'ioredis';
  const version = JSON.parse(fs.readFileSync(new URL(`node_modules/${pkg}/package.json`, import.meta.url))).version;
  console.log(JSON.stringify({ library, version, status: 'passed', verify_only: fixture.verify_only }));
} finally { await client.close(); }
