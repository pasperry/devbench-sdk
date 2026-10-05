# frozen_string_literal: true

require 'digest'

module Devbench
  # A port of internal/fingerprint (Go): the same signal gives the same
  # fingerprint, byte for byte, as the sidecar and the browser compute.
  #
  # If two implementations disagree, the same problem is counted as two:
  # triage cost doubles and one issue is split in half, and a customer moving
  # between direct and sidecar mode would fork every issue they have.
  # testdata/fingerprint_vectors.json holds all of them to one answer
  # (test/fingerprint_vectors_test.rb).
  #
  # Go's regexp and Ruby's differ in ways that matter here, so the patterns
  # are spelled out rather than copied:
  #   * Go's \b is an ASCII word boundary; Ruby's treats "é" as a word
  #     character. GO_B rebuilds Go's.
  #   * Go's \s is [\t\n\f\r ]; Ruby's adds \v. GO_WS is Go's.
  #   * Go's strings.TrimSpace trims Unicode spaces; String#strip does not.
  module Fingerprint
    GO_W = '[0-9A-Za-z_]'
    GO_B = "(?:(?<=#{GO_W})(?!#{GO_W})|(?<!#{GO_W})(?=#{GO_W}))".freeze
    GO_WS = '[\t\n\f\r ]'
    GO_NOT_WS = '[^\t\n\f\r ]'
    # unicode.IsSpace, which strings.TrimSpace and strings.Fields use.
    GO_SPACE = "[\t\n\v\f\r \u0085\u00A0\u1680\u2000-\u200A\u2028\u2029\u202F\u205F\u3000]"
    TRIM = /\A#{GO_SPACE}+|#{GO_SPACE}+\z/o

    def self.go_re(source, options = 0)
      Regexp.new(source.gsub('\b', GO_B), options)
    end
    private_class_method :go_re

    # Ordered as in Go: more specific patterns first, or a UUID becomes a
    # string of <num> fragments. (Go skips rules a cheap scan proves cannot
    # match; that changes no output, so it is not ported.)
    TEMPLATE_RULES = [
      [go_re('\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b', Regexp::IGNORECASE), '<uuid>'],
      [go_re('\b\d{4}-\d{2}-\d{2}[T ]\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:?\d{2})?'), '<ts>'],
      [go_re('\b\d{4}-\d{2}-\d{2}\b'), '<date>'],
      [go_re('\b[^\t\n\f\r <>@]+@[^\t\n\f\r <>@]+\.[A-Za-z]{2,}\b'), '<email>'],
      [go_re("\\b[a-z][a-z0-9+.-]*://#{GO_NOT_WS}+"), '<url>'],
      [go_re('\b\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}\b'), '<ip>'],
      [go_re('\b(?:0x)?[0-9a-fA-F]{8,}\b'), '<hex>'],
      [/"[^"]*"/, '<str>'],
      [/'[^']*'/, '<str>'],
      [go_re("\\b\\d+(?:\\.\\d+)?#{GO_WS}?(?:ms|s|us|ns|µs)\\b"), '<dur>'],
      [go_re('\b\d+(?:\.\d+)?\b'), '<num>']
    ].freeze

    WHITESPACE = /#{GO_WS}+/o
    MAX_FRAMES = 5
    KEEP_SEGMENTS = 3

    DEPENDENCY_FRAGMENTS = [
      '/node_modules/', '/vendor/', '/gems/', '/ruby/gems/', '/usr/local/go/src/',
      '/go/pkg/mod/', '/.bundle/', '<anonymous>'
    ].freeze

    ASSET_DIGEST = /\A(.+)[-.]([A-Za-z0-9_]{8,})((?:\.chunk|\.bundle)?\.(?:js|mjs|cjs|css)(?:\.map)?)\z/

    class << self
      # The fingerprint (64 hex chars) of a signal, or nil when the signal
      # has no type, message or frames (Go returns an error there).
      #
      # frames: [{function:, file:, line:}] (string or symbol keys).
      def compute(kind:, source:, service: '', type: '', message: '', frames: [])
        return nil if kind.to_s.empty? || source.to_s.empty?
        return nil if type.to_s.empty? && message.to_s.empty? && (frames.nil? || frames.empty?)

        Digest::SHA256.hexdigest(canonical(normalize(kind, source, service, type, message, frames)))
      end

      # The canonical form, field by field, as Go's Normalized.
      def normalize(kind, source, service, type, message, frames)
        {
          kind: kind.to_s, source: source.to_s, service: service.to_s,
          type: trim(type.to_s), template: template(message.to_s),
          frames: normalize_frames(frames || [])
        }
      end

      # Reduces a message to its shape, replacing values that vary between
      # occurrences with holes.
      def template(message)
        out = trim(utf8(message))
        return '' if out.empty?

        TEMPLATE_RULES.each { |re, with| out = out.gsub(re, with) }
        trim(out.gsub(WHITESPACE, ' '))
      end

      def normalize_path(path)
        p = trim(path.to_s)
        return '' if p.empty?

        if (i = p.index('://'))
          rest = p[(i + 3)..]
          j = rest.index('/')
          p = j ? rest[j..] : rest
        end

        if (i = p.index(/[?#]/))
          p = p[0, i]
        end

        p = p.tr('\\', '/')
        segments = p.gsub(%r{\A/+|/+\z}, '').split('/', -1)
        segments = [''] if segments.empty?
        segments = segments.last(KEEP_SEGMENTS)
        segments[-1] = strip_asset_digest(segments[-1])
        segments.join('/')
      end

      def trim(text)
        text.gsub(TRIM, '')
      end

      private

      def normalize_frames(frames)
        out = []
        frames.each do |f|
          function = field(f, :function)
          file = field(f, :file)
          next if dependency?(file, function)

          fn = trim(function)
          path = normalize_path(file)
          next if fn.empty? && path.empty?

          # Deliberately no line number.
          out << "#{fn}@#{path}"
          break if out.length == MAX_FRAMES
        end
        out
      end

      def field(frame, name)
        return '' unless frame.is_a?(Hash)

        utf8((frame[name] || frame[name.to_s]).to_s)
      end

      def dependency?(file, function)
        hay = "#{file} #{function}"
        DEPENDENCY_FRAGMENTS.any? { |frag| hay.include?(frag) }
      end

      def strip_asset_digest(name)
        m = ASSET_DIGEST.match(name)
        return name if m.nil? || !digest?(m[2])

        m[1] + m[3]
      end

      def digest?(token)
        digit = token.match?(/[0-9]/)
        upper = token.match?(/[A-Z]/)
        hex_only = token.match?(/\A[0-9a-f]*\z/)
        return true if hex_only && digit && token.length >= 8

        token.length == 8 && digit && upper
      end

      def canonical(norm)
        ["v1", norm[:kind], norm[:source], norm[:service], norm[:type], norm[:template],
         norm[:frames].join("\x1f")].join("\x1e")
      end

      # Go treats invalid UTF-8 as U+FFFD; so does this, rather than raising.
      def utf8(text)
        return text if text.encoding == Encoding::UTF_8 && text.valid_encoding?

        text.encode('UTF-8', invalid: :replace, undef: :replace).scrub
      end
    end
  end
end
