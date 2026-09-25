import { readFileSync, readdirSync } from 'node:fs';
import { join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

const root = process.argv[2] ? resolve(process.argv[2]) : resolve(fileURLToPath(new URL('..', import.meta.url)));
const localeDir = join(root, 'public', 'locales');
const errors = [];
const fail = (message) => errors.push(message);
const placeholderNames = (text) => [...text.matchAll(/\{([a-zA-Z][a-zA-Z0-9]*)\}/g)].map((match) => match[1]).sort().join(',');
const notificationContract = {
  'notifications.title.success': '',
  'notifications.title.completedWithIssues': '',
  'notifications.title.failed': '',
  'notifications.message.success': 'taskName',
  'notifications.message.completedWithIssues': 'taskName',
  'notifications.message.backupNativeSucceededWithIssues': 'taskName',
  'notifications.message.failed': 'taskName',
  'notifications.appUpdate.title': '',
  'notifications.appUpdate.message': '',
  'notifications.task.backup': 'name',
  'notifications.task.backupTarget': 'name,target',
  'notifications.task.check': 'name',
  'notifications.task.deleteSnapshot': 'name',
  'notifications.task.restoreSnapshot': 'name',
  'notifications.task.restoreSelectedItems': 'name',
};

// JSON.parse accepts a repeated object key and silently keeps its last value.
// Walk the token stream first so this review gate detects that accident even
// when the final parsed object appears complete.
function duplicateKeys(source, path) {
  let index = 0;
  const space = () => { while (/\s/.test(source[index] ?? '')) index++; };
  const string = () => {
    const start = index++;
    while (index < source.length) {
      if (source[index++] === '"') return JSON.parse(source.slice(start, index));
      if (source[index - 1] === '\\') index++;
    }
    throw new Error('unterminated string');
  };
  const value = (location) => {
    space();
    if (source[index] === '{') {
      index++;
      const seen = new Set();
      space();
      while (source[index] !== '}') {
        const key = string();
        if (seen.has(key)) fail(`${path}: duplicate key ${location}.${key}`);
        seen.add(key);
        space();
        if (source[index++] !== ':') throw new Error('expected colon');
        value(`${location}.${key}`);
        space();
        if (source[index] === '}') break;
        if (source[index++] !== ',') throw new Error('expected comma');
        space();
      }
      index++;
    } else if (source[index] === '[') {
      index++;
      space();
      while (source[index] !== ']') {
        value(location);
        space();
        if (source[index] === ']') break;
        if (source[index++] !== ',') throw new Error('expected comma');
      }
      index++;
    } else if (source[index] === '"') string();
    else { while (index < source.length && !/[\s,}\]]/.test(source[index])) index++; }
  };
  value('$');
}

function collectSourceFiles(dir) {
  return readdirSync(dir, { withFileTypes: true }).flatMap((entry) =>
    entry.isDirectory() ? collectSourceFiles(join(dir, entry.name)) :
    /\.tsx?$/.test(entry.name) && !/\.test\./.test(entry.name) ? [join(dir, entry.name)] : []);
}

const catalogs = new Map();
for (const filename of readdirSync(localeDir).filter((name) => name.endsWith('.json'))) {
  const path = join(localeDir, filename);
  const raw = readFileSync(path, 'utf8');
  try { duplicateKeys(raw, path); } catch (error) { fail(`${path}: ${error.message}`); }
  let catalog;
  try { catalog = JSON.parse(raw); } catch (error) { fail(`${path}: ${error.message}`); continue; }
  if (catalog.locale !== filename.slice(0, -5)) fail(`${path}: locale does not match filename`);
  let validLocale = false;
  try { validLocale = Intl.getCanonicalLocales(catalog.locale)[0] === catalog.locale; } catch { /* reported below */ }
  if (!validLocale) fail(`${path}: invalid or noncanonical BCP 47 locale`);
  if (!['ltr', 'rtl'].includes(catalog.direction)) fail(`${path}: invalid direction`);
  if (!catalog.messages || typeof catalog.messages !== 'object' || Array.isArray(catalog.messages)) {
    fail(`${path}: messages must be an object`); continue;
  }
  if (validLocale) catalogs.set(catalog.locale, catalog);
}
const english = catalogs.get('en');
if (!english) fail('Missing canonical en.json');
if (english) {
  const keys = Object.keys(english.messages);
  for (const [key, placeholders] of Object.entries(notificationContract)) {
    const message = english.messages[key];
    if (typeof message !== 'string' || placeholderNames(message) !== placeholders) fail(`Invalid required notification key or placeholders: ${key}`);
  }
  for (const key of keys.filter((item) => item.startsWith('notifications.'))) {
    if (!(key in notificationContract)) fail(`Unknown notification contract key: ${key}`);
  }
  const referenced = new Set();
  for (const file of collectSourceFiles(join(root, 'src'))) {
    const source = readFileSync(file, 'utf8');
    for (const match of source.matchAll(/\b(?:t|renderMessage)\(\s*["'`]([^"'`]+)["'`]/g)) referenced.add(match[1]);
  }
  // Backend notification calls consume the same catalog without appearing in
  // frontend source, so these are explicit cross-process contract keys.
  for (const key of Object.keys(notificationContract)) referenced.add(key);
  for (const key of keys.filter((item) => item.startsWith('ui.integration.'))) referenced.add(key);
  for (const key of referenced) if (!(key in english.messages)) fail(`Unknown source key: ${key}`);
  for (const key of keys) if (!referenced.has(key)) fail(`Unused catalog key: ${key}`);
  for (const [locale, catalog] of catalogs) {
    const requiredPluralCategories = new Set(new Intl.PluralRules(locale).resolvedOptions().pluralCategories);
    for (const key of keys) if (!(key in catalog.messages)) fail(`${locale}: missing key ${key}`);
    for (const key of Object.keys(catalog.messages)) if (!(key in english.messages)) fail(`${locale}: unknown key ${key}`);
    for (const [key, message] of Object.entries(catalog.messages)) {
      const base = english.messages[key];
      if (base === undefined) continue;
      const pluralCategories = new Set(['zero', 'one', 'two', 'few', 'many', 'other']);
      const valid = typeof message === 'string' || (message && typeof message === 'object' &&
        typeof message.other === 'string' &&
        [...requiredPluralCategories].every((category) => typeof message[category] === 'string') &&
        Object.keys(message).every((category) => pluralCategories.has(category) && typeof message[category] === 'string'));
      if (!valid || typeof message !== typeof base) { fail(`${locale}: invalid message shape ${key}`); continue; }
      const variants = typeof message === 'string' ? [['other', message]] : Object.entries(message);
      variants.forEach(([category, variant]) => {
        const baseVariant = typeof base === 'string' ? base : (base[category] ?? base.other);
        if (placeholderNames(variant) !== placeholderNames(baseVariant)) fail(`${locale}: placeholder mismatch ${key}`);
        if (/[<>]|https?:\/\/|www\.|\]\(/i.test(variant)) fail(`${locale}: markup or URL in ${key}`);
        if (/vault\.replicaro|\bGLACIER\b|\bDEEP_ARCHIVE\b/.test(variant)) fail(`${locale}: native identifier must be a placeholder in ${key}`);
        if (/[{}]/.test(variant.replace(/\{[a-zA-Z][a-zA-Z0-9]*\}/g, ''))) fail(`${locale}: invalid placeholder in ${key}`);
        for (const brand of ['Replicaro', 'Restic', 'Kopia', 'rclone']) {
          if (baseVariant.split(brand).length !== variant.split(brand).length) fail(`${locale}: changed brand ${brand} in ${key}`);
        }
      });
    }
  }
}
if (errors.length) { console.error(errors.join('\n')); process.exitCode = 1; }
else console.log(`Validated ${catalogs.size} locale catalog(s).`);
