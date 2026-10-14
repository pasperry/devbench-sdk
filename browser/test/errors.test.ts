import { test } from 'node:test';
import assert from 'node:assert/strict';
import { installErrorCapture, isContentFreeConsoleLine, parseStack } from '../src/capture/errors.js';
import { Sensor } from '../src/sensor.js';
import { resetForTests } from '../src/safe.js';

test('parses V8 stacks', () => {
  const frames = parseStack([
    'TypeError: boom',
    '    at CustomerCtrl.save (https://app.example.com/assets/js/app.js:1204:9)',
    '    at https://app.example.com/assets/js/app.js:88:3',
  ].join('\n'));

  assert.equal(frames.length, 2);
  assert.equal(frames[0]!.function, 'CustomerCtrl.save');
  assert.equal(frames[0]!.file, 'https://app.example.com/assets/js/app.js');
  assert.equal(frames[0]!.line, 1204);
});

test('parses SpiderMonkey stacks', () => {
  const frames = parseStack('save@https://app.example.com/app.js:12:9');
  assert.equal(frames[0]!.function, 'save');
  assert.equal(frames[0]!.line, 12);
});

test('tolerates absent or unparseable stacks', () => {
  assert.deepEqual(parseStack(undefined), []);
  assert.deepEqual(parseStack('complete gibberish'), []);
});

// Replacing the app's console.error — or any other tool's — would itself create
// a silent failure. We wrap, and the original always runs first.
test('console.error is wrapped, never replaced', () => {
  resetForTests();
  const seen: unknown[][] = [];
  const fake = { error: (...args: unknown[]) => { seen.push(args); } };

  const sensor = new Sensor({ endpoint: 'http://127.0.0.1:9/v1/flush', tenant: 'acme', ingestKey: 'adt_client_test' });
  const uninstall = installErrorCapture(sensor, { console: fake });

  fake.error('something broke', 42);

  assert.equal(seen.length, 1, 'the original console.error must still be called');
  assert.deepEqual(seen[0], ['something broke', 42]);
  assert.equal(sensor.stats().distinctFingerprints, 1, 'and the signal must be recorded');

  uninstall();
  fake.error('after uninstall');
  assert.equal(seen.length, 2, 'the original survives uninstall');
  assert.equal(sensor.stats().distinctFingerprints, 1, 'no signal after uninstall');
});

test('a throwing sensor cannot break console.error', () => {
  resetForTests();
  const seen: unknown[][] = [];
  const fake = { error: (...args: unknown[]) => { seen.push(args); } };

  const sensor = new Sensor({
    endpoint: 'http://127.0.0.1:9/v1/flush', tenant: 'acme', ingestKey: 'adt_client_test',
    now: () => { throw new Error('boom'); },
  });
  installErrorCapture(sensor, { console: fake });

  assert.doesNotThrow(() => fake.error('app still works'));
  assert.equal(seen.length, 1);
  resetForTests();
});

// #518: content-free console.error lines (banners, `[object Event]`, a bare
// `Error code: N`) merged unrelated incidents into one fingerprint. They are
// not recorded; the app's console still sees them, and lines that say
// something are recorded as before.
test('content-free console.error lines are not recorded', () => {
  resetForTests();
  const seen: unknown[][] = [];
  const fake = { error: (...args: unknown[]) => { seen.push(args); } };
  const sensor = new Sensor({ endpoint: 'http://127.0.0.1:9/v1/flush', tenant: 'acme', ingestKey: 'adt_client_test' });
  const uninstall = installErrorCapture(sensor, { console: fake });

  const noise: unknown[][] = [
    ['=================================================='],
    ['----', '****'],
    ['═══════════'],
    [],
    [''],
    [{ toString: () => '[object Event]' }],
    ['[object Event]'],
    [{}],
    ['Error code: 31005'],
    ['Error code: 20101'],
    ['Error code:', 31009],
    ['error: 20104'],
    ['code 31009.'],
    [42],
  ];
  for (const args of noise) fake.error(...args);
  assert.equal(seen.length, noise.length, 'the original console.error still sees every line');
  assert.equal(sensor.stats().distinctFingerprints, 0, 'no content-free line is recorded');

  fake.error('Error code: 31005 (ConnectionError: websocket closed)');
  fake.error('Twilio device error', { code: 31005 });
  fake.error('[object Event] while loading the call');
  fake.error(new Error(''));
  assert.equal(sensor.stats().distinctFingerprints, 4, 'lines with content, and Errors, are recorded');
  uninstall();
  resetForTests();
});

test('isContentFreeConsoleLine', () => {
  for (const line of ['=====', '', '   ', '12345', '--- 2026-10-14 ---', '>>>> <<<<', '🔥🔥🔥',
    '[object Event]', '[object Object] [object Object]', 'Error code: 31005', 'Error: 20101',
    'ERRNO=5', 'status #503']) {
    assert.equal(isContentFreeConsoleLine(line), true, `content-free: ${JSON.stringify(line)}`);
  }
  for (const line of ['Payment failed', 'Error code: 31005 token expired', 'TypeError', 'Ошибка',
    'エラー', '[object Event] in checkout', 'code: 31005 from device', 'Error code: abc']) {
    assert.equal(isContentFreeConsoleLine(line), false, `has content: ${JSON.stringify(line)}`);
  }
});
