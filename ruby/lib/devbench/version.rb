# frozen_string_literal: true

module Devbench
  # The gem's release version. Each SDK has its own version and release tag;
  # the Ruby release (tag ruby-vX.Y.Z in the public repo) checks this equals
  # the tag. The wire format is PROTOCOL_VERSION below.
  VERSION = '0.7.0'

  # The wire format this speaks: the `v` every payload carries (the flush
  # body, the evidence bundle, every control-socket message). Not the gem's
  # release VERSION above: it changes only for a breaking wire change, and
  # ingest tells formats apart by it. It must equal `current` in
  # testdata/protocol_vectors.json (docs/SERVER_SDK_SPEC.md, "Protocol
  # version"); send it, never a literal.
  PROTOCOL_VERSION = 1
end
