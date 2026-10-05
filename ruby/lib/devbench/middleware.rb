# frozen_string_literal: true

require_relative 'trace'
require_relative 'current'
require_relative 'reporter'

module Devbench
  # Rack middleware: accepts the correlation id and makes it available for the
  # duration of the request, and reports exceptions that escape the app.
  #
  # Never rejects a request, never alters the response body, and never raises
  # anything of its own. An exception from the application is reported and
  # then re-raised — the same object, backtrace intact — so whatever sits
  # outside this middleware sees exactly what it would have without it. A
  # diagnostics middleware that can fail a request is worse than no
  # diagnostics.
  #
  #   config.middleware.insert_before 0, Devbench::Middleware
  class Middleware
    HANDLED_HEADER = 'x-adt-handled'
    EXPOSE_HEADER = 'Access-Control-Expose-Headers'

    def initialize(app)
      @app = app
    end

    def call(env)
      # DEVBENCH_ENABLED=false: not even the trace scope or the header.
      return @app.call(env) unless Devbench.enabled?

      trace = begin
        Trace.parse(env[Trace::RACK_HEADER])
      rescue StandardError
        nil
      end

      # Scoped even without a trace: identity set during this request must
      # end with it, and a thread's next request must not inherit it.
      status, headers, body = Current.with(trace) do
        result = begin
          @app.call(env)
        rescue Exception => e # rubocop:disable Lint/RescueException
          # Reported inside the scope so the report carries this request's
          # trace and user. capture never raises; if it somehow did, the
          # application's exception would be lost, which is the one outcome
          # this must not have.
          report(e, env)
          raise e
        end

        # Rails' ShowExceptions turns an exception into a 500 page itself, so
        # nothing escapes to here. It leaves the exception in env.
        report(env['action_dispatch.exception'], env)

        trace.nil? ? result : mark_handled(result)
      end

      [status, headers, body]
    end

    private

    def report(error, env)
      return if error.nil?

      Reporter.capture(error, context: 'request', handled: false, symbol: symbol(env))
    rescue Exception # rubocop:disable Lint/RescueException
      nil
    end

    # Controller#action, when Rails has said which. The controller instance
    # knows its own class name exactly (acronym inflections included); the
    # path parameters are the fallback when the error happened before it ran.
    def symbol(env)
      controller = env['action_controller.instance']
      if controller.respond_to?(:action_name) && controller.action_name
        return "#{controller.class.name}##{controller.action_name}"
      end

      params = env['action_dispatch.request.path_parameters']
      return nil unless params.is_a?(Hash)

      name = params[:controller] || params['controller']
      action = params[:action] || params['action']
      return nil if name.to_s.empty? || action.to_s.empty?

      "#{camelize(name.to_s)}Controller##{action}"
    rescue StandardError, SystemStackError
      nil
    end

    # 'admin/deal_notes' -> 'Admin::DealNotes'. Plain, without
    # ActiveSupport's inflections, which this gem cannot assume are loaded.
    def camelize(path)
      path.split('/').map { |part| part.split('_').map(&:capitalize).join }.join('::')
    end

    # Tells the browser that something was handled during this request.
    #
    # This is the whole of detector 2's server half. The browser already knows
    # whether the application showed the user an error; it does not know
    # whether anything went wrong. One header closes that gap, and the join
    # happens where both facts are present at the same moment — rather than in
    # a database, which would need per-trace records from both sides and
    # per-event traffic from the client.
    #
    # The value is a count and nothing else. No class name, no message, no
    # symbol: this header is readable by any script on the page, and the
    # detail already travels to the sidecar over a unix socket on the
    # customer's own host.
    #
    # CROSS-ORIGIN: a browser cannot read a custom response header unless the
    # server lists it in Access-Control-Expose-Headers. An Angular app on a
    # different origin from its Rails API — a common setup — will see
    # nothing without that. See docs/DETECTORS.md.
    def mark_handled(result)
      count = Current.handled_count
      return result if count.zero?

      status, headers, body = result
      headers = headers.dup
      headers[HANDLED_HEADER] = count.to_s

      exposed = headers[EXPOSE_HEADER].to_s
      unless exposed.downcase.include?(HANDLED_HEADER)
        headers[EXPOSE_HEADER] = exposed.empty? ? HANDLED_HEADER : "#{exposed}, #{HANDLED_HEADER}"
      end

      [status, headers, body]
    rescue StandardError
      # Never fail a response over instrumentation.
      result
    end
  end
end
