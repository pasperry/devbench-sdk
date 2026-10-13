"""The SDK's release version and the protocol version it speaks."""

# This package's own release (python-v<VERSION>). The SDKs release
# independently (docs/specs/sdk-python-php-dotnet.md, "Versioning").
VERSION = "0.2.0"

# This SDK's name in every flush's `sdk` field: the key
# testdata/protocol_vectors.json `sdk.names` gives sdk/server/python
# (docs/server-sdk/wire-protocol.md, "the SDK's name and version"). The
# field's version is VERSION, never a second literal.
SDK_NAME = "python"

# The wire format this speaks: the `v` every payload carries (the flush body
# and the evidence bundle). Not the release VERSION above: it changes only for
# a breaking wire change, and ingest tells formats apart by it. It must equal
# `current` in testdata/protocol_vectors.json (docs/server-sdk/protocol-version.md,
# "Protocol version"); send it, never a literal.
PROTOCOL_VERSION = 1
