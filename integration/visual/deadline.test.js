import test from 'node:test';
import assert from 'node:assert/strict';
import {withDeadline} from './deadline.js';

test('deadline rejects a renderer that never resolves', async () => {
  await assert.rejects(withDeadline(new Promise(() => {}), 5, 'render'), /render exceeded 5ms/);
});
test('deadline preserves completed results and failures', async () => {
  assert.equal(await withDeadline(Promise.resolve('done'), 1000, 'render'), 'done');
  const failure = new Error('original failure');
  await assert.rejects(withDeadline(Promise.reject(failure), 1000, 'render'), error => error === failure);
});
