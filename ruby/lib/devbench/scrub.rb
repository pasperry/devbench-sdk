# frozen_string_literal: true

require_relative 'fingerprint'

module Devbench
  # A port of the sidecar's strict egress (internal/scrub and
  # Sidecar.forEgress): what direct mode does to an exemplar before it leaves
  # the process, so leaving out the sidecar does not loosen redaction
  # (DECISIONS #160). Held to testdata/redaction_vectors.json
  # (test/redaction_vectors_test.rb).
  #
  # Strict mode is the only mode here: template (values the format marks:
  # quoted strings, numbers, durations), then scrub (values with a shape:
  # emails, cards, tokens, credentials, IPs), then the prose heuristic
  # (runs of capitalised words that are not framework vocabulary).
  #
  # Patterns are Go's, with Go's \b and \s (see Fingerprint).
  module Scrub
    WS = Fingerprint::GO_WS
    NOT_WS = Fingerprint::GO_NOT_WS

    def self.go_re(source, options = 0)
      Regexp.new(source.gsub('\b', Fingerprint::GO_B), options)
    end
    private_class_method :go_re

    # Ordered most-specific first, and the order is load-bearing: see the
    # comments on internal/scrub's rules (a credential containing an email,
    # a phone pattern eating an SSN).
    RULES = [
      [/-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?-----END [A-Z ]*PRIVATE KEY-----/, '<redacted:private-key>'],
      [go_re('\beyJ[A-Za-z0-9_\-]{4,}\.[A-Za-z0-9_\-]{4,}\.[A-Za-z0-9_\-]{4,}\b'), '<redacted:jwt>'],
      [go_re('([a-zA-Z][a-zA-Z0-9+.\-]*://)[^\t\n\f\r :/@]+:[^\t\n\f\r /@]+@'), '\1<redacted:credentials>@'],
      [go_re("\\b(authorization|proxy-authorization)(#{WS}*[:=]#{WS}*)(?:#{NOT_WS}+[ \\t]+)?#{NOT_WS}+",
             Regexp::IGNORECASE), '\1\2<redacted:authorization>'],
      [go_re("\\b(bearer|basic)#{WS}+[A-Za-z0-9._\\-=/+]{8,}", Regexp::IGNORECASE), '\1 <redacted:token>'],
      [go_re('\bAKIA[0-9A-Z]{16}\b'), '<redacted:aws-key>'],
      [go_re('\b(password|passwd|pwd|secret|api[_\-]?key|access[_\-]?token|refresh[_\-]?token|' \
             'client[_\-]?secret|private[_\-]?key|session[_\-]?id|csrf[_\-]?token)' \
             "(#{WS}*[:=]#{WS}*)(\"[^\"]*\"|'[^']*'|#{NOT_WS}+)", Regexp::IGNORECASE), '\1\2<redacted:secret>'],
      [go_re('\b[^\t\n\f\r <>@]+@[^\t\n\f\r <>@]+\.[A-Za-z]{2,}\b'), '<redacted:email>'],
      [go_re('\b\d{3}-\d{2}-\d{4}\b'), '<redacted:ssn>'],
      [go_re('\b(?:\d[ \-]*?){13,19}\b'), '<redacted:card>'],
      [go_re('\b\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}\b'), '<redacted:ip>'],
      [/\+?\d[\d\t\n\f\r ().\-]{7,}\d/, '<redacted:phone>']
    ].freeze

    # internal/sidecar/trace.go's traceRe.
    TRACE = go_re('\bv1/([A-Za-z0-9_-]{1,64})/([A-Za-z0-9_-]{1,64})/(\d{1,3})\b')

    # What Rails, Go and HTTP say, lowercased (internal/scrub/prose.go).
    # Masking any of these would destroy diagnostic content.
    VOCABULARY = %w[
      ok created accepted content moved permanently found modified bad request
      unauthorized payment required forbidden not method allowed acceptable
      timeout conflict gone unsupported media type unprocessable entity too many
      requests internal server error implemented gateway service unavailable no
      see other temporary redirect
      started completed processing rendered rendering redirected filter chain
      halted parameters views load exists destroy update create transaction
      rollback commit cache performed performing enqueued retrying rescued
      validation failed invalid email has already been taken
      panic goroutine runtime fatal context deadline exceeded canceled cancelled
      connection refused such host timeout_ closed reset peer broken pipe
      temporarily
      warning warn info debug trace exception failure retry skipping unknown
      missing expired denied true false nil null none
    ].to_h { |w| [w, true] }.freeze

    FIELD = /[^#{Fingerprint::GO_SPACE[1..-2]}]+/o
    TRAILING_PUNCT = /[.,;:!?)"']+\z/
    CODE_CHARS = /[:#\/()_=<>@\[\]{}"']/

    class << self
      # internal/scrub.Text: every shape rule, in order.
      def text(line)
        RULES.reduce(utf8(line)) { |out, (re, with)| out.gsub(re, with) }
      end

      # internal/scrub.Prose: runs of two or more capitalised words that are
      # not framework vocabulary become <redacted:name>.
      def prose(line)
        return line if line.empty?

        fields = line.scan(FIELD)
        return line if fields.length < 2

        out = []
        run = []
        flush = lambda do
          if run.length >= 2
            out << '<redacted:name>'
          elsif run.length == 1
            out << run.first
          end
          run.clear
        end

        fields.each do |field|
          if proper_noun?(field)
            run << field
          else
            flush.call
            out << field
          end
        end
        flush.call
        out.join(' ')
      end

      # The sidecar's forEgress in strict mode, for one line: any trace kept
      # as is, the rest templated, scrubbed and prose-masked.
      def egress(line)
        line = utf8(line)
        match = TRACE.match(line)
        body = match ? line.gsub(TRACE, '<trace>') : line
        redacted = strict(body)
        match ? "[#{match[0]}] #{redacted}" : redacted
      end

      # A bundle's template text: as the sidecar's answerEvidenceRequests
      # does, strict redaction and then a second shape pass.
      def template_text(text)
        self.text(strict(utf8(text)))
      end

      private

      def strict(body)
        prose(text(Fingerprint.template(body)))
      end

      def proper_noun?(token)
        trimmed = token.sub(TRAILING_PUNCT, '')
        return false if trimmed.bytesize < 2
        return false if CODE_CHARS.match?(trimmed)
        return false if trimmed == trimmed.upcase

        first = trimmed.getbyte(0)
        return false if first < 65 || first > 90 # 'A'..'Z'
        return false if trimmed.match?(/[0-9]/)

        !VOCABULARY.key?(trimmed.downcase)
      end

      def utf8(text)
        text = text.to_s
        return text if text.encoding == Encoding::UTF_8 && text.valid_encoding?

        text.encode('UTF-8', invalid: :replace, undef: :replace).scrub
      end
    end
  end
end
