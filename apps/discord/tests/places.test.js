
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
  config.GOOGLE_MAPS_API_KEY = 'test-google-key';
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
    expect(decodePlaceIdSentinel(null)).toBeNull();
    expect(decodePlaceIdSentinel(undefined)).toBeNull();
    expect(decodePlaceIdSentinel(42)).toBeNull();
  });

  test('decode rejects an empty payload ("qurl_place:" with no id)', () => {
    expect(decodePlaceIdSentinel('qurl_place:')).toBeNull();
  });

  test('decode rejects a payload that does not match the place_id shape', () => {
    expect(decodePlaceIdSentinel('qurl_place:foo')).toBeNull();
    expect(decodePlaceIdSentinel('qurl_place:has space123456')).toBeNull(); // space not allowed
    expect(decodePlaceIdSentinel('qurl_place:has!bang12345678')).toBeNull(); // `!` not allowed
    expect(decodePlaceIdSentinel('qurl_place:abcdefghijklmno')).toBeNull(); // 15 chars, one short
    expect(decodePlaceIdSentinel('qurl_place:abcdefghijklmnop')).toBe('abcdefghijklmnop');
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
    const url = buildPlaceUrl('Tom & Jerry\'s = Café', 'ChIJabc');
    const parsed = new URL(url);
    expect(parsed.searchParams.get('query')).toBe('Tom & Jerry\'s = Café');
    expect(parsed.searchParams.get('query_place_id')).toBe('ChIJabc');
  });

  test('falls back to placeId as query when name is empty (still a valid URL)', () => {
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
    fetchMock.mockRejectedValueOnce(new Error('transient network failure'));
    await expect(searchPlaces('eiffel')).rejects.toThrow(/transient/);
    fetchMock.mockResolvedValueOnce(jsonResponse({
      status: 'OK',
      predictions: [{ place_id: 'ChIJ1', description: 'Eiffel Tower' }],
    }));
    const r = await searchPlaces('eiffel');
    expect(r).toEqual([{ placeId: 'ChIJ1', name: 'Eiffel Tower', address: '' }]);
    expect(fetchMock).toHaveBeenCalledTimes(2);
  });

  test('cache hit after TTL expiry re-fetches the API (60 s freshness window)', async () => {
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
    jest.advanceTimersByTime(59_000);
    const second = await searchPlaces('eiffel');
    expect(second[0].name).toBe('Place v1');
    expect(fetchMock).toHaveBeenCalledTimes(1);
    jest.advanceTimersByTime(2_000);
    const third = await searchPlaces('eiffel');
    expect(third[0].name).toBe('Place v2 (refreshed)');
    expect(fetchMock).toHaveBeenCalledTimes(2);
    jest.useRealTimers();
  });

  test('FIFO eviction at AUTOCOMPLETE_CACHE_MAX — oldest entry drops when cap is hit', async () => {
    places._resetAutocompleteCache();
    const CAP = 500;
    for (let i = 0; i <= CAP + 1; i++) {
      fetchMock.mockResolvedValueOnce(jsonResponse({
        status: 'OK',
        predictions: [{ place_id: `ChIJ${i}`, description: `Place ${i}` }],
      }));
    }
    for (let i = 0; i < CAP; i++) {
      await searchPlaces(`q${i}`);
    }
    await searchPlaces('overflow');
    expect(fetchMock).toHaveBeenCalledTimes(CAP + 1);
    await searchPlaces('q0');
    expect(fetchMock).toHaveBeenCalledTimes(CAP + 2);
    await searchPlaces(`q${Math.floor(CAP / 2)}`);
    expect(fetchMock).toHaveBeenCalledTimes(CAP + 2);
  });

  test('cached results are defensive-copied (caller mutation does not poison the cache)', async () => {
    fetchMock.mockResolvedValueOnce(jsonResponse({
      status: 'OK',
      predictions: [{ place_id: 'ChIJ1', description: 'White House' }],
    }));
    const first = await searchPlaces('white');
    first.push({ placeId: 'POISON', name: 'X', address: '' });
    first.length = 0;
    const second = await searchPlaces('white');
    expect(second).toEqual([{ placeId: 'ChIJ1', name: 'White House', address: '' }]);
    expect(fetchMock).toHaveBeenCalledTimes(1);
  });

  test('strips ASCII control chars + caps at 500 chars before reaching the request URL', async () => {
    fetchMock.mockResolvedValueOnce(jsonResponse({ status: 'OK', predictions: [] }));
    await searchPlaces('hello\x00\nworld' + 'x'.repeat(1000));
    const calledUrl = fetchMock.mock.calls[0][0];
    expect(calledUrl).not.toMatch(/%00/);
    expect(calledUrl).not.toMatch(/%0A/);
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
    fetchMock.mockResolvedValueOnce(jsonResponse({
      status: 'OK',
      candidates: [{ name: 'Place without an id', formatted_address: '...' }],
    }));
    const r = await findPlaceFromText('zzzz');
    expect(r).toBeNull();
  });

  test('falls back to formatted_address when name is missing', async () => {
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
    expect(logger.warn).not.toHaveBeenCalled();
  });

  test('throws on a non-OK Places status that is not a recognized null-case (OVER_QUERY_LIMIT)', async () => {
    fetchMock.mockResolvedValueOnce(jsonResponse({ status: 'OVER_QUERY_LIMIT' }));
    await expect(getPlaceDetails('ChIJabc')).rejects.toThrow(/OVER_QUERY_LIMIT/);
  });

  test('falls back to the caller-supplied placeId when the response omits place_id', async () => {
    fetchMock.mockResolvedValueOnce(jsonResponse({
      status: 'OK',
      result: { name: 'X', formatted_address: 'Y' },
    }));
    const r = await getPlaceDetails('ChIJabc');
    expect(r.placeId).toBe('ChIJabc');
  });

  test('returns null when both the response and caller place_id are empty', async () => {
    fetchMock.mockResolvedValueOnce(jsonResponse({
      status: 'OK',
      result: { name: 'X', formatted_address: 'Y' },
    }));
    const r = await getPlaceDetails('');
    expect(r).toBeNull();
  });
});
