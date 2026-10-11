/**
 * `Devbench.init`: the whole install in one call.
 *
 *   Devbench.init({ dsn: 'https://<key>@adt-ingest.onrender.com', release: BUILD_SHA });
 *
 * Builds the sensor (auto-flush on), installs error, network and vitals
 * capture on the page, and, when the page has AngularJS, hooks it too. It is
 * what the README's hand-written install did, minus every step an integrator
 * could forget.
 *
 * Never throws. A bad DSN, `enabled: false` or an internal failure returns an
 * inert sensor whose methods do nothing, so an app never has to check.
 * Calling it again returns the first sensor and installs nothing twice.
 */
import { Sensor, type SensorOptions } from './sensor.js';
import { installErrorCapture, type ErrorCaptureTargets } from './capture/errors.js';
import { attachAngular, type AngularStatic } from './adapters/angularjs.js';
import { parseDsn, warn } from './dsn.js';
import { WidgetLoader, type LoaderScope } from './widget/loader.js';

export type UserInput = { email?: string | null; account?: string | number | null } | null | undefined;

export interface InitOptions extends SensorOptions {
  /** The signed-in user, if known at init; `setUser` later for logins. */
  user?: UserInput;
  /**
   * The chat widget's signed identity (`dbw1.…`), rendered by your server
   * (Rails: automatic; Go: `devbench.WidgetIdentity`). Without one the SDK
   * makes no chat request. Role and permissions live inside the token: there
   * is deliberately no option for them.
   */
  widgetIdentity?: string | null;
  /** The chat API base. Default `https://api.developerbench.com`. */
  api?: string;
  /** The page's CSP nonce, put on the widget's script tag. */
  nonce?: string;
}

/**
 * The page `init` installs onto: `window` in a browser. A parameter only so
 * tests can hand it a window-like object; apps never pass it.
 */
export interface InitScope extends ErrorCaptureTargets, Omit<LoaderScope, 'document' | 'fetch' | 'addEventListener' | 'removeEventListener'> {
  angular?: AngularStatic;
}

interface Installed {
  sensor: Sensor;
  scope: InitScope;
  teardown: Array<() => void>;
  angulars: AngularStatic[];
  widget: WidgetLoader | null;
}

let current: Installed | null = null;
/** A token given to `setWidgetIdentity` before `init`. */
let pendingIdentity: string | null | undefined;

export function init(options: InitOptions, scope?: InitScope): Sensor {
  let sensor: Sensor | null = null;
  try {
    if (current) {
      warn('init was already called; keeping the first sensor.');
      return current.sensor;
    }

    const opts: InitOptions = options || {};
    sensor = new Sensor(opts);
    if (!sensor.enabled) return sensor;

    const s = sensor;
    const page: InitScope = scope ?? (globalThis as unknown as InitScope);
    const installed: Installed = { sensor: s, scope: page, teardown: [], angulars: [], widget: null };
    current = installed;

    if (opts.user) s.setUser(opts.user);
    installed.teardown.push(installErrorCapture(s, page));
    installed.teardown.push(() => s.stop());

    if (!registerAngular(page.angular)) whenDomReady(installed);

    const widget = new WidgetLoader(page as LoaderScope, s, publicKey(opts), opts.api, opts.nonce || '');
    installed.widget = widget;
    installed.teardown.push(() => widget.close());
    widget.set(opts.widgetIdentity !== undefined ? opts.widgetIdentity : pendingIdentity);
    return sensor;
  } catch {
    // Half-installed is worse than not installed: undo, then stand down.
    if (current && current.sensor === sensor) close();
    return new Sensor({ enabled: false });
  }
}

/**
 * Hooks AngularJS into the initialized sensor. `init` does this itself when
 * `window.angular` exists, or exists by DOMContentLoaded; call it yourself
 * only when AngularJS loads later than that, before the app bootstraps.
 * Returns whether AngularJS is hooked. Idempotent.
 */
export function registerAngular(angular?: AngularStatic): boolean {
  try {
    const c = current;
    if (!c || !angular || typeof angular.module !== 'function') return false;
    if (c.angulars.indexOf(angular) >= 0) return true;
    attachAngular(angular, c.sensor);
    c.angulars.push(angular);
    return true;
  } catch {
    return false;
  }
}

/**
 * Sets (or with `null` clears) who is using the page, on the initialized
 * sensor. Before `init`, or when disabled, does nothing.
 */
export function setUser(user: UserInput): void {
  current?.sensor.setUser(user);
}

/**
 * Sets the chat widget's signed identity: a new token (after login or a role
 * change) replaces the widget, `null` (logout) removes it. Before `init` it is
 * kept for `init` to use. Takes only the token: role and permissions are
 * signed inside it.
 */
export function setWidgetIdentity(token: string | null): void {
  pendingIdentity = token;
  current?.widget?.set(token);
}

/** The DSN's public key (or a pre-DSN `ingestKey`): the chat API's `X-ADT-Key`. */
function publicKey(opts: InitOptions): string {
  if (opts.dsn !== undefined) {
    const d = parseDsn(opts.dsn);
    return typeof d === 'string' ? '' : d.key;
  }
  return opts.ingestKey ?? '';
}

/** The sensor `init` installed, or null. */
export function getSensor(): Sensor | null {
  return current ? current.sensor : null;
}

/**
 * Removes every hook `init` installed and stops the flush loop, so `init`
 * can run again. AngularJS hooks cannot be removed once registered: they
 * stay bound to the closed sensor, which no longer sends.
 */
export function close(): void {
  const c = current;
  current = null;
  pendingIdentity = undefined;
  if (!c) return;
  for (const fn of c.teardown) {
    try {
      fn();
    } catch {
      // Already gone is as good as removed.
    }
  }
}

/**
 * AngularJS loaded after this script: look again at DOMContentLoaded. A
 * listener added now runs before the one angular.js adds when it loads (later),
 * and that one is what bootstraps `ng-app`, so the hook is still in time.
 */
function whenDomReady(installed: Installed): void {
  const doc = installed.scope.document as (Document | undefined);
  if (!doc || doc.readyState !== 'loading' || typeof doc.addEventListener !== 'function') return;
  const onReady = (): void => {
    doc.removeEventListener('DOMContentLoaded', onReady);
    if (current === installed) registerAngular(installed.scope.angular);
  };
  doc.addEventListener('DOMContentLoaded', onReady);
  installed.teardown.push(() => doc.removeEventListener('DOMContentLoaded', onReady));
}
