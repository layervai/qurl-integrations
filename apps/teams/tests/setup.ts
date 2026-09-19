import { webcrypto } from 'node:crypto';

// jose 6 targets WebCrypto. Keep the test runner deterministic on Node
// versions/configurations where Vitest does not expose globalThis.crypto.
if (!globalThis.crypto) Object.defineProperty(globalThis, 'crypto', { value: webcrypto });
