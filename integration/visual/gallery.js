import {Terminal} from '@xterm/xterm';
import '@xterm/xterm/css/xterm.css';
import {validateFixtures, plainText, expectedLines, standaloneCommands, contrastRatio} from './model.js';

const response = await fetch('fixtures.json');
if (!response.ok) throw new Error(`Fixtures: HTTP ${response.status}`);
const fixtures = validateFixtures(await response.json());
const palettes = {
  dark: {background: '#141a22', foreground: '#e4e8ee'},
  light: {background: '#faf9f5', foreground: '#20272f'},
};
// Explicit ANSI defaults make indexed-color checks independent of the browser.
const ansiColors = ['#000000', '#cd0000', '#00cd00', '#cdcd00', '#0000ee', '#cd00cd', '#00cdcd', '#e5e5e5',
  '#7f7f7f', '#ff0000', '#00ff00', '#ffff00', '#5c5cff', '#ff00ff', '#00ffff', '#ffffff'];
const ansiNames = ['black', 'red', 'green', 'yellow', 'blue', 'magenta', 'cyan', 'white',
  'brightBlack', 'brightRed', 'brightGreen', 'brightYellow', 'brightBlue', 'brightMagenta', 'brightCyan', 'brightWhite'];
const rgb = hex => [1, 3, 5].map(i => parseInt(hex.slice(i, i + 2), 16));
let term;
let current;

function options(id, values, selected) {
  const select = document.getElementById(id);
  select.replaceChildren(...values.map(value => new Option(String(value), String(value))));
  select.value = String(selected);
}
function updateSelectors(f) {
  options('case', [...new Set(fixtures.map(f => f.case))], f.case);
  options('theme', [...new Set(fixtures.filter(v => v.case === f.case).map(v => v.theme))], f.theme);
  options('width', fixtures.filter(v => v.case === f.case && v.theme === f.theme).map(v => v.width).sort((a,b) => a-b), f.width);
}

window.renderCase = async function(name, theme, width, background = theme === 'dark' ? 'dark' : 'light') {
  const fixture = fixtures.find(f => f.case === name && f.theme === theme && f.width === width);
  if (!fixture || !palettes[background]) throw new Error('Unknown fixture/palette');
  term?.dispose();
  const screen = document.querySelector('#screen');
  screen.replaceChildren(Object.assign(document.createElement('div'), {id: 'terminal'}));
  screen.style.background = palettes[background].background;
  updateSelectors(fixture);
  options('background', Object.keys(palettes), background);
  await document.fonts.ready;
  const plain = plainText(fixture.ansi);
  // Allocate a conservative full-transcript bound before writing: no scrolling
  // away long help/status content, and no fixed 100-row crop.
  const rows = plain.split('\n').reduce((sum, line) => sum + 1 +
    Math.ceil(([...line].length * 2 + (line.match(/\t/g)?.length || 0) * 8) / width), 0);
  term = new Terminal({cols: width, rows, scrollback: 0,
    theme: {...palettes[background], ...Object.fromEntries(ansiNames.map((name, i) => [name, ansiColors[i]]))},
    fontFamily: 'Menlo, Consolas, "Liberation Mono", monospace', fontSize: 14, lineHeight: 1.25,
    letterSpacing: 0, allowTransparency: false, convertEol: true, disableStdin: true,
    drawBoldTextInBrightColors: false, cursorBlink: false});
  term.open(document.querySelector('#terminal'));
  await new Promise(resolve => term.write('\x1b[?25l' + fixture.ansi, resolve));
  if (term.buffer.active.baseY !== 0) throw new Error('Transcript scrolled out of capture buffer');
  term.resize(width, term.buffer.active.cursorY + 1);
  await new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve)));
  const params = new URLSearchParams({case: name, theme, width, background});
  history.replaceState(null, '', `?${params}`);
  document.querySelector('#summary').textContent = `${name} · ${width} columns · ${theme} · ${background} background · ${term.rows} rows`;
  current = {fixture, plain, background};
  return {case: name, theme, width, background, rows: term.rows};
};

function logicalLines() {
  const lines = [];
  for (let row = 0; row < term.rows; row++) {
    const line = term.buffer.active.getLine(row);
    const next = term.buffer.active.getLine(row + 1);
    let text = line.translateToString(false);
    // A wide character wraps early when only one cell remains. That last null
    // cell is layout padding, not an original space.
    if (next?.isWrapped && next.getCell(0).getWidth() === 2 &&
        line.getCell(term.cols - 1).getChars() === '') text = text.slice(0, -1);
    if (line.isWrapped && lines.length) lines.at(-1).text += text;
    else lines.push({row, text});
  }
  return lines.map(line => ({...line, text: line.text.replace(/ +$/, '')}));
}

function cellColor(cell, foreground) {
  const value = foreground ? cell.getFgColor() : cell.getBgColor();
  if (foreground ? cell.isFgDefault() : cell.isBgDefault()) {
    return rgb(palettes[current.background][foreground ? 'foreground' : 'background']);
  }
  if (foreground ? cell.isFgRGB() : cell.isBgRGB()) return [value >> 16 & 255, value >> 8 & 255, value & 255];
  if (value < 16) return rgb(ansiColors[value]);
  if (value >= 232) return Array(3).fill(8 + (value - 232) * 10);
  const n = value - 16, ramp = [0, 95, 135, 175, 215, 255];
  return [ramp[Math.floor(n / 36)], ramp[Math.floor(n / 6) % 6], ramp[n % 6]];
}

window.evaluateCurrent = function() {
  const {fixture, plain} = current;
  const lines = logicalLines();
  const expected = expectedLines(plain, fixture.width);
  const actual = lines.map(line => line.text);
  if (JSON.stringify(actual) !== JSON.stringify(expected)) {
    const i = expected.findIndex((line, i) => line !== actual[i]);
    throw new Error(`Logical content mismatch at line ${i + 1}: expected ${JSON.stringify(expected[i])}, got ${JSON.stringify(actual[i])}; lengths ${expected.length}/${actual.length}`);
  }
  const commands = standaloneCommands(plain);
  const input = document.createElement('textarea');
  input.setAttribute('aria-label', 'Non-executing command selection check');
  document.body.append(input);
  try {
    for (const command of commands) {
      const line = lines.find(line => line.text === command);
      if (!line) throw new Error(`Standalone command not found: ${command}`);
      // Count terminal cells, not UTF-16 units; wide Unicode and soft wraps
      // must survive xterm's own selection path, not a copy from the fixture.
      let remaining = command, length = 0;
      for (let row = line.row; remaining.length && row < term.rows; row++) {
        for (let col = 0; remaining.length && col < term.cols; col++) {
          const cell = term.buffer.active.getLine(row).getCell(col);
          if (cell.getWidth() === 0) continue;
          const chars = cell.getChars() || ' ';
          if (!remaining.startsWith(chars)) throw new Error('Command cell mismatch');
          remaining = remaining.slice(chars.length);
          length += cell.getWidth();
        }
      }
      if (remaining) throw new Error('Incomplete command selection');
      term.select(0, line.row, length);
      input.value = term.getSelection();
      term.clearSelection();
      if (input.value !== command) throw new Error(`Selected command differs: ${JSON.stringify(input.value)}`);
    }
  } finally { input.remove(); }
  let contrast = Infinity;
  const pairs = new Set();
  for (let row = 0; row < term.rows; row++) {
    const line = term.buffer.active.getLine(row);
    for (let col = 0; col < term.cols; col++) {
      const cell = line.getCell(col);
      if (!cell.getChars().trim()) continue;
      let fg = cellColor(cell, true), bg = cellColor(cell, false);
      if (cell.isInverse()) [fg, bg] = [bg, fg];
      if (cell.isDim()) fg = fg.map((v, i) => (v + bg[i]) / 2);
      if (cell.isInvisible()) throw new Error('Invisible fixture text');
      const key = `${fg}/${bg}`;
      if (pairs.has(key)) continue;
      pairs.add(key);
      contrast = Math.min(contrast, contrastRatio(fg, bg));
    }
  }
  if (!Number.isFinite(contrast) || contrast < 4.5) throw new Error(`Insufficient text contrast: ${contrast}`);
  return {contrast: Number(contrast.toFixed(2)), logicalTextPreserved: true,
    tabExpansionNormalized: plain.includes('\t'), rightPaddingNormalized: true,
    commandSelectionsPreserved: commands.length, logicalLines: expected.length};
};

for (const el of document.querySelectorAll('select')) el.addEventListener('change', async () => {
  const name = document.querySelector('#case').value;
  const theme = document.querySelector('#theme').value;
  const width = +document.querySelector('#width').value;
  const candidates = fixtures.filter(f => f.case === name);
  const fixture = candidates.find(f => f.theme === theme && f.width === width) ||
    candidates.find(f => f.theme === theme) || candidates[0];
  await window.renderCase(fixture.case, fixture.theme, fixture.width, document.querySelector('#background').value);
});
const params = new URLSearchParams(location.search);
const initial = fixtures.find(f => f.case === params.get('case') && f.theme === params.get('theme') && f.width === +params.get('width')) || fixtures[0];
await window.renderCase(initial.case, initial.theme, initial.width, params.get('background') || undefined);
window.ready = true;
