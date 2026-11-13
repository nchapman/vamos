import test from 'node:test';
import assert from 'node:assert/strict';
import {plainText, validateFixtures, expectedLines, standaloneCommands, contrastRatio} from './model.js';

const fixture = overrides => ({case: 'help-root', theme: 'terminal', width: 80, ansi: 'Soar\n', ...overrides});

test('plainText removes only SGR, preserving Unicode, tabs, spaces and newlines', () => {
  assert.equal(plainText('\x1b[1mSoar\x1b[0m\n\t界 é  \n'), 'Soar\n\t界 é  \n');
  assert.equal(plainText('\x1b[38;2;20;30;40mRGB\x1b[m\x1b[38:2::1:2:3m!'), 'RGB!');
  assert.equal(plainText(''), '');
});

test('plainText rejects non-SGR controls rather than hiding them', () => {
  for (const control of ['\x1b[2J', '\x1b[H', '\x1b]0;title\x07', '\r', '\b', '\x00', '\x7f', '\x85', '\x1b[31']) {
    assert.throws(() => plainText(`before${control}after`), /non-SGR/);
  }
});

test('validateFixtures accepts supported themes and widths without mutating input', () => {
  const fixtures = ['dark', 'light', 'mono', 'terminal'].flatMap(theme =>
    [48, 80, 120].map(width => fixture({theme, width})));
  const before = structuredClone(fixtures);
  assert.equal(validateFixtures(fixtures), fixtures);
  assert.deepEqual(fixtures, before);
  validateFixtures([fixture({width: 8}), fixture({width: 500})]);
});

test('validateFixtures rejects missing, malformed, duplicate and unsafe fixtures', () => {
  for (const value of [null, {}, [], 'fixtures']) assert.throws(() => validateFixtures(value), /No visual fixtures/);
  for (const value of [null, undefined, 'fixture', fixture({case: undefined}), fixture({case: 'Bad/path'}),
    fixture({case: 123}), fixture({theme: 'auto'}), fixture({width: '80'}), fixture({width: 7}),
    fixture({width: 501}), fixture({width: 48.5}), fixture({ansi: ''}), fixture({ansi: null})]) {
    assert.throws(() => validateFixtures([value]), /Invalid fixture/);
  }
  assert.throws(() => validateFixtures([fixture(), fixture({ansi: 'Different'})]), /Duplicate fixture/);
  assert.throws(() => validateFixtures([fixture({ansi: '\x1b[2J'})]), /non-SGR/);
});

test('expectedLines preserves logical lines, indentation and Unicode, ignoring right spaces only', () => {
  assert.deepEqual(expectedLines('  heading  \n界 é\n\n', 48), ['  heading', '界 é', '', '']);
  assert.deepEqual(expectedLines('a'.repeat(100), 48), ['a'.repeat(100)]);
  assert.deepEqual(expectedLines('', 80), ['']);
});

test('expectedLines expands ASCII tabs at eight-column stops and clamps at edge', () => {
  assert.deepEqual(expectedLines('a\tb\tc', 80), ['a       b       c']);
  assert.deepEqual(expectedLines('12345678\tx', 10), ['12345678 x']);
  assert.deepEqual(expectedLines('12345678x\tx', 10), ['12345678xx']);
  assert.deepEqual(expectedLines('1234567890a\tb', 10), ['1234567890a       b']);
  assert.throws(() => expectedLines('界\tvalue', 80), /Non-ASCII tab line/);
});

test('standaloneCommands selects only unindented non-template command lines', () => {
  const eligible = ['soar status', "soar logs --app 'café'", 'soar help service remove'];
  const excluded = [' soar status', '\tsoar status', 'soar status  Show status', 'soar status ',
    'soar deploy <app>', 'soar logs [flags]', 'soar logs | cat', 'soar\tstatus', 'soar status\r',
    'Soar status', 'soar', '$ soar status'];
  assert.deepEqual(standaloneCommands([...eligible, ...excluded].join('\n')), eligible);
});

test('contrastRatio matches reference ratios and is symmetric', () => {
  assert.equal(contrastRatio([0, 0, 0], [255, 255, 255]), 21);
  assert.equal(contrastRatio([42, 42, 42], [42, 42, 42]), 1);
  const gray = contrastRatio([119, 119, 119], [255, 255, 255]);
  assert.ok(Math.abs(gray - 4.478089453577214) < 1e-10);
  assert.equal(gray, contrastRatio([255, 255, 255], [119, 119, 119]));
  assert.ok(contrastRatio([10, 10, 10], [255, 255, 255]) > 19);
});
