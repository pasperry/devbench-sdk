#!/usr/bin/env bash
# Which SDK does a release tag publish, and does that SDK's version agree?
#
#   .github/scripts/release-tag.sh <tag>     (from the repo root)
#
# Prints `sdk=<name>` and `version=<X.Y.Z>`, and appends them to
# $GITHUB_OUTPUT when it is set. Exits non-zero, publishing nothing, for a
# tag that names no SDK or whose version differs from the SDK's own.
#
# One SDK per tag (docs/specs/sdk-python-php-dotnet.md, "Versioning"):
#   ruby-vX.Y.Z     -> RubyGems devbench
#   browser-vX.Y.Z  -> npm devbench
#   go-vX.Y.Z       -> the module tags go/vX.Y.Z and go/gin/vX.Y.Z
#   python-vX.Y.Z   -> PyPI devbench
# Go is triggered by go-v*, not by its module tag: a go/v* tag is public the
# moment it is pushed, so it is only created by the approved release job.
# A new SDK adds a line to each case below and a job to release.yml.
set -euo pipefail

fail() { echo "::error::$*" >&2; exit 1; }

tag="${1:-}"
[ -n "$tag" ] || fail "usage: release-tag.sh <tag>"

case "$tag" in
	ruby-v*) sdk=ruby ;;
	browser-v*) sdk=browser ;;
	go-v*) sdk=go ;;
	python-v*) sdk=python ;;
	v[0-9]*) fail "$tag: SDKs release one at a time now; tag ruby-v${tag#v}, browser-v${tag#v}, go-v${tag#v} or python-v${tag#v} instead" ;;
	*) fail "$tag: not a release tag (ruby-vX.Y.Z, browser-vX.Y.Z, go-vX.Y.Z or python-vX.Y.Z)" ;;
esac

version="${tag#"$sdk"-v}"
# Plain X.Y.Z only: a prerelease would go out as npm's `latest`.
[[ "$version" =~ ^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]] ||
	fail "$tag: version '$version' is not X.Y.Z"

case "$sdk" in
	ruby) declared="$(sed -n "s/^ *VERSION = '\([^']*\)'.*/\1/p" ruby/lib/devbench/version.rb)" ;;
	browser) declared="$(node -p "require('./browser/package.json').version")" ;;
	go) declared="$(sed -n 's/^const Version = "\([^"]*\)"$/\1/p' go/version.go)" ;;
	# The wheel's version comes from this line (pyproject: attr devbench.version.VERSION).
	python) declared="$(sed -n 's/^VERSION = "\([^"]*\)"$/\1/p' python/devbench/version.py)" ;;
esac

[ "$declared" = "$version" ] ||
	fail "$tag: the $sdk SDK declares version '${declared}', not $version; bump it (and merge) before tagging"

echo "sdk=$sdk"
echo "version=$version"
if [ -n "${GITHUB_OUTPUT:-}" ]; then
	{ echo "sdk=$sdk"; echo "version=$version"; } >> "$GITHUB_OUTPUT"
fi
