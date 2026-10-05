// A separate module on purpose.
//
// This is imported into a customer's application. Making it part of the
// mothership module would drag pgx, the AWS SDK, and everything else into
// their build for the sake of a few hundred lines. It has no dependencies
// outside the standard library and is intended to keep it that way.
module github.com/pasperry/devbench-sdk/go

go 1.22
