/** SDK version: this package's own (the release tag browser-vX.Y.Z checks it equals package.json). */
export const VERSION = '0.8.1';

/**
 * This SDK's name in every flush's `sdk` field: the key
 * testdata/protocol_vectors.json `sdk.names` gives sdk/client
 * (docs/server-sdk/wire-protocol.md, "the SDK's name and version"). The
 * field's version is VERSION, never a second literal.
 */
export const SDK_NAME = 'browser';
