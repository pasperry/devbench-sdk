# frozen_string_literal: true

require_relative 'current'
require_relative 'trace'

module Devbench
  # Forwarding helpers.
  #
  # This is the capability people forget. Accepting a header is easy and
  # obviously correct; forwarding it on the way out is the part that gets
  # missed, and when it is missing the failure is silent — the call succeeds,
  # and downstream logs simply cannot be joined to the browser that caused
  # them. See ARCHITECTURE_PROPOSAL.md risk #9.
  module HTTP
    # Returns headers to merge into an outbound request, or {}.
    #
    #   Net::HTTP.post(uri, body, **Devbench::HTTP.headers)
    #   Faraday.get(url, nil, Devbench::HTTP.headers)
    def self.headers
      trace = Current.trace
      return {} if trace.nil?

      forwarded = trace.next_hop
      return {} if forwarded.nil?

      { Trace::HEADER => forwarded.to_s }
    end

    # Merges the trace header into an existing hash, without mutating it.
    def self.merge(existing)
      (existing || {}).merge(headers)
    end
  end
end
