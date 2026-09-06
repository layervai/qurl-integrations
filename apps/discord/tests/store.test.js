describe('store entrypoint', () => {
  afterEach(() => delete process.env.STORE_TYPE);

  test('rejects a removed backend', () => {
    process.env.STORE_TYPE = 'sqlite';
    expect(() => require('../src/store')).toThrow("Unknown STORE_TYPE: 'sqlite'");
  });

  test('exports every store method called by production source', () => {
    const fs = require('fs');
    const path = require('path');
    const sourceRoot = path.resolve(__dirname, '../src');
    const files = [];
    const walk = directory => fs.readdirSync(directory, { withFileTypes: true }).forEach(entry => {
      const file = path.join(directory, entry.name);
      if (entry.isDirectory()) walk(file);
      else if (entry.name.endsWith('.js')) files.push(file);
    });
    walk(sourceRoot);

    const called = new Set();
    for (const file of files) {
      const source = fs.readFileSync(file, 'utf8');
      for (const match of source.matchAll(/const\s+(\w+)\s*=\s*require\(['"](?:\.\.?\/)*store['"]\)/g)) {
        const calls = new RegExp(`\\b${match[1]}\\.([A-Za-z_$][\\w$]*)\\s*\\(`, 'g');
        for (const call of source.matchAll(calls)) called.add(call[1]);
      }
    }

    const store = require('../src/store');
    expect(called.size).toBeGreaterThan(0);
    expect([...called].filter(method => typeof store[method] !== 'function')).toEqual([]);
  });
});
