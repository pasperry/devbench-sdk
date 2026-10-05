# frozen_string_literal: true

module Devbench
  # Turns Ruby backtrace lines into the control socket's frames:
  # [{function:, file:, line:}], innermost first.
  #
  # Ruby has changed this format under us before. Up to 3.3:
  #
  #   /app/models/deal.rb:42:in `block in save'
  #
  # From 3.4, a straight quote, and the label carries the owner:
  #
  #   /app/models/deal.rb:42:in 'block in Deal#save'
  #
  # Both parse to the same file and line; the function is whatever Ruby said.
  module Backtrace
    MAX_FRAMES = 50
    # Bounds one frame so 50 of them cannot push a message past the socket's
    # 256 KiB line limit.
    MAX_FIELD = 1024
    LINE = /\A(.+?):(\d+)(?::in [`'](.*)')?\z/m

    def self.frames(backtrace, root)
      return [] unless backtrace.is_a?(Array)

      prefix = root_prefix(root)
      backtrace.first(MAX_FRAMES).map { |raw| frame(utf8(raw.to_s), prefix) }
    end

    def self.frame(raw, prefix)
      match = LINE.match(raw)
      # Unparseable lines are kept whole rather than dropped: a frame the
      # parser did not understand is still evidence.
      return { function: '', file: clip(raw), line: 0 } if match.nil?

      file = match[1]
      file = file[prefix.length..] if prefix && file.start_with?(prefix)

      { function: clip(match[3].to_s), file: clip(file), line: match[2].to_i }
    end

    def self.root_prefix(root)
      root = root.to_s
      return nil if root.empty?

      root.end_with?('/') ? root : "#{root}/"
    end

    def self.clip(text)
      text.length > MAX_FIELD ? text[0, MAX_FIELD] : text
    end

    # JSON.generate raises on invalid UTF-8, which would lose the whole report
    # over one byte in a path.
    def self.utf8(text)
      text.encode('UTF-8', invalid: :replace, undef: :replace).scrub
    end

    private_class_method :frame, :root_prefix, :clip, :utf8
  end
end
