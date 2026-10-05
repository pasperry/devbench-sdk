import { test } from 'node:test';
import assert from 'node:assert/strict';
import { formatTrace, parseTrace, newId, TRACE_HEADER, readSessionToken, readHandled } from '../src/trace.js';
import { Sensor } from '../src/sensor.js';
import { registerAngularModule, type AngularStatic } from '../src/adapters/angularjs.js';
import { resetForTests } from '../src/safe.js';

function sensor(): Sensor {
  resetForTests();
  return new Sensor({ endpoint: 'http://127.0.0.1:9/v1/flush', tenant: 'acme', ingestKey: 'k' });
}

test('ids match the wire grammar', () => {
  for (let i = 0; i < 200; i++) {
    const id = newId();
    assert.match(id, /^[A-Za-z0-9_-]{1,64}$/, `bad id: ${id}`);
  }
});

test('ids do not collide in a session', () => {
  const seen = new Set<string>();
  for (let i = 0; i < 20_000; i++) seen.add(newId());
  assert.equal(seen.size, 20_000, 'id collision');
});

test('format and parse round-trip', () => {
  const t = { session: 'sess_A-1', intent: 'act_B-2', hop: 3 };
  assert.equal(formatTrace(t), 'v1/sess_A-1/act_B-2/3');
  assert.deepEqual(parseTrace(formatTrace(t)), t);
});

// Must agree with the Go SDK and with the sidecar's extraction regex, or a
// trace written by the browser is unreadable everywhere else.
test('rejects the same malformed values the Go SDK rejects', () => {
  for (const bad of ['', 'garbage', 'v2/a/b/0', 'v1/a/b', 'v1/a/b/c', 'v1//b/0',
                     'v1/a/b/-1', 'v1/a/b/1000', `v1/${'x'.repeat(65)}/b/0`,
                     'v1/a b/c/0', 'v1/a/b/0/extra']) {
    assert.equal(parseTrace(bad), null, `accepted malformed trace: ${JSON.stringify(bad)}`);
  }
});

test('no trace before an action has begun', () => {
  const s = sensor();
  assert.equal(s.currentTrace(), null, 'a request belonging to no user action should not be correlated');
  assert.equal(s.traceHeader(), null);
});

test('an action produces a hop-0 trace on a stable session', () => {
  const s = sensor();

  s.beginIntent('submit', 'Save');
  const first = s.currentTrace()!;
  assert.equal(first.hop, 0, 'the browser is always hop 0');
  assert.equal(first.session, s.session());

  s.beginIntent('click', 'Delete');
  const second = s.currentTrace()!;
  assert.equal(second.session, first.session, 'the session must persist across actions');
  assert.notEqual(second.intent, first.intent, 'each action needs its own intent');
});

test('traceHeader renders the wire form', () => {
  const s = sensor();
  s.beginIntent('submit', 'Save');

  const h = s.traceHeader()!;
  assert.equal(h.name, TRACE_HEADER);
  const parsed = parseTrace(h.value)!;
  assert.equal(parsed.hop, 0);
  assert.equal(parsed.session, s.session());
});

// The browser end of the four-runtime chain. Without this there is nothing for
// Rails to forward and nothing for the sidecar to index by.
test('the AngularJS interceptor stamps the header on outbound requests', () => {
  const s = sensor();
  s.beginIntent('submit', 'Save customer');

  const captured: { factory?: (q: unknown) => any } = {};
  const registered: string[] = [];

  const angular: AngularStatic = {
    module() {
      const mod: any = {
        factory(name: string, fn: unknown[]) {
          if (name === 'adtHttpInterceptor') captured.factory = fn[fn.length - 1] as (q: unknown) => any;
          return mod;
        },
        config(fn: unknown[]) {
          const configure = fn[fn.length - 1] as (p: { interceptors: string[] }) => void;
          configure({ interceptors: registered });
          return mod;
        },
        decorator() { return mod; },
        run() { return mod; },
      };
      return mod;
    },
  };

  registerAngularModule(angular, s);

  assert.ok(registered.includes('adtHttpInterceptor'), 'the interceptor was never registered');
  assert.ok(captured.factory, 'no interceptor factory');

  const interceptor = captured.factory!({ reject: (v: unknown) => v });
  const config = interceptor.request({ method: 'PUT', url: '/api/customers/1' });

  const value = config.headers[TRACE_HEADER];
  assert.ok(value, 'the interceptor did not stamp the trace header');
  assert.equal(parseTrace(value)!.hop, 0);
});

test('the interceptor never breaks a request when the sensor is disabled', () => {
  const s = sensor();
  s.beginIntent('submit', 'Save');

  const captured: { factory?: (q: unknown) => any } = {};
  const angular: AngularStatic = {
    module() {
      const mod: any = {
        factory(name: string, fn: unknown[]) {
          if (name === 'adtHttpInterceptor') captured.factory = fn[fn.length - 1] as (q: unknown) => any;
          return mod;
        },
        config() { return mod; },
        decorator() { return mod; },
        run() { return mod; },
      };
      return mod;
    },
  };
  registerAngularModule(angular, s);

  const interceptor = captured.factory!({ reject: (v: unknown) => v });

  // A config object that throws when written to, standing in for anything
  // unexpected in the host app.
  const hostile = Object.freeze({ method: 'GET', url: '/x' });
  assert.doesNotThrow(() => interceptor.request(hostile as never));
});

// The browser never mints a session token — it only carries what the customer's
// backend put in the page. That is the whole point: the secret stays server-side.
test('reads the session token the server injected', () => {
  const doc = {
    querySelector(sel: string) {
      if (sel !== 'meta[name="adt-session"]') return null;
      return { getAttribute: (a: string) => (a === 'content' ? '  adts1.abc.def  ' : null) };
    },
  } as unknown as Document;

  assert.equal(readSessionToken(doc), 'adts1.abc.def', 'the token was not read or not trimmed');
});

// A tenant that has not enabled session tokens must work exactly as before.
test('an absent session token is normal, not an error', () => {
  const doc = { querySelector: () => null } as unknown as Document;
  assert.equal(readSessionToken(doc), null);

  const empty = {
    querySelector: () => ({ getAttribute: () => '   ' }),
  } as unknown as Document;
  assert.equal(readSessionToken(empty), null, 'a blank tag should read as absent');
});

test('a hostile document cannot break the sensor', () => {
  const doc = {
    querySelector() { throw new Error('boom'); },
  } as unknown as Document;
  assert.doesNotThrow(() => readSessionToken(doc));
  assert.equal(readSessionToken(doc), null);
});

// Every way of reading this header fails in an ordinary way, so none of them
// may throw: the server SDK may not be installed, the header may not be
// exposed cross-origin, and HTTP clients disagree about what a headers object
// is.
test('readHandled copes with every shape a client hands it', () => {
  // fetch-style Headers
  assert.equal(readHandled(new Headers({ 'x-adt-handled': '3' })), 3);
  assert.equal(readHandled(new Headers({})), 0);

  // AngularJS $http hands back a plain object, and header names are
  // case-insensitive.
  assert.equal(readHandled({ 'x-adt-handled': '2' }), 2);
  assert.equal(readHandled({ 'X-ADT-Handled': '2' }), 2);

  // Absent, malformed, or mangled by a proxy. Inventing a silent failure from
  // any of these would spend trust on noise.
  assert.equal(readHandled(null), 0);
  assert.equal(readHandled(undefined), 0);
  assert.equal(readHandled({}), 0);
  assert.equal(readHandled({ 'x-adt-handled': '' }), 0);
  assert.equal(readHandled({ 'x-adt-handled': 'yes' }), 0);
  assert.equal(readHandled({ 'x-adt-handled': '-1' }), 0);
  assert.equal(readHandled({ 'x-adt-handled': '1.7' }), 1);

  // A headers object whose get() throws.
  assert.equal(readHandled({ get() { throw new Error('nope'); } }), 0);
});

// Wired in the interceptor, not left to the application team.
//
// Detector 2 on the client is one header read. Asking an application to
// thread it through every call site would mean it never happens, and the
// interceptor is already a chokepoint for every response.
test('the AngularJS adapter reports a swallowed failure without app changes', () => {
  resetForTests();

  const sensor = new Sensor({
    endpoint: 'http://127.0.0.1:9/v1/flush', tenant: 'acme',
    ingestKey: 'adt_client_test', release: 'r1', sensorId: 's1',
  });
  sensor.beginIntent('submit', 'Save profile');

  const captured: { factory?: (q: unknown) => any } = {};
  const registered: string[] = [];

  const angular: AngularStatic = {
    module() {
      const mod: any = {
        factory(name: string, fn: unknown[]) {
          if (name === 'adtHttpInterceptor') captured.factory = fn[fn.length - 1] as (q: unknown) => any;
          return mod;
        },
        config(fn: unknown[]) {
          const configure = fn[fn.length - 1] as (p: { interceptors: string[] }) => void;
          configure({ interceptors: registered });
          return mod;
        },
        decorator() { return mod; },
        run() { return mod; },
      };
      return mod;
    },
  };

  registerAngularModule(angular, sensor);
  const interceptor = captured.factory!({ reject: (v: unknown) => v });

  // $http's accessor form: called with a name, returns that one header.
  interceptor.response({
    config: { method: 'PUT', url: '/api/customers/9912' },
    status: 200,
    headers: (name?: string) => (name === 'x-adt-handled' ? '1' : null),
  });

  const bundle = sensor.evidenceBundle('x') as { signals: Array<{ kind?: string }> };
  assert.ok(
    bundle.signals.some((s) => s.kind === 'silent_failure'),
    `expected a silent_failure signal, got ${JSON.stringify(bundle.signals.map((s) => s.kind))}`,
  );
});
