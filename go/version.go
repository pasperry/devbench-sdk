package devbench

// Version of this SDK. Each SDK has its own version and release tag; the Go
// release (tag go-vX.Y.Z in the public repo) checks this equals the tag and
// creates the module tags go/vX.Y.Z and go/gin/vX.Y.Z.
const Version = "0.8.1"

// Name is this SDK's name in every flush's `sdk` field: the key
// testdata/protocol_vectors.json `sdk.names` gives sdk/server/go
// (docs/server-sdk/wire-protocol.md, "the SDK's name and version"). The
// field's version is Version, never a second literal.
const Name = "go"

// ProtocolVersion is the wire format this SDK speaks: the `v` every payload
// carries (the flush body, the evidence bundle, every control-socket
// message). It is not the release Version above: it changes only for a
// breaking wire change, and ingest tells formats apart by it. It must equal
// `current` in testdata/protocol_vectors.json (docs/server-sdk/protocol-version.md,
// "Protocol version"); send it, never a literal.
const ProtocolVersion = 1
