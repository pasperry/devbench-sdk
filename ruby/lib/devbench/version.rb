# frozen_string_literal: true

module Devbench
  # The gem's release version. Each SDK has its own version and release tag;
  # the Ruby release (tag ruby-vX.Y.Z in the public repo) checks this equals
  # the tag. The wire format is PROTOCOL_VERSION below.
  VERSION = '0.8.1'

  # This SDK's name in every flush's `sdk` field: the key
  # testdata/protocol_vectors.json `sdk.names` gives sdk/server/ruby
  # (docs/server-sdk/wire-protocol.md, "the SDK's name and version"). The
  # field's version is VERSION, never a second literal.
  SDK_NAME = 'ruby'

  # The wire format this speaks: the `v` every payload carries (the flush
  # body, the evidence bundle, every control-socket message). Not the gem's
  # release VERSION above: it changes only for a breaking wire change, and
  # ingest tells formats apart by it. It must equal `current` in
  # testdata/protocol_vectors.json (docs/server-sdk/protocol-version.md, "Protocol
  # version"); send it, never a literal.
  PROTOCOL_VERSION = 1
end
