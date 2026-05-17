// Structural-typing fixture: TypeScript classes that satisfy interfaces by
// shape alone, without an explicit `implements` clause.

export interface Logger {
  log(message: string): void;
}

export interface Closeable {
  close(): void;
}

// Note: no `implements Logger`, but the shape matches.
export class ConsoleLogger {
  log(message: string): void {
    console.log(message);
  }
}

// Implements both Logger and Closeable structurally.
export class FileLogger {
  log(message: string): void {
    /* write */
  }
  close(): void {
    /* fclose */
  }
}

// Only matches Logger — `close()` exists but with the wrong arity.
export class WeirdLogger {
  log(message: string): void {
    /* … */
  }
  close(_a: string, _b: string): void {
    /* incompatible arity */
  }
}
