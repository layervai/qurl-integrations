const {
  SENDER_ID,
  handleQurlMap,
  isOnCooldown,
  makeInteraction,
  mockFindPlaceFromText,
  mockGetPlaceDetails,
  mockSupersedeOrCreate,
} = require('./helpers/qurl-send-fixtures');

describe('handleQurlMap — slash entry', () => {
  test('Google Maps URL → URL preserved + name extracted from /place/', async () => {
    const int = makeInteraction({
      options: {
        location: 'https://www.google.com/maps/place/Eiffel+Tower/@48.8,2.3',
        recipients: '<@100000000000000001>',
      },
      guildMembers: { '100000000000000001': {} },
    });
    await handleQurlMap(int);
    expect(mockSupersedeOrCreate).toHaveBeenCalled();
    const payload = mockSupersedeOrCreate.mock.calls[0][0].payload;
    expect(payload.resourceType).toBe('maps');
    expect(payload.locationUrl).toMatch(/google\.com\/maps\/place\/Eiffel/);
    expect(payload.locationName).toMatch(/Eiffel Tower/);
  });

  test('deferReply throws (expired token) → cooldown cleared, no flow row, no editReply', async () => {
    const int = makeInteraction({
      options: { location: 'somewhere', recipients: '<@100000000000000001>' },
      guildMembers: { '100000000000000001': {} },
    });
    int.deferReply = jest.fn(async () => { const e = new Error('Unknown interaction'); e.code = 10062; throw e; });
    await handleQurlMap(int);
    expect(isOnCooldown(SENDER_ID)).toBe(false);
    expect(int.editReply).not.toHaveBeenCalled();
    expect(mockSupersedeOrCreate).not.toHaveBeenCalled();
  });

  test('defers ONCE — handleQurlSlashSend skips its own defer when already deferred', async () => {
    mockFindPlaceFromText.mockResolvedValueOnce({ placeId: 'ChIJ1', name: 'Place', address: '' });
    const int = makeInteraction({
      options: { location: 'somewhere', recipients: '<@100000000000000001>' },
      guildMembers: { '100000000000000001': {} },
    });
    await handleQurlMap(int);
    expect(int.deferReply).toHaveBeenCalledTimes(1);
  });

  test('arbitrary text → resolved through Places to a place_id-pinned URL', async () => {
    mockFindPlaceFromText.mockResolvedValueOnce({
      placeId: 'ChIJ4zGFAZpYwokRGUGph3Mf37k',
      name: 'Central Park',
      address: 'New York, NY',
    });
    const int = makeInteraction({
      options: {
        location: 'Central Park, NYC',
        recipients: '<@100000000000000001>',
      },
      guildMembers: { '100000000000000001': {} },
    });
    await handleQurlMap(int);
    expect(mockFindPlaceFromText).toHaveBeenCalledWith('Central Park, NYC');
    const payload = mockSupersedeOrCreate.mock.calls[0][0].payload;
    expect(payload.locationUrl).toContain('query_place_id=ChIJ4zGFAZpYwokRGUGph3Mf37k');
    expect(payload.locationName).toMatch(/Central Park/);
  });

  test('free-text input is trimmed + 500-char-capped before reaching Places', async () => {
    mockFindPlaceFromText.mockResolvedValueOnce({
      placeId: 'ChIJ4zGFAZpYwokRGUGph3Mf37k', name: 'X', address: 'Y',
    });
    const padding = '  '.repeat(40); // 80 chars of leading whitespace, trimmed first
    const content = 'a'.repeat(600); // 600 chars of content, slice() caps at 500
    const int = makeInteraction({
      options: {
        location: padding + content + padding,
        recipients: '<@100000000000000001>',
      },
      guildMembers: { '100000000000000001': {} },
    });
    await handleQurlMap(int);
    const calledWith = mockFindPlaceFromText.mock.calls[0][0];
    expect(calledWith.length).toBe(500);
    expect(calledWith.startsWith('a')).toBe(true);
    expect(calledWith.endsWith('a')).toBe(true);
  });

  test('place_id sentinel from autocomplete → resolved through Place Details', async () => {
    mockGetPlaceDetails.mockResolvedValueOnce({
      placeId: 'ChIJ37FjGE63t4kRD2_jXSF1F9o',
      name: 'The White House',
      address: '1600 Pennsylvania Ave NW',
    });
    const int = makeInteraction({
      options: {
        location: 'qurl_place:ChIJ37FjGE63t4kRD2_jXSF1F9o',
        recipients: '<@100000000000000001>',
      },
      guildMembers: { '100000000000000001': {} },
    });
    await handleQurlMap(int);
    expect(mockGetPlaceDetails).toHaveBeenCalledWith('ChIJ37FjGE63t4kRD2_jXSF1F9o');
    const payload = mockSupersedeOrCreate.mock.calls[0][0].payload;
    expect(payload.locationUrl).toContain('query_place_id=ChIJ37FjGE63t4kRD2_jXSF1F9o');
    expect(payload.locationName).toBe('The White House');
  });

  test('Places returns no match → actionable ephemeral, cooldown cleared, no flow row', async () => {
    mockFindPlaceFromText.mockResolvedValueOnce(null);
    const int = makeInteraction({
      options: {
        location: 'zzzz-no-such-place',
        recipients: '<@100000000000000001>',
      },
      guildMembers: { '100000000000000001': {} },
    });
    await handleQurlMap(int);
    expect(int.deferReply).toHaveBeenCalledWith(expect.objectContaining({ ephemeral: true }));
    expect(int.editReply).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/Couldn't find/),
    }));
    expect(mockSupersedeOrCreate).not.toHaveBeenCalled();
    expect(isOnCooldown(SENDER_ID)).toBe(false);
  });

  test('stale-sentinel NOT_FOUND → place-specific message (does NOT echo the wire sentinel)', async () => {
    mockGetPlaceDetails.mockResolvedValueOnce(null);
    const int = makeInteraction({
      options: {
        location: 'qurl_place:ChIJ-deleted-place',
        recipients: '<@100000000000000001>',
      },
      guildMembers: { '100000000000000001': {} },
    });
    await handleQurlMap(int);
    const editReplyCall = int.editReply.mock.calls[0][0];
    expect(editReplyCall.content).toMatch(/no longer available/);
    expect(editReplyCall.content).not.toContain('qurl_place:');
    expect(isOnCooldown(SENDER_ID)).toBe(false);
  });

  test('Places call throws → actionable ephemeral, cooldown cleared, no flow row', async () => {
    mockFindPlaceFromText.mockRejectedValueOnce(new Error('upstream timeout'));
    const int = makeInteraction({
      options: {
        location: 'somewhere',
        recipients: '<@100000000000000001>',
      },
      guildMembers: { '100000000000000001': {} },
    });
    await handleQurlMap(int);
    expect(int.editReply).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/lookup failed/),
    }));
    expect(mockSupersedeOrCreate).not.toHaveBeenCalled();
    expect(isOnCooldown(SENDER_ID)).toBe(false);
  });

  test('location-name override wins over URL-derived name', async () => {
    const int = makeInteraction({
      options: {
        location: 'https://www.google.com/maps/place/Eiffel+Tower',
        'location-name': 'Custom Label',
        recipients: '<@100000000000000001>',
      },
      guildMembers: { '100000000000000001': {} },
    });
    await handleQurlMap(int);
    const payload = mockSupersedeOrCreate.mock.calls[0][0].payload;
    expect(payload.locationName).toBe('Custom Label');
  });

  test('empty location string → ephemeral error, cooldown CLEARED (honest user error)', async () => {
    const int = makeInteraction({
      options: { location: '   ', recipients: '<@100000000000000001>' },
    });
    await handleQurlMap(int);
    expect(int.reply).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/empty/),
    }));
    expect(isOnCooldown(SENDER_ID)).toBe(false);
  });

  test('rejects in DM context', async () => {
    const int = makeInteraction({
      guildId: null,
      options: { location: 'Eiffel', recipients: '<@100000000000000001>' },
    });
    await handleQurlMap(int);
    expect(int.reply).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/in a server/),
    }));
  });

  test('locationName strips bidi/zero-width control chars (RLO spoof defense)', async () => {
    const int = makeInteraction({
      options: {
        location: 'https://www.google.com/maps/place/Cafe',
        'location-name': '‮Backwards Cafe',
        recipients: '<@100000000000000001>',
      },
      guildMembers: { '100000000000000001': {} },
    });
    await handleQurlMap(int);
    const payload = mockSupersedeOrCreate.mock.calls[0][0].payload;
    expect(payload.locationName).toBe('Backwards Cafe');
    expect(payload.locationName).not.toContain('‮');
  });

  test('locationName 256-cap is codepoint-aware (no surrogate split)', async () => {
    const name = 'a'.repeat(254) + '😀' + 'extra';  // 254 + 2 surrogates + 5 = 261 code units
    const int = makeInteraction({
      options: {
        location: 'https://www.google.com/maps/place/Cafe',
        'location-name': name,
        recipients: '<@100000000000000001>',
      },
      guildMembers: { '100000000000000001': {} },
    });
    await handleQurlMap(int);
    const payload = mockSupersedeOrCreate.mock.calls[0][0].payload;
    const lone = /[\uD800-\uDBFF](?![\uDC00-\uDFFF])|(?<![\uD800-\uDBFF])[\uDC00-\uDFFF]/;
    expect(payload.locationName).not.toMatch(lone);
  });

  test('forged interaction missing required `location` → actionable ephemeral, no flow row, cooldown cleared', async () => {
    const int = makeInteraction({ options: {} });
    int.options.getString = jest.fn((name, required) => {
      if (name === 'location' && required) {
        const err = new Error('CommandInteractionOptionNotFound');
        err.code = 'CommandInteractionOptionNotFound';
        throw err;
      }
      return null;
    });
    await handleQurlMap(int);
    expect(int.reply).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/`location:` option is required/),
      ephemeral: true,
    }));
    expect(mockSupersedeOrCreate).not.toHaveBeenCalled();
    expect(isOnCooldown(SENDER_ID)).toBe(false);
  });
});
