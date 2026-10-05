/**
 * A PerformanceObserver for the entry types Node does not have.
 *
 * Node ships a real PerformanceObserver, but not `largest-contentful-paint`
 * or `event` (Event Timing) entries: those come from a browser's renderer.
 * Like `xhr.ts`, this stands in for the platform, not for the code under
 * test: it behaves as the browser's observer does as far as the sensor can
 * tell — `supportedEntryTypes`, `observe({type, buffered, durationThreshold})`,
 * entries queued and then delivered to the callback in a batch, `takeRecords()`
 * returning (and removing) whatever is queued but undelivered, `disconnect()`
 * ending delivery, and Event Timing's `durationThreshold` filter applied by the
 * "browser" before delivery.
 *
 * What it cannot prove is that a real browser produces these entries at the
 * moments, and with the values, the sensor expects. That needs a real browser.
 */
import type { PerfEntry, VitalsCaptureTargets } from '../../src/capture/vitals.js';

export interface ObserveOptions {
  type: string;
  buffered?: boolean;
  durationThreshold?: number;
}

export interface TestObserver {
  options: ObserveOptions | null;
  connected: boolean;
}

export function perfPlatform(supported: string[] = ['largest-contentful-paint', 'event', 'mark', 'measure']) {
  const observers: Array<TestObserver & { queued: PerfEntry[]; deliver(): void }> = [];

  class Observer {
    static readonly supportedEntryTypes = supported;
    options: ObserveOptions | null = null;
    connected = false;
    queued: PerfEntry[] = [];
    constructor(private readonly cb: (list: { getEntries(): PerfEntry[] }) => void) {}

    observe(options: ObserveOptions): void {
      this.options = options;
      this.connected = true;
      observers.push(this);
    }

    disconnect(): void {
      this.connected = false;
      this.queued = [];
    }

    takeRecords(): PerfEntry[] {
      const out = this.queued;
      this.queued = [];
      return out;
    }

    deliver(): void {
      if (!this.connected || this.queued.length === 0) return;
      const batch = this.takeRecords();
      this.cb({ getEntries: () => batch.slice() });
    }
  }

  /** Queues entries for observers of their type, without delivering them. */
  function queue(...entries: PerfEntry[]): void {
    for (const e of entries) {
      for (const o of observers) {
        if (!o.connected || o.options?.type !== e.entryType) continue;
        const min = o.options.durationThreshold ?? 104; // the spec's default
        if (e.entryType === 'event' && e.duration < min) continue;
        o.queued.push(e);
      }
    }
  }

  /** Delivers everything queued, as the browser's observer task does. */
  function deliver(): void {
    for (const o of observers.slice()) o.deliver();
  }

  return {
    PerformanceObserver: Observer,
    observers,
    connected: (type: string) => observers.filter((o) => o.connected && o.options?.type === type),
    queue,
    deliver,
    emit(...entries: PerfEntry[]): void {
      queue(...entries);
      deliver();
    },
  };
}

/** An LCP candidate, shaped as the browser's LargestContentfulPaint entry. */
export function lcpEntry(startTime: number): PerfEntry & Record<string, unknown> {
  return {
    entryType: 'largest-contentful-paint', name: '', startTime, duration: 0,
    renderTime: startTime, loadTime: 0, size: 48_000, id: '', url: '', element: null,
  };
}

/** An Event Timing entry, shaped as the browser's PerformanceEventTiming. */
export function eventEntry(name: string, duration: number, interactionId: number, startTime = 10_000): PerfEntry & Record<string, unknown> {
  return {
    entryType: 'event', name, startTime, duration, interactionId,
    processingStart: startTime + 4, processingEnd: startTime + duration - 8, cancelable: true, target: null,
  };
}

export type TestPage = EventTarget & Required<Pick<VitalsCaptureTargets, 'location'>> & {
  PerformanceObserver?: unknown;
  document: EventTarget & { visibilityState: DocumentVisibilityState };
};

/** A window-shaped target: events, a location, a document with visibility. */
export function testPage(platform: ReturnType<typeof perfPlatform> | null, pathname = '/', hash = ''): TestPage {
  const document = Object.assign(new EventTarget(), { visibilityState: 'visible' as DocumentVisibilityState });
  return Object.assign(new EventTarget(), {
    PerformanceObserver: platform?.PerformanceObserver,
    location: { pathname, hash },
    document,
  });
}

/** The page as the install function's target type. */
export function asTarget(page: TestPage): VitalsCaptureTargets {
  return page as unknown as VitalsCaptureTargets;
}

/** Hides the page the way a browser does: state first, then the event. */
export function hide(page: TestPage): void {
  page.document.visibilityState = 'hidden';
  page.document.dispatchEvent(new Event('visibilitychange'));
}
