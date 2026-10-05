# frozen_string_literal: true

require_relative 'fingerprint'

module Devbench
  # Learned redaction rules, as ingest sends them on a flush response
  # (`redaction`), compiled and applied exactly as the sidecar does
  # (internal/sidecar/rules.go and Sidecar.redactBody):
  #
  #   {"fp": <shape fp>, "egress": "masked"|"none"|"full"}   shape rule
  #   {"kind": "field", "target": "license"}                 mask that field's value
  #   {"kind": "term",  "target": "Whitfield"}               mask that literal
  #
  # Immutable once built; the transport swaps a whole new one in when a
  # response carries a rule set, and keeps the current one when a response
  # carries none (absent is not empty: ingest omits the field when it could
  # not look the rules up).
  class EgressPolicy
    FIELD = 'field'
    TERM = 'term'
    MASKED = 'masked'
    NONE = 'none'
    FULL = 'full'

    # store.NormalizeRule's field-name shape; anything else is ignored, so a
    # malformed rule never becomes an odd pattern here.
    FIELD_NAME = /\A[a-z0-9_.\-]{1,64}\z/
    # A shorter term would mask fragments of ordinary words everywhere.
    MIN_TERM_BYTES = 3

    WS = Fingerprint::GO_WS

    attr_reader :shapes, :terms

    # rules: the parsed JSON array. Entries that are not objects, or not
    # understood, are skipped (a newer ingest may send kinds this gem cannot
    # enforce; the local passes still run).
    def self.compile(rules)
      shapes = {}
      fields = []
      terms = []
      Array(rules).each do |rule|
        next unless rule.is_a?(Hash)

        kind = rule['kind'].to_s
        target = rule['target'].is_a?(String) ? rule['target'] : ''
        case kind
        when ''
          fp = rule['fp']
          shapes[fp] = rule['egress'].to_s if fp.is_a?(String) && !fp.empty?
        when FIELD
          name = target.strip.downcase
          fields << name if FIELD_NAME.match?(name) && !fields.include?(name)
        when TERM
          terms << target if target.bytesize >= MIN_TERM_BYTES && !terms.include?(target)
        end
      end
      new(shapes, fields, terms)
    end

    def initialize(shapes, fields, terms)
      @shapes = shapes.freeze
      @terms = terms.freeze
      @fields = fields.empty? ? nil : field_pattern(fields)
      freeze
    end

    # The shape rule for a fingerprint, or nil.
    def rule_for(fp)
      @shapes[fp]
    end

    def shapes?
      !@shapes.empty?
    end

    # Field and term rules. Additive only: it removes text, never restores
    # any.
    def mask(text)
      text = text.gsub(@fields, '\1\2\3\4<redacted:field>') if @fields
      @terms.each { |term| text = text.gsub(term) { '<redacted:term>' } }
      text
    end

    private

    # The field name, exactly — "license" must not catch "licensed" or
    # "driver_license" — optionally quoted or a Ruby symbol, then a
    # separator, then one value: a quoted string or a bare token. Go's
    # pattern, with Go's \s and with \A for Go's unanchored-mode ^.
    def field_pattern(fields)
      names = fields.map { |f| Regexp.escape(f) }.join('|')
      Regexp.new(
        "(\\A|[^a-z0-9_.\\-])([\"':]?)(#{names})" \
        "([\"']?#{WS}*(?:=>|=|:)#{WS}*)" \
        "(\"(?:[^\"\\\\]|\\\\.)*\"|'(?:[^'\\\\]|\\\\.)*'|[^\\t\\n\\f\\r ,;&)}\\]]+)",
        Regexp::IGNORECASE
      )
    end
  end
end
