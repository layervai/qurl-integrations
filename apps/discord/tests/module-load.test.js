// Module-load smoke test — fails fast if any source module has an
// undefined identifier (no-undef) or a missing require/import. ESLint's
// `no-undef` rule covers most cases statically, but a CI run on a
// stale merge-ref can pass lint while the actually-merged code fails
// (see #204 / #202 for the regression that motivated this). Loading
// the modules at jest time gives us a runtime cross-check that
// doesn't depend on ESLint having seen the post-merge state.

describe('module load smoke', () => {
  // Discord-side require()s are heavy (discord.js, jose, AWS SDK) but
  // jest already imports a similar surface via the existing test files,
  // so the marginal cost here is negligible.
  test.each([
    ['../src/commands'],
    ['../src/qurl'],
    ['../src/connector'],
    ['../src/revoke-render'],
  ])('%s loads without ReferenceError', (modulePath) => {
    expect(() => require(modulePath)).not.toThrow();
  });
});
