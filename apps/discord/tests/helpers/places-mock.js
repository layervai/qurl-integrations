
const PLACE_ID_SHAPE_RE = /^[A-Za-z0-9_-]{16,}$/;

const mockSearchPlaces = jest.fn().mockResolvedValue([]);
const mockFindPlaceFromText = jest.fn().mockResolvedValue(null);
const mockGetPlaceDetails = jest.fn().mockResolvedValue(null);

const mockPlacesModule = {
  searchPlaces: (...a) => mockSearchPlaces(...a),
  findPlaceFromText: (...a) => mockFindPlaceFromText(...a),
  getPlaceDetails: (...a) => mockGetPlaceDetails(...a),
  buildPlaceUrl: (name, placeId) => {
    const url = new URL('https://www.google.com/maps/search/');
    url.searchParams.set('api', '1');
    url.searchParams.set('query', name || placeId);
    url.searchParams.set('query_place_id', placeId);
    return url.toString();
  },
  PLACE_ID_SENTINEL_PREFIX: 'qurl_place:',
  PLACE_ID_SHAPE_RE,
  encodePlaceIdSentinel: (placeId) => `qurl_place:${placeId}`,
  decodePlaceIdSentinel: (value) => {
    if (typeof value !== 'string' || !value.startsWith('qurl_place:')) return null;
    const placeId = value.slice('qurl_place:'.length);
    return PLACE_ID_SHAPE_RE.test(placeId) ? placeId : null;
  },
};

module.exports = {
  mockPlacesModule,
  mockSearchPlaces,
  mockFindPlaceFromText,
  mockGetPlaceDetails,
  PLACE_ID_SHAPE_RE,
};
