/**
 * Fixed-capacity ring buffer.
 *
 * Preallocated and never grown: the sensor's memory ceiling is a property of
 * construction, not of the host app behaving well. When it is full, the oldest
 * entry is dropped — losing old context is always preferable to growing without
 * bound inside someone else's process (principle #2).
 */
export class RingBuffer<T> {
  private readonly items: Array<T | undefined>;
  private head = 0;
  private size = 0;
  private dropped = 0;

  constructor(readonly capacity: number) {
    if (!Number.isInteger(capacity) || capacity <= 0) {
      throw new Error(`RingBuffer: capacity must be a positive integer, got ${capacity}`);
    }
    this.items = new Array<T | undefined>(capacity);
  }

  push(item: T): void {
    if (this.size === this.capacity) {
      this.items[this.head] = item;
      this.head = (this.head + 1) % this.capacity;
      this.dropped++;
      return;
    }
    this.items[(this.head + this.size) % this.capacity] = item;
    this.size++;
  }

  /** Entries oldest-first. Allocates; call it at flush time, not per event. */
  toArray(): T[] {
    const out: T[] = new Array(this.size);
    for (let i = 0; i < this.size; i++) out[i] = this.items[(this.head + i) % this.capacity] as T;
    return out;
  }

  /** Empties the buffer and releases references so entries can be collected. */
  clear(): void {
    this.items.fill(undefined);
    this.head = 0;
    this.size = 0;
  }

  get length(): number {
    return this.size;
  }

  /** How many entries were discarded because the buffer was full. Reported on
   *  flush so silent loss is visible rather than inferred. */
  get droppedCount(): number {
    return this.dropped;
  }
}
