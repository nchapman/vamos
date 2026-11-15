// Only SGR is presentation. Never silently discard cursor movement, OSC, or text.
export function plainText(ansi) {
  const plain = ansi.replace(/\x1b\[[0-9;:]*m/g, '');
  if (/[\x00-\x08\x0b-\x1f\x7f-\x9f]/u.test(plain)) {
    throw new Error('Fixture contains non-SGR terminal controls');
  }
  return plain;
}

export function validateFixtures(fixtures) {
  if (!Array.isArray(fixtures) || !fixtures.length) throw new Error('No visual fixtures');
  const keys = new Set();
  for (const f of fixtures) {
    if (!f || typeof f !== 'object' || typeof f.case !== 'string' ||
        !/^[a-z0-9][a-z0-9-]*$/.test(f.case) ||
        !['dark', 'light', 'mono', 'terminal'].includes(f.theme) ||
        !Number.isInteger(f.width) || f.width < 8 || f.width > 500 ||
        typeof f.ansi !== 'string' || !f.ansi.length) {
      throw new Error(`Invalid fixture: ${String(JSON.stringify(f)).slice(0, 200)}`);
    }
    const key = `${f.case}-${f.theme}-${f.width}`;
    if (keys.has(key)) throw new Error(`Duplicate fixture: ${key}`);
    keys.add(key);
    plainText(f.ansi);
  }
  return fixtures;
}

// xterm uses 8-column stops, clamped to the last column. Restrict tab-bearing
// lines to ASCII so this normalization never guesses Unicode display widths.
// Other lines retain Unicode verbatim. Only terminal right-padding is ignored.
export function expectedLines(plain, width) {
  return plain.split('\n').map(line => {
    if (line.includes('\t')) {
      if (/[^\x20-\x7e\t]/u.test(line)) throw new Error('Non-ASCII tab line needs a display-width normalizer');
      let column = 0;
      let expanded = '';
      for (const char of line) {
        if (char === '\t') {
          const next = Math.min(width - 1, (Math.floor(column / 8) + 1) * 8);
          expanded += ' '.repeat(Math.max(0, next - column));
          column = next;
        } else {
          if (column === width) column = 0;
          expanded += char;
          column++;
        }
      }
      line = expanded;
    }
    return line.replace(/ +$/, '');
  });
}

// Help contains command tables, placeholders, and many examples. Check only
// standalone, unindented command lines, never table descriptions or templates.
// This is selection eligibility, not shell parsing; nothing is ever executed.
export function standaloneCommands(plain) {
  return plain.split('\n').filter(line => /^vamos [a-z]/.test(line) &&
    !/[\t\r\n]| {2,}|[<>\[\]|]/.test(line) && line === line.trimEnd());
}

export function contrastRatio(foreground, background) {
  const luminance = rgb => rgb.map(v => {
    v /= 255;
    return v <= 0.04045 ? v / 12.92 : ((v + 0.055) / 1.055) ** 2.4;
  }).reduce((sum, v, i) => sum + v * [0.2126, 0.7152, 0.0722][i], 0);
  const a = luminance(foreground), b = luminance(background);
  return (Math.max(a, b) + 0.05) / (Math.min(a, b) + 0.05);
}
