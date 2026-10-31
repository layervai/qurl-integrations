const { autocompleteBuckets } = require('../src/utils/autocomplete-rate-limit');
const {
  AUTOCOMPLETE_FAILURE_LOG_BURST,
  _resetAutocompleteFailureBurst,
  handleAutocomplete,
  mockSearchPlaces,
} = require('./helpers/qurl-send-fixtures');

describe('handleAutocomplete', () => {
  beforeEach(() => {
    mockSearchPlaces.mockReset().mockResolvedValue([]);
    _resetAutocompleteFailureBurst();
    autocompleteBuckets.clear();
  });

  function makeAutocompleteInteraction({
    subcommand = 'map',
    focused = { name: 'location', value: 'whitehouse' },
    guildId = 'guild-1',
    userId = 'user-1',
    authorizingIntegrationOwners,
  } = {}) {
    const respond = jest.fn().mockResolvedValue(undefined);
    return {
      commandName: 'qurl',
      user: { id: userId },
      guildId,
      authorizingIntegrationOwners,
      respond,
      options: {
        getSubcommand: () => subcommand,
        getFocused: () => focused,
      },
    };
  }

  function fakePlaceId(seed) {
    const s = String(seed);
    return s.length >= 16 ? s : `ChIJ${'a'.repeat(16 - s.length)}${s}`;
  }

  test('rotating prefixes and guilds share one user budget before Places I/O', async () => {
    for (let i = 0; i < 11; i++) {
      const int = makeAutocompleteInteraction({ guildId: `guild-${i}`, focused: { name: 'location', value: `prefix-${i}` } });
      await handleAutocomplete(int);
      expect(int.respond).toHaveBeenCalledWith([]);
    }
    expect(mockSearchPlaces).toHaveBeenCalledTimes(10);
    await handleAutocomplete(makeAutocompleteInteraction({ userId: 'other-user' }));
    expect(mockSearchPlaces).toHaveBeenCalledTimes(11);
  });

  test('missing user identity fails closed without Places I/O', async () => {
    const int = makeAutocompleteInteraction();
    delete int.user;
    await handleAutocomplete(int);
    expect(int.respond).toHaveBeenCalledWith([]);
    expect(mockSearchPlaces).not.toHaveBeenCalled();
  });

  test('responds empty for non-qurl commands', async () => {
    const int = makeAutocompleteInteraction();
    int.commandName = 'link';
    await handleAutocomplete(int);
    expect(int.respond).toHaveBeenCalledWith([]);
    expect(mockSearchPlaces).not.toHaveBeenCalled();
  });

  test('responds empty for DM autocomplete (no guildId)', async () => {
    const int = makeAutocompleteInteraction({ guildId: null });
    await handleAutocomplete(int);
    expect(int.respond).toHaveBeenCalledWith([]);
    expect(mockSearchPlaces).not.toHaveBeenCalled();
  });

  test('responds empty for user-install autocomplete invoked inside a guild', async () => {
    const int = makeAutocompleteInteraction({
      authorizingIntegrationOwners: { 1: 'user-1' },
    });
    await handleAutocomplete(int);
    expect(int.respond).toHaveBeenCalledWith([]);
    expect(mockSearchPlaces).not.toHaveBeenCalled();
  });

  test('responds empty for /qurl send (only /qurl map has suggestions)', async () => {
    const int = makeAutocompleteInteraction({ subcommand: 'send' });
    await handleAutocomplete(int);
    expect(int.respond).toHaveBeenCalledWith([]);
    expect(mockSearchPlaces).not.toHaveBeenCalled();
  });

  test('responds empty for /qurl map with a non-location focused option', async () => {
    const int = makeAutocompleteInteraction({ focused: { name: 'personal-message', value: 'hi' } });
    await handleAutocomplete(int);
    expect(int.respond).toHaveBeenCalledWith([]);
    expect(mockSearchPlaces).not.toHaveBeenCalled();
  });

  test('skips Places call for partial queries below the min-length cap', async () => {
    const int = makeAutocompleteInteraction({ focused: { name: 'location', value: 'a' } });
    await handleAutocomplete(int);
    expect(int.respond).toHaveBeenCalledWith([]);
    expect(mockSearchPlaces).not.toHaveBeenCalled();
  });

  test('skips Places call when input already looks like a URL', async () => {
    const int = makeAutocompleteInteraction({ focused: { name: 'location', value: 'https://goo.gl/maps/abc' } });
    await handleAutocomplete(int);
    expect(int.respond).toHaveBeenCalledWith([]);
    expect(mockSearchPlaces).not.toHaveBeenCalled();
  });

  test('returns sentinel-encoded choices with name + address labels', async () => {
    mockSearchPlaces.mockResolvedValueOnce([
      { placeId: fakePlaceId('whitehouse_dc_id'), name: 'The White House', address: '1600 Pennsylvania Ave NW, Washington, DC' },
      { placeId: fakePlaceId('whitehouse_uk_id'), name: 'Whitehouse Pub', address: 'Manchester, UK' },
    ]);
    const int = makeAutocompleteInteraction();
    await handleAutocomplete(int);
    expect(mockSearchPlaces).toHaveBeenCalledWith('whitehouse');
    expect(int.respond).toHaveBeenCalledTimes(1);
    const choices = int.respond.mock.calls[0][0];
    expect(choices).toHaveLength(2);
    expect(choices[0]).toEqual({
      name: 'The White House — 1600 Pennsylvania Ave NW, Washington, DC',
      value: `qurl_place:${fakePlaceId('whitehouse_dc_id')}`,
    });
    expect(choices[1].value).toBe(`qurl_place:${fakePlaceId('whitehouse_uk_id')}`);
    expect(choices[0].name).not.toBe(choices[1].name);
  });

  test('truncates a label exceeding the 100-char Discord cap (UTF-16 units)', async () => {
    const longAddress = '1234 Very Long Street Name, Somewhere Far Away, In A Large City With A Long Name, Region, Country 99999';
    mockSearchPlaces.mockResolvedValueOnce([{ placeId: fakePlaceId('longlabel'), name: 'Place', address: longAddress }]);
    const int = makeAutocompleteInteraction();
    await handleAutocomplete(int);
    const choice = int.respond.mock.calls[0][0][0];
    expect(choice.name.length).toBeLessThanOrEqual(100);
    expect(choice.value.length).toBeLessThanOrEqual(100);
  });

  test('boundary — exactly 100 UTF-16 units ending in a lone high surrogate gets backed off', async () => {
    const malformed = 'a'.repeat(99) + '\uD83D'; // lone high surrogate at index 99 → length 100
    expect(malformed.length).toBe(100);
    mockSearchPlaces.mockResolvedValueOnce([{ placeId: fakePlaceId('boundary1'), name: malformed, address: '' }]);
    const int = makeAutocompleteInteraction();
    await handleAutocomplete(int);
    const choice = int.respond.mock.calls[0][0][0];
    expect(choice.name.length).toBe(99);
    const loneHigh = /[\uD800-\uDBFF](?![\uDC00-\uDFFF])/;
    expect(choice.name).not.toMatch(loneHigh);
  });

  test('truncation does not split a surrogate pair (emoji-heavy label stays valid UTF-16)', async () => {
    const emoji = '🏛️'; // 🏛 + variation selector — 3 UTF-16 units
    const name = (emoji + 'X').repeat(40); // 160 UTF-16 units of mixed surrogate + ASCII
    mockSearchPlaces.mockResolvedValueOnce([{ placeId: fakePlaceId('emojiplace'), name, address: '' }]);
    const int = makeAutocompleteInteraction();
    await handleAutocomplete(int);
    const choice = int.respond.mock.calls[0][0][0];
    expect(choice.name.length).toBeLessThanOrEqual(100);
    const loneHigh = /[\uD800-\uDBFF](?![\uDC00-\uDFFF])/;
    expect(choice.name).not.toMatch(loneHigh);
  });

  test('drops a choice whose value would exceed the 100-char Discord cap', async () => {
    const good1 = fakePlaceId('good1_id');
    const good2 = fakePlaceId('good2_id');
    mockSearchPlaces.mockResolvedValueOnce([
      { placeId: good1, name: 'Good', address: 'addr' },
      { placeId: 'x'.repeat(95), name: 'Bad (too long)', address: 'addr' },
      { placeId: good2, name: 'Also Good', address: 'addr' },
    ]);
    const int = makeAutocompleteInteraction();
    await handleAutocomplete(int);
    const choices = int.respond.mock.calls[0][0];
    expect(choices).toHaveLength(2);
    expect(choices.map(c => c.value)).toEqual([`qurl_place:${good1}`, `qurl_place:${good2}`]);
  });

  test('drops a choice whose name is missing (Places returned no main_text + no description)', async () => {
    const valid = fakePlaceId('valid_for_label');
    mockSearchPlaces.mockResolvedValueOnce([
      { placeId: valid, name: 'Valid', address: 'addr' },
      { placeId: fakePlaceId('no_name_entry'), name: undefined, address: 'addr2' },
      { placeId: fakePlaceId('empty_name_xx'), name: '', address: 'addr3' },
    ]);
    const int = makeAutocompleteInteraction();
    await handleAutocomplete(int);
    const choices = int.respond.mock.calls[0][0];
    expect(choices).toHaveLength(1);
    expect(choices[0].value).toBe(`qurl_place:${valid}`);
  });

  test('outer-catch handles a rejection from an early-return respond() (return await contract)', async () => {
    const int = makeAutocompleteInteraction({ guildId: null });
    let respondCallCount = 0;
    int.respond = jest.fn(async () => {
      respondCallCount += 1;
      if (respondCallCount === 1) throw new Error('Unknown interaction');
    });
    await handleAutocomplete(int);
    expect(respondCallCount).toBe(2);
  });

  test('drops a choice whose place_id fails the documented shape check', async () => {
    const valid = fakePlaceId('valid_id_one');
    mockSearchPlaces.mockResolvedValueOnce([
      { placeId: valid, name: 'Valid', address: 'addr' },
      { placeId: 'tooshort', name: 'Bad short', address: 'addr' },
      { placeId: 'has spaces in it just bad', name: 'Bad chars', address: 'addr' },
    ]);
    const int = makeAutocompleteInteraction();
    await handleAutocomplete(int);
    const choices = int.respond.mock.calls[0][0];
    expect(choices).toHaveLength(1);
    expect(choices[0].value).toBe(`qurl_place:${valid}`);
  });

  test('caps results at 25 (Discord choice limit)', async () => {
    mockSearchPlaces.mockResolvedValueOnce(
      Array.from({ length: 40 }, (_, i) => ({
        placeId: fakePlaceId(`place_id_${i}_padding_xyz`),
        name: `Place ${i}`,
        address: 'addr',
      })),
    );
    const int = makeAutocompleteInteraction();
    await handleAutocomplete(int);
    expect(int.respond.mock.calls[0][0]).toHaveLength(25);
  });

  test('responds empty (does not throw) when Places API throws', async () => {
    mockSearchPlaces.mockRejectedValueOnce(new Error('Places API status: OVER_QUERY_LIMIT'));
    const int = makeAutocompleteInteraction();
    await handleAutocomplete(int);
    expect(int.respond).toHaveBeenCalledWith([]);
  });

  test('failure burst counter emits one warn per BURST failures (SRE outage signal)', async () => {
    const logger = require('../src/logger');
    let user = 0;
    const nextInteraction = () => makeAutocompleteInteraction({ userId: `outage-user-${user++}` });
    logger.warn.mockClear();
    for (let i = 0; i < AUTOCOMPLETE_FAILURE_LOG_BURST - 1; i++) {
      mockSearchPlaces.mockRejectedValueOnce(new Error('Places API status: UNKNOWN_ERROR'));
      await handleAutocomplete(nextInteraction());
    }
    const burstWarns = () => logger.warn.mock.calls.filter(
      (call) => call[0] === 'autocomplete handler failure burst',
    ).length;
    expect(burstWarns()).toBe(0);

    mockSearchPlaces.mockRejectedValueOnce(new Error('Places API status: UNKNOWN_ERROR'));
    await handleAutocomplete(nextInteraction());
    expect(burstWarns()).toBe(1);
    expect(logger.warn).toHaveBeenCalledWith(
      'autocomplete handler failure burst',
      expect.objectContaining({ count: AUTOCOMPLETE_FAILURE_LOG_BURST }),
    );

    for (let i = 0; i < AUTOCOMPLETE_FAILURE_LOG_BURST - 1; i++) {
      mockSearchPlaces.mockRejectedValueOnce(new Error('Places API status: UNKNOWN_ERROR'));
      await handleAutocomplete(nextInteraction());
    }
    expect(burstWarns()).toBe(1);
  });

  test('failure burst counter does not increment when the early-return respond() throws', async () => {
    const logger = require('../src/logger');
    logger.warn.mockClear();
    for (let i = 0; i < AUTOCOMPLETE_FAILURE_LOG_BURST + 5; i++) {
      const int = makeAutocompleteInteraction({ guildId: null }); // hits DM gate early-return
      int.respond = jest.fn(async () => { throw new Error('Unknown interaction'); });
      await handleAutocomplete(int);
    }
    const burstWarns = logger.warn.mock.calls.filter(
      (call) => call[0] === 'autocomplete handler failure burst',
    ).length;
    expect(burstWarns).toBe(0);
  });

  test('failure burst counter does not increment on the success path', async () => {
    const logger = require('../src/logger');
    logger.warn.mockClear();
    mockSearchPlaces.mockResolvedValue([{ placeId: 'ChIJ1', name: 'X', address: 'Y' }]);
    for (let i = 0; i < AUTOCOMPLETE_FAILURE_LOG_BURST + 5; i++) {
      await handleAutocomplete(makeAutocompleteInteraction());
    }
    const burstWarns = logger.warn.mock.calls.filter(
      (call) => call[0] === 'autocomplete handler failure burst',
    ).length;
    expect(burstWarns).toBe(0);
  });
});
