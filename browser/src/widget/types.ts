/**
 * What the core SDK's loader hands the lazily loaded chat widget bundle
 * (SERVER_SDK_SPEC.md, "Capability 6 — Chat widget" › Loading; the routes are
 * docs/CHAT_API.md). Shared by both builds; types only, so nothing here lands
 * in either bundle.
 */

/** `GET /v1/chat/widget` when `enabled` is true. */
export interface Bootstrap {
  enabled: true;
  name?: string;
  color?: string;
  position?: string;
  /** What this user may do: `report`, `status`, `howto`, `feature_requests`, `handoff`. */
  capabilities?: string[];
  bundle: { url: string; integrity: string };
  resume?: { id: number; state: string; updated_at?: string } | null;
}

export interface WidgetConfig {
  bootstrap: Bootstrap;
  /** The DSN's public key, sent as `X-ADT-Key`. */
  key: string;
  /** The signed `dbw1.…` identity, sent as `X-ADT-Identity`. */
  token: string;
  /** `https://api.developerbench.com` or the page's override, no trailing slash. */
  api: string;
  /** The sensor's trace session, sent as `context.session`. */
  session?: string;
  /** The page's CSP nonce, for the widget's own `<style>`. */
  nonce?: string;
}

export interface WidgetHandle {
  destroy(): void;
}

/** `window.DevbenchWidget`, defined by the widget bundle. */
export interface WidgetGlobal {
  version: string;
  mount(config: WidgetConfig): WidgetHandle;
}
