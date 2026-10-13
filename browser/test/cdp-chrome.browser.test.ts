/**
 * close() against real Chrome (#317): it finds Chrome's helper processes by
 * the profile on their command lines. If a Chrome release stopped naming the
 * profile there, close() would wait for nothing and the profile race would
 * be back, silently; this is what notices. cdp-chrome.test.ts covers the
 * waiting itself with a stand-in Chrome.
 */
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';
import { existsSync } from 'node:fs';
import { openTab } from './helpers/cdp-chrome.js';

test('close() sees real Chrome\'s helpers by its profile, and none of them outlives it', async () => {
  const tab = await openTab('data:text/html,<p>a page, so a renderer</p>');
  const profile = tab.profile;
  const report = await tab.close();

  assert.ok(report.atClose.includes('browser') && report.atClose.includes('renderer'),
    `close() did not find Chrome's processes by the profile: saw [${report.atClose.join(', ')}]`);
  assert.equal(report.problem, undefined);
  assert.equal(report.removed, true);
  assert.equal(existsSync(profile), false);
  const ps = execFileSync('ps', ['-A', '-ww', '-o', 'command='], { encoding: 'utf8' });
  assert.equal(ps.split('\n').filter((l) => l.includes(profile)).length, 0, 'a Chrome process outlived close()');
});
