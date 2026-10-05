/**
 * AngularJS (1.x) adapter.
 *
 * AngularJS is better for this than a modern SPA, not worse (proposal 4.1):
 * `$http` interceptors are a single chokepoint for every request and response,
 * so write-then-readback detection needs no per-call-site glue, and
 * `$exceptionHandler` catches everything the framework swallows.
 *
 * Registered as a module the app depends on:
 *
 *   angular.module('app', ['adt']);
 */
import type { Sensor } from './../sensor.js';
import { HANDLED_HEADER, readHandled } from './../trace.js';

/** The slice of AngularJS we use. Typed structurally so the SDK never depends
 *  on an `angular` package that an app of this vintage may not have. */
export interface AngularModule {
  factory(name: string, fn: unknown[]): AngularModule;
  config(fn: unknown[]): AngularModule;
  decorator?(name: string, fn: unknown[]): AngularModule;
  run(fn: unknown[]): AngularModule;
}

export interface AngularStatic {
  module(name: string, deps?: string[]): AngularModule;
}

interface HttpConfig {
  method?: string;
  url?: string;
  data?: unknown;
  headers?: Record<string, string>;
}

interface HttpResponse {
  config?: HttpConfig;
  status?: number;
  data?: unknown;
  /**
   * AngularJS hands back a function that returns either one header or the
   * whole set. Typed loosely because it is both, depending on arity, and
   * because some test doubles supply a plain object instead.
   */
  headers?: ((name?: string) => unknown) | Record<string, unknown>;
}

const INTERCEPTOR = 'adtHttpInterceptor';
/** Marks our $exceptionHandler wrapper, so it is applied once per injector. */
const WRAPPED = '__devbench';

/**
 * Registers the `adt` AngularJS module against `sensor`.
 *
 * Every hook re-throws or re-rejects: the adapter observes, it never changes
 * the app's behavior. An interceptor that swallowed a rejection would create
 * exactly the silent failure we exist to detect.
 */
export function registerAngularModule(angular: AngularStatic, sensor: Sensor): void {
  const mod = angular.module('adt', []);

  mod.factory(INTERCEPTOR, interceptorFactory(sensor));

  mod.config([
    '$httpProvider',
    function ($httpProvider: { interceptors: string[] }) {
      addInterceptor($httpProvider);
    },
  ]);

  // $exceptionHandler is decorated, not replaced: the app's own handler
  // still runs.
  if (mod.decorator) mod.decorator('$exceptionHandler', exceptionDecorator(sensor));
}

/** The slice of AngularJS's `$provide` used by `attachAngular`. */
export interface AngularProvide {
  factory(name: string, fn: unknown[]): unknown;
  decorator(name: string, fn: unknown[]): unknown;
}

/**
 * What `Devbench.init` does on a page with AngularJS: registers the `adt`
 * module (so apps that already list it keep working) and also hooks the core
 * `ng` module, which every injector loads, so the app needs no dependency
 * change at all.
 *
 * Config blocks added to `ng` run after ng's own provider registrations, so
 * `$provide` and `$httpProvider` are ready. An app that also lists `adt` gets
 * the interceptor and the exception decorator once, not twice: both are
 * deduplicated per injector. Like any module, this must run before the app
 * bootstraps.
 */
export function attachAngular(angular: AngularStatic, sensor: Sensor): void {
  registerAngularModule(angular, sensor);
  angular.module('ng').config([
    '$provide',
    '$httpProvider',
    function ($provide: AngularProvide, $httpProvider: { interceptors: string[] }) {
      $provide.factory(INTERCEPTOR, interceptorFactory(sensor));
      addInterceptor($httpProvider);
      $provide.decorator('$exceptionHandler', exceptionDecorator(sensor));
    },
  ]);
}

function addInterceptor($httpProvider: { interceptors: string[] }): void {
  if ($httpProvider.interceptors.indexOf(INTERCEPTOR) < 0) $httpProvider.interceptors.push(INTERCEPTOR);
}

function interceptorFactory(sensor: Sensor): unknown[] {
  return [
    '$q',
    function ($q: { reject(v: unknown): unknown }) {
      return {
        // Stamp the correlation id on the way out. This is the browser end of
        // the four-runtime chain: without it there is nothing for Rails to
        // forward and nothing for the sidecar to index by.
        request(cfg: HttpConfig): HttpConfig {
          try {
            const header = sensor.traceHeader();
            if (header) {
              cfg.headers = cfg.headers ?? {};
              cfg.headers[header.name] = header.value;
            }
          } catch {
            // Never break a request over instrumentation.
          }
          return cfg;
        },
        response(res: HttpResponse): HttpResponse {
          record(sensor, res, res.status ?? 0);
          return res;
        },
        responseError(res: HttpResponse): unknown {
          record(sensor, res, res.status ?? 0);
          return $q.reject(res); // never swallow
        },
      };
    },
  ];
}

type ExceptionHandler = ((ex: Error, cause?: string) => void) & { [WRAPPED]?: true };

function exceptionDecorator(sensor: Sensor): unknown[] {
  return [
    '$delegate',
    function ($delegate: ExceptionHandler): ExceptionHandler {
      if ($delegate && $delegate[WRAPPED]) return $delegate;
      const handler: ExceptionHandler = function (ex: Error, cause?: string): void {
        try {
          sensor.record('error', ex?.name ?? 'Error', ex?.message ?? String(ex));
        } catch {
          // never let instrumentation break the handler chain
        }
        // The default handler logs through console.error, which global
        // capture would count again (with frames, so as a second fingerprint).
        sensor.muteConsole(() => $delegate(ex, cause));
      };
      handler[WRAPPED] = true;
      return handler;
    },
  ];
}

function record(sensor: Sensor, res: HttpResponse, status: number): void {
  const cfg = res.config ?? {};
  const method = (cfg.method ?? 'GET').toUpperCase();
  const url = cfg.url ?? '';
  if (url === '') return;

  const isWrite = method !== 'GET' && method !== 'HEAD';
  const writtenValues = isWrite ? flatten(cfg.data) : undefined;
  const body = !isWrite ? flatten(res.data) : undefined;

  sensor.recordRequest(method, url, status, {
    writtenValues, body, handled: handledFrom(res),
    // $http runs on XMLHttpRequest. When global capture is installed it has
    // already seen this exchange and records its outcome; recording it here
    // too would count one failure twice. Readback stays here: only $http
    // hands us the parsed bodies. JSONP is not XHR, so it keeps its outcome.
    outcomeRecordedElsewhere: sensor.capturesNetwork() && method !== 'JSONP',
  });
}

/**
 * Reads the handled count from an AngularJS response.
 *
 * Wired here rather than left to the customer, because this is the whole of
 * detector 2 on the client and asking an application team to thread a header
 * through every call site would mean it never gets done. The interceptor is
 * already a chokepoint for every response.
 */
function handledFrom(res: HttpResponse): number {
  try {
    const h = res.headers;
    if (typeof h === 'function') {
      // $http's accessor: called with a name it returns that header, called
      // bare it returns the whole set. Ask for the one we want.
      return readHandled({ [HANDLED_HEADER]: h(HANDLED_HEADER) });
    }
    return readHandled(h);
  } catch {
    return 0;
  }
}

/** Flattens a one-level object of scalars. Nested structures are ignored
 *  rather than walked: readback comparison is per-field, and walking arbitrary
 *  payloads is how a sensor ends up holding a customer's whole database row. */
function flatten(data: unknown): Record<string, string> | undefined {
  if (!data || typeof data !== 'object' || Array.isArray(data)) return undefined;

  const out: Record<string, string> = {};
  for (const [k, v] of Object.entries(data as Record<string, unknown>)) {
    if (v === null || v === undefined) continue;
    const t = typeof v;
    if (t === 'string' || t === 'number' || t === 'boolean') out[k] = String(v);
  }
  return Object.keys(out).length ? out : undefined;
}
