/**
 * Direct unit tests for src/places.js.
 *
 * Every other test file in the suite mocks ../src/places — so the real
 * functions in this module have NO coverage from those callers. This
 * file pins the contract directly:
 *
 *   - searchPlaces:           autocomplete dropdown source
 *   - findPlaceFromText:      free-text → top-match resolver (used by
 *                             resolveLocation when the sender ignores
 *                             the autocomplete dropdown)
 *   - getPlaceDetails:        place_id → canonical name/address (used
 *                             when the sender PICKS from the dropdown)
 *   - buildPlaceUrl:          construct the canonical place_id-pinned
 *                             Maps URL recipients open
 *   - PLACE_ID_SENTINEL_PREFIX: wire literal stability
 *
 * Network is mocked at the global.fetch boundary — these are unit
 * tests, not contract tests against the live Google Places API.
 */

jest.mock('../src/config', () => ({
  GOOGLE_MAPS_API_KEY: 'test-google-key',
}));
jest.mock('../src/logger', () => ({
  info: jest.fn(), warn: jest.fn(), error: jest.fn(), debug: jest.fn(), audit: jest.fn(),
}));

const config = require('../src/config');
const places = require('../src/places');
const {
  searchPlaces,
  findPlaceFromText,
  getPlaceDetails,
  buildPlaceUrl,
  PLACE_ID_SENTINEL_PREFIX,
  encodePlaceIdSentinel,
  decodePlaceIdSentinel,
} = places;

const originalFetch = global.fetch;
let fetchMock;

beforeEach(() => {
  fetchMock = jest.fn();
  global.fetch = fetchMock;
  // Re-seed the API key default (the no-key test deletes it).
  config.GOOGLE_MAPS_API_KEY = 'test-google-key';
  // Clear the autocomplete cache so a hit from a prior test can't
  // satisfy a fetchMock expectation in this one.
  places._resetAutocompleteCache();
});

afterAll(() => {
  global.fetch = originalFetch;
});

function jsonResponse(body, { status = 200, ok = true } = {}) {
  return {
    ok,
    status,
    json: async () => body,
  };
}

describe('PLACE_ID_SENTINEL_PREFIX', () => {
  test('is the wire literal "qurl_place:" (DO NOT change without a coordinated deploy)', () => {
    // The vulnerable window is autocomplete-pick → slash-submit
    // (seconds): a previously-rendered dropdown choice would resolve
    // to NOT_FOUND if the new deploy renamed the literal. DDB rows
    // always carry the resolved place_id-pinned URL, not the sentinel,
    // so they're unaffected.
    expect(PLACE_ID_SENTINEL_PREFIX).toBe('qurl_place:');
  });
});

describe('encodePlaceIdSentinel / decodePlaceIdSentinel', () => {
  test('encode then decode round-trips the placeId', () => {
    const realisticId = 'ChIJ37FjGE63t4kRD2_jXSF1F9o';
    const encoded = encodePlaceIdSentinel(realisticId);
    expect(encoded).toBe(`qurl_place:${realisticId}`);
    expect(decodePlaceIdSentinel(encoded)).toBe(realisticId);
  });

  test('decode returns null for non-sentinel strings', () => {
    expect(decodePlaceIdSentinel('Eiffel Tower')).toBeNull();
    expect(decodePlaceIdSentinel('https://goo.gl/maps/xyz')).toBeNull();
    expect(decodePlaceIdSentinel('')).toBeNull();
  });

  test('decode is type-safe (returns null for non-strings)', () => {
    // parseLocationInput may pass in unexpected values during forged
    // interactions; the null guard keeps the function total.
    expect(decodePlaceIdSentinel(null)).toBeNull();
    expect(decodePlaceIdSentinel(undefined)).toBeNull();
    expect(decodePlaceIdSentinel(42)).toBeNull();
  });

  test('decode rejects an empty payload ("qurl_place:" with no id)', () => {
    // Defensive: an empty-payload sentinel decodes to '' (falsy), which
    // would let parseLocationInput's `if (decodedPlaceId)` silently fall
    // through to the URL/text branches. Reject explicitly so the
    // failure surfaces as "no match" instead.
    expect(decodePlaceIdSentinel('qurl_place:')).toBeNull();
  });

  test('decode rejects a payload that does not match the place_id shape', () => {
    // Defends against a user typing `qurl_place:something` as free
    // text — the sentinel branch would otherwise misroute them to
    // the "place no longer available" message (which presumes they
    // picked from the dropdown). Place IDs are documented as ASCII
    // alphanumeric + `_-`, typically 27+ chars; the >=16 floor
    // excludes typos while still accepting real Google IDs.
    expect(decodePlaceIdSentinel('qurl_place:foo')).toBeNull();
    expect(decodePlaceIdSentinel('qurl_place:has space123456')).toBeNull(); // space not allowed
    expect(decodePlaceIdSentinel('qurl_place:has!bang12345678')).toBeNull(); // `!` not allowed
    expect(decodePlaceIdSentinel('qurl_place:abcdefghijklmno')).toBeNull(); // 15 chars, one short
    // 16 chars + char class — passes.
    expect(decodePlaceIdSentinel('qurl_place:abcdefghijklmnop')).toBe('abcdefghijklmnop');
    // Realistic place_id — passes.
    expect(decodePlaceIdSentinel('qurl_place:ChIJ37FjGE63t4kRD2_jXSF1F9o')).toBe('ChIJ37FjGE63t4kRD2_jXSF1F9o');
  });
});

describe('buildPlaceUrl', () => {
  test('emits the documented ?api=1&query=…&query_place_id=… form', () => {
    const url = buildPlaceUrl('The White House', 'ChIJ37FjGE63t4kRD2_jXSF1F9o');
    const parsed = new URL(url);
    expect(parsed.host).toBe('www.google.com');
    expect(parsed.pathname).toBe('/maps/search/');
    expect(parsed.searchParams.get('api')).toBe('1');
    expect(parsed.searchParams.get('query')).toBe('The White House');
    expect(parsed.searchParams.get('query_place_id')).toBe('ChIJ37FjGE63t4kRD2_jXSF1F9o');
  });

  test('URL-encodes special characters in the place name (no smuggled params)', () => {
    // Defense-in-depth: even if a place name from Places contains `&`
    // or `=`, the URL constructor's searchParams.set handles encoding
    // so the embedded value can't break out of the query param.
    const url = buildPlaceUrl('Tom & Jerry\'s = Café', 'ChIJabc');
    const parsed = new URL(url);
    expect(parsed.searchParams.get('query')).toBe('Tom & Jerry\'s = Café');
    expect(parsed.searchParams.get('query_place_id')).toBe('ChIJabc');
  });

  test('falls back to placeId as query when name is empty (still a valid URL)', () => {
    // Per Google's URL spec, `query` is required even when query_place_id
    // is set. An empty/null name shouldn't produce ?query=&query_place_id=…
    // — Maps treats that inconsistently. Using the place_id keeps the URL
    // well-formed.
    const url = buildPlaceUrl('', 'ChIJabc');
    const parsed = new URL(url);
    expect(parsed.searchParams.get('query')).toBe('ChIJabc');
    expect(parsed.searchParams.get('query_place_id')).toBe('ChIJabc');
  });
});

describe('searchPlaces', () => {
  test('returns [] (no fetch) when GOOGLE_MAPS_API_KEY is unset', async () => {
    delete config.GOOGLE_MAPS_API_KEY;
    const r = await searchPlaces('eiffel');
    expect(r).toEqual([]);
    expect(fetchMock).not.toHaveBeenCalled();
  });

  test('maps predictions to {placeId, name, address} from structured_formatting', async () => {
    fetchMock.mockResolvedValueOnce(jsonResponse({
      status: 'OK',
      predictions: [
        {
          place_id: 'ChIJ1',
          description: 'Eiffel Tower, Paris, France',
          structured_formatting: { main_text: 'Eiffel Tower', secondary_text: 'Paris, France' },
        },
        // Fallback path: when structured_formatting is missing, use `description`
        { place_id: 'ChIJ2', description: 'Tower Bridge, London' },
      ],
    }));
    const r = await searchPlaces('tower');
    expect(r).toEqual([
      { placeId: 'ChIJ1', name: 'Eiffel Tower', address: 'Paris, France' },
      { placeId: 'ChIJ2', name: 'Tower Bridge, London', address: '' },
    ]);
  });

  test('treats ZERO_RESULTS as an empty list (no throw)', async () => {
    fetchMock.mockResolvedValueOnce(jsonResponse({ status: 'ZERO_RESULTS' }));
    const r = await searchPlaces('asdfasdf');
    expect(r).toEqual([]);
  });

  test('throws on a non-OK Places status (OVER_QUERY_LIMIT)', async () => {
    fetchMock.mockResolvedValueOnce(jsonResponse({ status: 'OVER_QUERY_LIMIT' }));
    await expect(searchPlaces('eiffel')).rejects.toThrow(/OVER_QUERY_LIMIT/);
  });

  test('throws with status code on a non-2xx HTTP response', async () => {
    fetchMock.mockResolvedValueOnce(jsonResponse(null, { ok: false, status: 503 }));
    await expect(searchPlaces('eiffel')).rejects.toThrow(/503/);
  });

  test('throws a typed error when the response is not JSON', async () => {
    // Places occasionally serves an HTML error page during outages;
    // raw `.json()` throws SyntaxError with no context.
    fetchMock.mockResolvedValueOnce({
      ok: true,
      status: 200,
      json: async () => { throw new Error('Unexpected token < in JSON at position 0'); },
    });
    await expect(searchPlaces('eiffel')).rejects.toThrow(/non-JSON/);
  });

  test('sends GOOGLE_MAPS_API_KEY as a query param (Places has no header auth)', async () => {
    fetchMock.mockResolvedValueOnce(jsonResponse({ status: 'OK', predictions: [] }));
    await searchPlaces('eiffel');
    const calledUrl = fetchMock.mock.calls[0][0];
    expect(calledUrl).toContain('key=test-google-key');
    expect(calledUrl).toContain('input=eiffel');
  });

  test('caches results per normalized query — repeat lookups skip the API', async () => {
    // Autocomplete fires per keystroke; a TTL cache collapses the
    // repeat lookups a single typing session produces ("white", "white
    // ", "white h", …) so we don't pay per-keystroke against Places.
    fetchMock.mockResolvedValueOnce(jsonResponse({
      status: 'OK',
      predictions: [{ place_id: 'ChIJ1', description: 'White House' }],
    }));
    const a = await searchPlaces('white');
    const b = await searchPlaces('white');
    expect(a).toEqual(b);
    expect(fetchMock).toHaveBeenCalledTimes(1);
  });

  test('cache key is case-insensitive + whitespace-normalized', async () => {
    // Case-only variants of the same query (and surrounding/extra
    // whitespace) hit the same cache entry because Places Autocomplete
    // itself is case-insensitive — without the normalization the
    // autocomplete cache misses for what's effectively one user query.
    // (Note: "White House" with a space and "Whitehouse" compound are
    // genuinely different queries to Places and stay distinct.)
    fetchMock.mockResolvedValueOnce(jsonResponse({
      status: 'OK',
      predictions: [{ place_id: 'ChIJ1', description: 'White House' }],
    }));
    await searchPlaces('White House');
    await searchPlaces('white house');
    await searchPlaces('  WHITE HOUSE  ');
    await searchPlaces('white  house'); // extra interior whitespace collapses
    expect(fetchMock).toHaveBeenCalledTimes(1);
  });

  test('in-flight requests for the same key are deduped (single-flight)', async () => {
    // Fast typists can fire multiple keystrokes for the same prefix
    // before the first response settles — without dedup we'd pay
    // Places per concurrent call. The second concurrent call must
    // return the same promise as the first.
    let resolveBody;
    fetchMock.mockReturnValueOnce(new Promise((resolve) => {
      resolveBody = () => resolve(jsonResponse({
        status: 'OK',
        predictions: [{ place_id: 'ChIJ1', description: 'White House' }],
      }));
    }));
    const a = searchPlaces('white');
    const b = searchPlaces('white');
    resolveBody();
    const [ra, rb] = await Promise.all([a, b]);
    expect(ra).toEqual(rb);
    expect(fetchMock).toHaveBeenCalledTimes(1);
  });

  test('in-flight map cleans up after a rejected request (next call hits the API)', async () => {
    // Without `.finally(autocompleteInflight.delete)`, a rejected
    // in-flight promise would stick around as a permanently-rejecting
    // entry — subsequent calls for the same key would re-await the
    // same rejection and never retry. Pin the cleanup contract.
    fetchMock.mockRejectedValueOnce(new Error('transient network failure'));
    await expect(searchPlaces('eiffel')).rejects.toThrow(/transient/);
    // Second call must hit fetch again (in-flight slot was freed) and
    // can succeed independently.
    fetchMock.mockResolvedValueOnce(jsonResponse({
      status: 'OK',
      predictions: [{ place_id: 'ChIJ1', description: 'Eiffel Tower' }],
    }));
    const r = await searchPlaces('eiffel');
    expect(r).toEqual([{ placeId: 'ChIJ1', name: 'Eiffel Tower', address: '' }]);
    expect(fetchMock).toHaveBeenCalledTimes(2);
  });

  test('cache hit after TTL expiry re-fetches the API (60 s freshness window)', async () => {
    // The cache TTL is from initial insert (intentional refresh-after-
    // 60s for upstream-changed places). Pin the contract: a cache hit
    // BEFORE expiry skips the API; the same key AFTER expiry refetches.
    jest.useFakeTimers().setSystemTime(new Date('2026-01-01T00:00:00Z'));
    fetchMock
      .mockResolvedValueOnce(jsonResponse({
        status: 'OK',
        predictions: [{ place_id: 'ChIJ1', description: 'Place v1' }],
      }))
      .mockResolvedValueOnce(jsonResponse({
        status: 'OK',
        predictions: [{ place_id: 'ChIJ1', description: 'Place v2 (refreshed)' }],
      }));
    const first = await searchPlaces('eiffel');
    expect(first[0].name).toBe('Place v1');
    // Still within TTL — second call is a cache hit, no fetch.
    jest.advanceTimersByTime(59_000);
    const second = await searchPlaces('eiffel');
    expect(second[0].name).toBe('Place v1');
    expect(fetchMock).toHaveBeenCalledTimes(1);
    // Past TTL — entry expired, refetch fires.
    jest.advanceTimersByTime(2_000);
    const third = await searchPlaces('eiffel');
    expect(third[0].name).toBe('Place v2 (refreshed)');
    expect(fetchMock).toHaveBeenCalledTimes(2);
    jest.useRealTimers();
  });

  test('FIFO eviction at AUTOCOMPLETE_CACHE_MAX — oldest entry drops when cap is hit', async () => {
    // Don't pin the constant value here (avoid coupling to the
    // implementation detail), but pin the BEHAVIOR: when the cache is
    // saturated, the next miss-and-fetch evicts the oldest entry.
    // Test it by saturating, then re-querying the oldest key — that
    // re-query should miss and fetch again (proving it was evicted).
    //
    // We fetch (CAP) distinct queries to fill the cache, then one more
    // distinct query to trigger eviction of the oldest, then re-query
    // the oldest and verify it re-fetches.
    places._resetAutocompleteCache();
    // Mock 502 distinct fetches with unique payloads so each goes
    // through the API. CAP=500 + 1 overflow + 1 re-query = 502.
    const CAP = 500;
    for (let i = 0; i <= CAP + 1; i++) {
      fetchMock.mockResolvedValueOnce(jsonResponse({
        status: 'OK',
        predictions: [{ place_id: `ChIJ${i}`, description: `Place ${i}` }],
      }));
    }
    // Fill the cache.
    for (let i = 0; i < CAP; i++) {
      await searchPlaces(`q${i}`);
    }
    // One more query — triggers FIFO eviction of "q0" (oldest insert).
    await searchPlaces('overflow');
    expect(fetchMock).toHaveBeenCalledTimes(CAP + 1);
    // Re-query the oldest — if it was evicted, this hits the API again.
    await searchPlaces('q0');
    expect(fetchMock).toHaveBeenCalledTimes(CAP + 2);
    // Re-query a still-resident middle entry — should be cached, no fetch.
    await searchPlaces(`q${Math.floor(CAP / 2)}`);
    expect(fetchMock).toHaveBeenCalledTimes(CAP + 2);
  });

  test('cached results are defensive-copied (caller mutation does not poison the cache)', async () => {
    fetchMock.mockResolvedValueOnce(jsonResponse({
      status: 'OK',
      predictions: [{ place_id: 'ChIJ1', description: 'White House' }],
    }));
    const first = await searchPlaces('white');
    // A future caller could sort/splice the returned array (e.g.
    // ranking suggestions). Mutating must NOT bleed into the next hit.
    first.push({ placeId: 'POISON', name: 'X', address: '' });
    first.length = 0;
    const second = await searchPlaces('white');
    expect(second).toEqual([{ placeId: 'ChIJ1', name: 'White House', address: '' }]);
    expect(fetchMock).toHaveBeenCalledTimes(1);
  });

  test('strips ASCII control chars + caps at 500 chars before reaching the request URL', async () => {
    // Header-injection defense: a NUL or newline in the query MUST NOT
    // ride into the outgoing URL. (Modern fetch wouldn't allow it
    // anyway, but the sanitize keeps the contract tight.)
    fetchMock.mockResolvedValueOnce(jsonResponse({ status: 'OK', predictions: [] }));
    await searchPlaces('hello\x00\nworld' + 'x'.repeat(1000));
    const calledUrl = fetchMock.mock.calls[0][0];
    expect(calledUrl).not.toMatch(/%00/);
    expect(calledUrl).not.toMatch(/%0A/);
    // 500-char cap + 'helloworld' (10 chars after control strip) means
    // the input= param value is at most 500 chars.
    const input = new URL(calledUrl).searchParams.get('input');
    expect(input.length).toBeLessThanOrEqual(500);
  });
});

describe('findPlaceFromText', () => {
  test('throws when GOOGLE_MAPS_API_KEY is unset (resolveLocation caller maps this to no_api_key)', async () => {
    delete config.GOOGLE_MAPS_API_KEY;
    await expect(findPlaceFromText('eiffel')).rejects.toThrow(/GOOGLE_MAPS_API_KEY/);
    expect(fetchMock).not.toHaveBeenCalled();
  });

  test('returns the top candidate when Places matches', async () => {
    fetchMock.mockResolvedValueOnce(jsonResponse({
      status: 'OK',
      candidates: [
        { place_id: 'ChIJ1', name: 'The White House', formatted_address: '1600 Pennsylvania Ave NW' },
        { place_id: 'ChIJ2', name: 'White House Pub' },
      ],
    }));
    const r = await findPlaceFromText('the whitehouse');
    expect(r).toEqual({
      placeId: 'ChIJ1',
      name: 'The White House',
      address: '1600 Pennsylvania Ave NW',
    });
  });

  test('returns null on ZERO_RESULTS (caller maps this to not_found)', async () => {
    fetchMock.mockResolvedValueOnce(jsonResponse({ status: 'ZERO_RESULTS', candidates: [] }));
    const r = await findPlaceFromText('zzzz');
    expect(r).toBeNull();
  });

  test('returns null when status is OK but candidates is empty', async () => {
    fetchMock.mockResolvedValueOnce(jsonResponse({ status: 'OK', candidates: [] }));
    const r = await findPlaceFromText('zzzz');
    expect(r).toBeNull();
  });

  test('returns null when the top candidate has no place_id (malformed response)', async () => {
    // Defensive: a candidate without a place_id can't be pinned to a
    // URL downstream — buildPlaceUrl would emit `query_place_id=`
    // which Google renders as a broken search. Surface as not_found.
    fetchMock.mockResolvedValueOnce(jsonResponse({
      status: 'OK',
      candidates: [{ name: 'Place without an id', formatted_address: '...' }],
    }));
    const r = await findPlaceFromText('zzzz');
    expect(r).toBeNull();
  });

  test('falls back to formatted_address when name is missing', async () => {
    // Geocoded results (addresses) typically have formatted_address but
    // no `name`. Use the address as the display name in that case.
    fetchMock.mockResolvedValueOnce(jsonResponse({
      status: 'OK',
      candidates: [{ place_id: 'ChIJ1', formatted_address: '742 Evergreen Terrace, Springfield' }],
    }));
    const r = await findPlaceFromText('742 evergreen');
    expect(r.name).toBe('742 Evergreen Terrace, Springfield');
    expect(r.address).toBe('742 Evergreen Terrace, Springfield');
  });

  test('throws on a non-OK Places status (REQUEST_DENIED)', async () => {
    fetchMock.mockResolvedValueOnce(jsonResponse({ status: 'REQUEST_DENIED' }));
    await expect(findPlaceFromText('x')).rejects.toThrow(/REQUEST_DENIED/);
  });

  test('passes an AbortSignal to fetch (so a hung Places call can be cut)', async () => {
    fetchMock.mockResolvedValueOnce(jsonResponse({ status: 'OK', candidates: [{ place_id: 'ChIJ1', name: 'X' }] }));
    const r = await findPlaceFromText('x');
    expect(r.placeId).toBe('ChIJ1');
    expect(fetchMock.mock.calls[0][1]).toEqual(expect.objectContaining({ signal: expect.any(Object) }));
  });
});

describe('getPlaceDetails', () => {
  test('throws when GOOGLE_MAPS_API_KEY is unset', async () => {
    delete config.GOOGLE_MAPS_API_KEY;
    await expect(getPlaceDetails('ChIJabc')).rejects.toThrow(/GOOGLE_MAPS_API_KEY/);
  });

  test('returns the hydrated place on OK', async () => {
    fetchMock.mockResolvedValueOnce(jsonResponse({
      status: 'OK',
      result: { place_id: 'ChIJabc', name: 'The White House', formatted_address: '1600 Pennsylvania Ave NW' },
    }));
    const r = await getPlaceDetails('ChIJabc');
    expect(r).toEqual({
      placeId: 'ChIJabc',
      name: 'The White House',
      address: '1600 Pennsylvania Ave NW',
    });
  });

  test('returns null on NOT_FOUND (place_id deleted upstream)', async () => {
    fetchMock.mockResolvedValueOnce(jsonResponse({ status: 'NOT_FOUND' }));
    const r = await getPlaceDetails('ChIJ-deleted');
    expect(r).toBeNull();
  });

  test('returns null on INVALID_REQUEST AND warns (likely API-key/scope misconfig, not deleted-upstream)', async () => {
    // Sentinel place_ids are shape-validated at decode time; if Places
    // returns INVALID_REQUEST anyway, that signals misconfig or
    // upstream-shape drift. Warn so a broken deploy doesn't hide
    // behind the recipient-facing "place no longer available" message.
    const logger = require('../src/logger');
    logger.warn.mockClear();
    fetchMock.mockResolvedValueOnce(jsonResponse({ status: 'INVALID_REQUEST' }));
    const r = await getPlaceDetails('ChIJxxxxxxxxxxxxxxxx');
    expect(r).toBeNull();
    expect(logger.warn).toHaveBeenCalledWith(
      expect.stringContaining('INVALID_REQUEST'),
      expect.objectContaining({ place_id: 'ChIJxxxxxxxxxxxxxxxx' }),
    );
  });

  test('returns null on NOT_FOUND WITHOUT warning (place legitimately deleted upstream)', async () => {
    const logger = require('../src/logger');
    logger.warn.mockClear();
    fetchMock.mockResolvedValueOnce(jsonResponse({ status: 'NOT_FOUND' }));
    const r = await getPlaceDetails('ChIJxxxxxxxxxxxxxxxx');
    expect(r).toBeNull();
    // NOT_FOUND is the normal "place deleted" path — no operator
    // signal needed; the recipient-facing "no longer available"
    // message is the right surface.
    expect(logger.warn).not.toHaveBeenCalled();
  });

  test('throws on a non-OK Places status that is not a recognized null-case (OVER_QUERY_LIMIT)', async () => {
    fetchMock.mockResolvedValueOnce(jsonResponse({ status: 'OVER_QUERY_LIMIT' }));
    await expect(getPlaceDetails('ChIJabc')).rejects.toThrow(/OVER_QUERY_LIMIT/);
  });

  test('falls back to the caller-supplied placeId when the response omits place_id', async () => {
    // Defensive: Places normally echoes the place_id but if it ever
    // returns just `name`/`formatted_address` we still need a placeId
    // to construct buildPlaceUrl downstream.
    fetchMock.mockResolvedValueOnce(jsonResponse({
      status: 'OK',
      result: { name: 'X', formatted_address: 'Y' },
    }));
    const r = await getPlaceDetails('ChIJabc');
    expect(r.placeId).toBe('ChIJabc');
  });

  test('returns null when both the response and caller place_id are empty', async () => {
    // Edge case: no place_id available anywhere. buildPlaceUrl
    // downstream would emit `query_place_id=` which Google renders
    // as a broken search — surface as not_found instead.
    fetchMock.mockResolvedValueOnce(jsonResponse({
      status: 'OK',
      result: { name: 'X', formatted_address: 'Y' },
    }));
    const r = await getPlaceDetails('');
    expect(r).toBeNull();
  });
});
