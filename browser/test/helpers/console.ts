/**
 * Collects console.warn calls for the duration of `fn`.
 *
 * Observes the SDK's only developer-facing output; it replaces the platform
 * console, never code under test.
 */
export async function captureWarnings(fn: () => unknown): Promise<string[]> {
  const seen: string[] = [];
  const original = console.warn;
  console.warn = (...args: unknown[]) => {
    seen.push(args.map(String).join(' '));
  };
  try {
    await fn();
  } finally {
    console.warn = original;
  }
  return seen;
}
