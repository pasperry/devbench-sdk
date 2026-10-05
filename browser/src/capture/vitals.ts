/**
 * Slowness users feel, as counted problems: slow page loads (LCP) and slow
 * interactions (Event Timing, the measurement behind INP).
 *
 * Not tracing. Nothing here records a span, a timing series or a number per
 * page view; a slow load or a slow interaction becomes one `degradation`
 * signal whose message names only the route, so it fingerprints and counts
 * exactly like an error. Traffic scales with distinct problems, not with
 * page views (see README, "Why no tracing").
 *
 * Thresholds are web.dev's "poor" bands, adjustable per sensor:
 *   - LCP >= `slow.pageLoad` (default 4000ms): `slow_page_load`, once per page
 *     load. LCP keeps emitting candidates while the page renders; the value is
 *     final at the first keydown/click or when the page is hidden, so it is
 *     judged then (taking any queued records first), and never again. A page
 *     loaded in a background tab is not judged at all: its LCP measures how
 *     long the tab sat unseen, not how long a user waited.
 *   - An interaction whose event duration (input to next paint) is
 *     >= `slow.interaction` (default 500ms): `slow_interaction`, once per
 *     interaction — a click produces pointerdown, pointerup and click entries
 *     sharing one interactionId, and that is one slow interaction, not three.
 *
 * The interaction message deliberately leaves out the current intent's
 * action. The sensor's current intent is the last one begun, not one known to
 * be in progress, so naming it would attribute a slow click to whatever the
 * user did before — splitting one problem across unrelated names. The intent
 * id still travels on the signal into evidence, where a human can judge it.
 *
 * Everything is feature-detected: no PerformanceObserver, no
 * `supportedEntryTypes`, or an entry type the engine lacks means nothing is
 * installed and nothing is recorded.
 */
import { safeVoid } from './../safe.js';
import { redactUrl } from './../redact.js';
import type { Sensor } from './../sensor.js';

/** The fields of a PerformanceEntry we read (LCP and Event Timing). */
export interface PerfEntry {
  entryType: string;
  startTime: number;
  duration: number;
  /** Event Timing only: non-zero for entries that belong to a user interaction. */
  interactionId?: number;
}

interface PerfObserver {
  observe(opts: { type: string; buffered?: boolean; durationThreshold?: number }): void;
  disconnect(): void;
  takeRecords?(): PerfEntry[];
}

export interface PerfObserverCtor {
  new (cb: (list: { getEntries(): PerfEntry[] }) => void): PerfObserver;
  readonly supportedEntryTypes?: readonly string[];
}

export interface VitalsCaptureTargets {
  PerformanceObserver?: PerfObserverCtor;
  location?: { pathname: string; hash: string };
  document?: Pick<Document, 'addEventListener' | 'removeEventListener' | 'visibilityState'>;
  addEventListener?: Window['addEventListener'];
  removeEventListener?: Window['removeEventListener'];
}

/** Interaction ids remembered for de-duplication. Entries of one interaction
 *  arrive together, so a handful is plenty; fixed so memory is too. */
const SEEN_INTERACTIONS = 8;

/**
 * The page's route as a template: the path, plus a hash route (`#!/…` or
 * `#/…`, as AngularJS uses) when there is one, each with ids reduced the way
 * request URLs are. `/#!/deals/123/edit` and `/#!/deals/456/edit` are one
 * route. A plain anchor (`#section`) is not a route and is dropped.
 */
export function routeTemplate(loc: { pathname?: string; hash?: string } | undefined): string {
  let route = redactUrl(loc?.pathname || '/');
  const hash = loc?.hash || '';
  const m = /^#(!?)(\/.*)$/.exec(hash);
  if (m) route += `#${m[1]}${redactUrl(m[2] ?? '')}`;
  return route;
}

/** Installs the observers on `target`. Returns their teardown. */
export function installVitalsCapture(sensor: Sensor, target: VitalsCaptureTargets): () => void {
  const teardown: Array<() => void> = [];

  safeVoid(() => {
    const PO = target.PerformanceObserver;
    const supported = PO && PO.supportedEntryTypes;
    if (typeof PO !== 'function' || !supported) return;

    const observe = (type: string, opts: { durationThreshold?: number }, cb: (entries: PerfEntry[]) => void): PerfObserver | null => {
      if (supported.indexOf(type) < 0) return null;
      // Feature detection, not our bug: an engine can list a type and still
      // reject an option. A throw here must mean "unsupported", not disable
      // the whole sensor the way a throw inside `safe` would.
      try {
        const obs = new PO((list) => safeVoid(() => cb(list.getEntries())));
        obs.observe({ type, buffered: true, ...opts });
        teardown.push(() => obs.disconnect());
        return obs;
      } catch {
        return null;
      }
    };

    const route = (): string => routeTemplate(target.location);
    const doc = target.document;

    // ---- slow page load (LCP)
    const loadLimit = sensor.slow.pageLoad;
    if (loadLimit > 0 && !(doc && doc.visibilityState === 'hidden')) {
      let lcp = -1;
      let lcpRoute = '';
      let done = false;
      const take = (entries: PerfEntry[]): void => {
        const last = entries[entries.length - 1];
        if (last) {
          lcp = last.startTime;
          lcpRoute = route();
        }
      };
      const obs = observe('largest-contentful-paint', {}, (entries) => {
        if (!done) take(entries);
      });

      if (obs) {
        const finalize = (): void => safeVoid(() => {
          if (done) return;
          done = true;
          if (obs.takeRecords) take(obs.takeRecords());
          obs.disconnect();
          stopListening();
          if (lcp >= loadLimit) sensor.record('degradation', 'slow_page_load', `page ${lcpRoute} loaded slowly`);
        });
        const onVisibility = (): void => {
          if (doc && doc.visibilityState === 'hidden') finalize();
        };
        const opts = { capture: true, once: true };
        const stops: Array<() => void> = [];
        const stopListening = (): void => {
          for (const fn of stops.splice(0)) safeVoid(fn);
        };
        if (target.addEventListener) {
          for (const type of ['keydown', 'click']) {
            target.addEventListener(type, finalize, opts);
            stops.push(() => target.removeEventListener?.(type, finalize, opts));
          }
        }
        if (doc && typeof doc.addEventListener === 'function') {
          doc.addEventListener('visibilitychange', onVisibility, true);
          stops.push(() => doc.removeEventListener('visibilitychange', onVisibility, true));
        }
        // Also ahead of the sensor's own hide-time flush, so a page left
        // without any input still sends its slow load before it goes.
        stops.push(sensor.onHide(finalize));
        teardown.push(() => {
          done = true;
          stopListening();
        });
      }
    }

    // ---- slow interactions (Event Timing)
    const interactionLimit = sensor.slow.interaction;
    if (interactionLimit > 0) {
      const seen: number[] = [];
      observe('event', { durationThreshold: Math.max(16, interactionLimit) }, (entries) => {
        for (const e of entries) {
          const id = e.interactionId;
          if (!id || e.duration < interactionLimit || seen.indexOf(id) >= 0) continue;
          seen.push(id);
          if (seen.length > SEEN_INTERACTIONS) seen.shift();
          sensor.record('degradation', 'slow_interaction', `interaction on ${route()} was slow`);
        }
      });
    }
  });

  return () => {
    for (const fn of teardown) safeVoid(fn);
  };
}
