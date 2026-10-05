import { test } from 'node:test';
import assert from 'node:assert/strict';
import { createHash } from 'node:crypto';

import { sha256Hex } from '../src/sha256.js';

// Known answers, so a failure here points at the hash rather than at the
// fingerprint rules.
test('matches published SHA-256 vectors', () => {
  assert.equal(sha256Hex(''), 'e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855');
  assert.equal(sha256Hex('abc'), 'ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad');
  assert.equal(
    sha256Hex('abcdbcdecdefdefgefghfghighijhijkijkljklmklmnlmnomnopnopq'),
    '248d6a61d20638b8e5c026930c3e6039a33ce45964ff2167f6ecedd419db06c1',
  );
});

// Cross-check against Node's own implementation over inputs that exercise
// multi-block messages, unicode, and the padding boundary.
test('agrees with node:crypto across tricky inputs', () => {
  const inputs = [
    'a'.repeat(55), 'a'.repeat(56), 'a'.repeat(63), 'a'.repeat(64), 'a'.repeat(65), 'a'.repeat(1000),
    'héllo wörld', '日本語テキスト', '🙂 emoji outside the BMP 🚀',
    'v1\u001eerror\u001eclient\u001e\u001eTypeError\u001eboom\u001ef@a.js',
  ];

  for (const input of inputs) {
    assert.equal(sha256Hex(input), createHash('sha256').update(input, 'utf8').digest('hex'), `input: ${input.slice(0, 40)}`);
  }
});
