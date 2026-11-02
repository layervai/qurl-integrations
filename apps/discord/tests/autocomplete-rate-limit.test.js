const { takeAutocompleteToken, autocompleteBuckets } = require('../src/utils/autocomplete-rate-limit');

beforeEach(() => autocompleteBuckets.clear());
afterEach(() => jest.restoreAllMocks());

test('allows ten immediate calls and refills one token per second without resetting denied attempts', () => {
  let now = 100_000;
  jest.spyOn(Date, 'now').mockImplementation(() => now);
  for (let i = 0; i < 10; i++) expect(takeAutocompleteToken('user')).toBe(true);
  expect(takeAutocompleteToken('user')).toBe(false);
  now += 500;
  expect(takeAutocompleteToken('user')).toBe(false);
  now += 500;
  expect(takeAutocompleteToken('user')).toBe(true);
  expect(takeAutocompleteToken('user')).toBe(false);
  now += 100_000;
  for (let i = 0; i < 10; i++) expect(takeAutocompleteToken('user')).toBe(true);
  expect(takeAutocompleteToken('user')).toBe(false);
});

test('ten 100ms refills restore exactly one token without rounding drift', () => {
  let now = 100_000;
  jest.spyOn(Date, 'now').mockImplementation(() => now);
  for (let i = 0; i < 10; i++) takeAutocompleteToken('user');
  for (let i = 0; i < 10; i++) {
    now += 100;
    expect(takeAutocompleteToken('user')).toBe(i === 9);
  }
});

test('preserves active budgets at capacity and replaces only an idle oldest entry', () => {
  let now = 100_000;
  jest.spyOn(Date, 'now').mockImplementation(() => now);
  for (let i = 0; i < 10_000; i++) autocompleteBuckets.set(`user-${i}`, { creditMs: 0, updatedAt: now });
  expect(takeAutocompleteToken('new')).toBe(false);
  expect(autocompleteBuckets.size).toBe(10_000);
  expect(takeAutocompleteToken('user-0')).toBe(false);
  now += 10_000;
  expect(takeAutocompleteToken('new')).toBe(true);
  expect(autocompleteBuckets.size).toBe(10_000);
  expect(autocompleteBuckets.has('user-1')).toBe(false);
  expect(autocompleteBuckets.has('user-0')).toBe(true);
});

test('backward clock movement cannot grant tokens', () => {
  const clock = jest.spyOn(Date, 'now').mockReturnValue(100_000);
  for (let i = 0; i < 10; i++) takeAutocompleteToken('user');
  clock.mockReturnValue(99_000);
  expect(takeAutocompleteToken('user')).toBe(false);
  clock.mockReturnValue(100_000);
  expect(takeAutocompleteToken('user')).toBe(false);
});

test.each([undefined, null, '', 42, 'x'.repeat(65)])('invalid identity %s does not allocate state', value => {
  expect(takeAutocompleteToken(value)).toBe(false);
  expect(autocompleteBuckets.size).toBe(0);
});
