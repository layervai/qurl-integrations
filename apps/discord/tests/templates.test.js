/**
 * Tests for src/templates/page.js
 */

jest.mock('../src/constants', () => ({
  COLORS: {
    PRIMARY: 0x3498DB,
    SUCCESS: 0x2ECC71,
    WARNING: 0xF39C12,
    ERROR: 0xE74C3C,
  },
}));

const { renderPage } = require('../src/templates/page');

describe('server error handler and startServer', () => {
  it('startServer function exists and is callable', () => {
    const { startServer } = require('../src/server');
    expect(typeof startServer).toBe('function');
  });

});

describe('renderPage', () => {
  it('renders a success page with all fields', () => {
    const html = renderPage({
      title: 'Test Success',
      icon: '✅',
      heading: 'All Good',
      message: 'Everything worked.',
      subtext: 'You can close this page.',
      type: 'success',
      showDiscordButton: true,
    });

    expect(html).toContain('<title>Test Success - qURL</title>');
    expect(html).toContain('All Good');
    expect(html).toContain('Everything worked.');
    expect(html).toContain('You can close this page.');
    expect(html).toContain('Open Discord');
    expect(html).toContain('#2ecc71'); // SUCCESS color
  });

  it('renders an error page without subtext or discord button', () => {
    const html = renderPage({
      title: 'Test Error',
      icon: '❌',
      heading: 'Something Failed',
      message: 'Bad things happened.',
      type: 'error',
    });

    expect(html).toContain('Something Failed');
    expect(html).toContain('Bad things happened.');
    expect(html).not.toContain('Open Discord');
    expect(html).toContain('#e74c3c'); // ERROR color
  });

  it('renders a warning page', () => {
    const html = renderPage({
      title: 'Warning',
      icon: '⚠',
      heading: 'Watch Out',
      message: 'Be careful.',
      type: 'warning',
    });

    expect(html).toContain('#f39c12'); // WARNING color
  });

  it('defaults to info type for unknown type', () => {
    const html = renderPage({
      title: 'Unknown',
      icon: '?',
      heading: 'Hmm',
      message: 'Not sure what type.',
      type: 'unknown_type',
    });

    expect(html).toContain('#3498db'); // PRIMARY color (info default)
  });

  it('defaults to info type when type is omitted', () => {
    const html = renderPage({
      title: 'Default',
      icon: 'ℹ',
      heading: 'Info',
      message: 'Default info page.',
    });

    expect(html).toContain('#3498db'); // PRIMARY color
  });

  it('includes Open Discord link when showDiscordButton is true', () => {
    const html = renderPage({
      title: 'Test',
      icon: '✅',
      heading: 'H',
      message: 'M',
      showDiscordButton: true,
    });

    expect(html).toContain('Open Discord');
    expect(html).toContain('discord://');
  });

  it('does not include Open Discord link when showDiscordButton is false', () => {
    const html = renderPage({
      title: 'Test',
      icon: '✅',
      heading: 'H',
      message: 'M',
      showDiscordButton: false,
    });

    expect(html).not.toContain('Open Discord');
  });
});
