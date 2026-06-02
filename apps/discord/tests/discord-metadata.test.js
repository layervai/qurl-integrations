const fs = require('fs');
const path = require('path');

const metadata = require('../discord-metadata.json');

describe('discord-metadata.json', () => {
  test('pins the LayerV-owned qURL Discord application identity and brand', () => {
    expect(metadata.bot.username).toBe('qURL');
    expect(metadata.application.name).toBe('qURL');
    expect(metadata.application.id).toBe('1511450217789128885');
    expect(metadata.application.public_key).toBe('f951fb4d407da2ac37ebb862f074e311d530b6e95940984695a320a1ac9f00ea');
  });

  test('references existing image assets and install permissions', () => {
    for (const asset of [
      metadata.bot.avatar,
      metadata.bot.banner,
      metadata.application.icon,
      metadata.application.cover_image,
    ]) {
      expect(fs.existsSync(path.join(__dirname, '..', asset))).toBe(true);
    }

    expect(metadata.application.install_params.scopes).toEqual(['bot', 'applications.commands']);
    expect(metadata.application.install_params.permissions).toBe('2147503104');
  });
});
