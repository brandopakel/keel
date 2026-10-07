// BullMQ against the server at REDIS_URL: a queue, a worker, a job and a
// delayed job, with results read back through queue events.
import { Queue, QueueEvents, Worker } from 'bullmq';

const url = new URL(process.env.REDIS_URL);
const connection = { host: url.hostname, port: Number(url.port), maxRetriesPerRequest: null };
const fail = (message) => {
  console.error(message);
  process.exit(1);
};
setTimeout(() => fail('timed out'), 30000).unref();

const queue = new Queue('check', { connection });
const events = new QueueEvents('check', { connection });
const worker = new Worker('check', async (job) => job.data.x * 2, { connection });
for (const emitter of [queue, events, worker]) {
  emitter.on('error', (error) => fail(String(error?.stack ?? error)));
}
try {
  await events.waitUntilReady();
  const job = await queue.add('double', { x: 21 });
  const result = await job.waitUntilFinished(events, 20000);
  if (result !== 42) fail(`a job returned ${result}, want 42`);
  const delayed = await queue.add('double', { x: 5 }, { delay: 500 });
  const later = await delayed.waitUntilFinished(events, 20000);
  if (later !== 10) fail(`a delayed job returned ${later}, want 10`);
  console.log('ok: a job and a delayed job ran');
} catch (error) {
  fail(String(error?.stack ?? error));
}
await Promise.allSettled([worker.close(), events.close(), queue.close()]);
process.exit(0);
