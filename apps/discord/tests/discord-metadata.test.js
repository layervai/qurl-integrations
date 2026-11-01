const fs = require('fs');
const path = require('path');

const metadata = require('../discord-metadata.json');

describe('discord-metadata.json', () => {
  test('pins the LayerV-owned qURL Discord application identity and brand', () => {
    expect(metadata.bot.username).toBe('qURL');
    expect(metadata.bot.unique_username).toBe('qurl');
    expect(metadata.bot.unique_username).toBe(metadata.bot.username.toLowerCase());
    expect(metadata.application.name).toBe('qURL (sandbox)');
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

  test('keeps documented LayerV app identity in sync with metadata', () => {
    const root = path.join(__dirname, '..');
    const readme = fs.readFileSync(path.join(root, 'README.md'), 'utf8');
    const envExample = fs.readFileSync(path.join(root, '.env.example'), 'utf8');

    expect(readme).toContain(`Application ID: \`${metadata.application.id}\``);
    expect(readme).toContain(`Public Key: \`${metadata.application.public_key}\``);
    expect(envExample).toContain(`LayerV sandbox app: ${metadata.application.id}`);
    expect(envExample).toContain('DISCORD_CLIENT_ID=your_discord_application_client_id');
  });
});
