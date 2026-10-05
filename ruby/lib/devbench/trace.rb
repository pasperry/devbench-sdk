# frozen_string_literal: true

module Devbench
  # The correlation id that ties a user action together across every service.
  #
  # Wire form (docs/SERVER_SDK_SPEC.md): v1/<session>/<intent>/<hop>
  #
  # This must agree exactly with the Go SDK and the sidecar's extraction regex.
  # A trace the browser writes and Rails mangles is worse than no trace: the
  # request still succeeds, so nothing looks wrong, and the evidence simply
  # cannot be joined.
  class Trace
    HEADER = 'x-adt-trace'
    RACK_HEADER = 'HTTP_X_ADT_TRACE'
    MAX_HOP = 99
    ID = /\A[A-Za-z0-9_-]{1,64}\z/

    attr_reader :session, :intent, :hop

    def initialize(session, intent, hop)
      @session = session
      @intent = intent
      @hop = hop
    end

    # Parses a header value. Returns nil rather than raising: ADT must never
    # fail a customer's request, and a request carrying a mangled correlation
    # id is still a request the user wants served.
    def self.parse(value)
      return nil unless value.is_a?(String)

      value = value.strip
      return nil unless value.start_with?('v1/')

      parts = value[3..].split('/', -1)
      return nil unless parts.length == 3

      session, intent, hop = parts
      return nil unless session =~ ID && intent =~ ID
      return nil unless hop =~ /\A\d{1,3}\z/

      hop = hop.to_i
      return nil if hop.negative? || hop > MAX_HOP

      new(session, intent, hop)
    end

    def to_s
      "v1/#{@session}/#{@intent}/#{@hop}"
    end

    # Identifies the user action, independent of which service saw it.
    def key
      "#{@session}/#{@intent}"
    end

    # The trace to send to a downstream service.
    def next_hop
      return nil if @hop >= MAX_HOP

      Trace.new(@session, @intent, @hop + 1)
    end

    def ==(other)
      other.is_a?(Trace) && other.session == @session && other.intent == @intent && other.hop == @hop
    end
  end

  # A Rails log tag (config.log_tags) carrying the request's x-adt-trace, or
  # nothing when there is none — Rails drops blank tags. The Railtie adds it;
  # apps without Rails::Railtie can add it themselves. Never raises: a log
  # tag that fails takes every log line of the request with it.
  LOG_TAG = lambda do |request|
    raw = request.respond_to?(:get_header) ? request.get_header(Trace::RACK_HEADER) : nil
    Devbench::Trace.parse(raw)&.to_s
  rescue StandardError, SystemStackError
    nil
  end
end
