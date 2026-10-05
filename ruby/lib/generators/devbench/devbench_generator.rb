# frozen_string_literal: true

require 'rails/generators'
require 'devbench'

module Devbench
  # bin/rails generate devbench
  #
  # Step 2 of the install (DECISIONS #161):
  #
  #   1. bundle add devbench
  #   2. bin/rails generate devbench
  #   3. set DEVBENCH_DSN
  #
  # Does two things, and touches nothing else:
  #
  #   * puts <%= devbench_script_tag %> in the application layout's <head>
  #     (.erb, .haml or .slim), or prints exactly what to add where;
  #   * if config/initializers/content_security_policy.rb defines a policy,
  #     allows the sensor's script (https://unpkg.com) in script_src, and
  #     the ingest host plus evidence storage (https://*.storage.supabase.co)
  #     in connect_src.
  #
  # Idempotent: a second run changes nothing. Prints what it changed.
  class DevbenchGenerator < ::Rails::Generators::Base
    namespace 'devbench'
    desc 'Adds the Dev Bench browser tag to your layout and allows it in your CSP.'

    TAG = 'devbench_script_tag'
    SCRIPT_HOST = 'https://unpkg.com'
    STORAGE_HOST = 'https://*.storage.supabase.co'
    # Ruby code, not a host: evaluated when the CSP initializer runs, so the
    # policy follows DEVBENCH_DSN (Devbench.csp_connect_sources).
    RUNTIME_CONNECT = '*Devbench.csp_connect_sources'
    LAYOUTS = %w[erb haml slim].map { |ext| "app/views/layouts/application.html.#{ext}" }.freeze
    CSP = 'config/initializers/content_security_policy.rb'

    def add_script_tag
      path = LAYOUTS.find { |p| File.exist?(File.join(destination_root, p)) }
      return manual_tag('no app/views/layouts/application.html.{erb,haml,slim} found') if path.nil?

      full = File.join(destination_root, path)
      text = File.read(full)
      if text.include?(TAG)
        say_status :identical, "#{path} (already has #{TAG})", :blue
        return
      end

      updated = path.end_with?('.erb') ? insert_erb(text) : insert_indented(text, path.end_with?('.haml') ? /%head\b/ : /head\b/)
      return manual_tag("could not find the <head> of #{path}") if updated.nil?

      File.write(full, updated)
      say_status :insert, "#{path}: <%= #{TAG} %> in <head>", :green
    end

    def allow_in_content_security_policy
      full = File.join(destination_root, CSP)
      unless File.exist?(full)
        say_status :skip, "#{CSP} not found: no CSP to change", :blue
        return
      end

      text = File.read(full)
      var = text[/^[ \t]*(?:Rails\.application\.)?config\.content_security_policy[ \t]+do[ \t]*\|[ \t]*(\w+)[ \t]*\|/, 1]
      if var.nil?
        say_status :skip, "#{CSP} defines no policy (all commented out): nothing to change", :blue
        return
      end

      changes = []
      text = allow(text, var, 'script_src', [SCRIPT_HOST], changes)
      text = allow(text, var, 'connect_src', [RUNTIME_CONNECT], changes)
      if changes.empty?
        say_status :identical, "#{CSP} (already allows Dev Bench)", :blue
        return
      end

      File.write(full, text)
      changes.each { |c| say_status :csp, "#{CSP}: #{c}", :green }
    end

    def print_next_steps
      say ''
      say 'Next:'
      say '  1. Set DEVBENCH_DSN in this app\'s environment (the value `adt dsn create` printed).'
      say '  2. Check it: bin/rails devbench:test'
    end

    private

    def manual_tag(reason)
      say_status :manual, reason, :yellow
      say "  Add this to your layout, just before </head>:\n\n    <%= #{TAG} %>\n\n" \
          "  (Haml or Slim: `= #{TAG}` as the last line inside head.)"
    end

    # Before </head>, indented one step deeper than it when it is on a line
    # of its own.
    def insert_erb(text)
      at = text =~ %r{</head\s*>}i
      return nil if at.nil?

      line_start = text.rindex("\n", at - 1)
      line_start = line_start.nil? ? 0 : line_start + 1
      before = text[line_start...at]
      if before.strip.empty?
        "#{text[0...line_start]}#{before}  <%= #{TAG} %>\n#{text[line_start..]}"
      else
        "#{text[0...at]}<%= #{TAG} %>#{text[at..]}"
      end
    end

    # Haml / Slim: `= devbench_script_tag` as the last child of the head
    # element, at its children's indentation.
    def insert_indented(text, head)
      lines = text.lines
      index = lines.index { |l| l =~ /\A([ \t]*)#{head}/ }
      return nil if index.nil?

      head_indent = lines[index][/\A[ \t]*/]
      last = index
      child_indent = nil
      (index + 1...lines.length).each do |i|
        line = lines[i]
        next if line.strip.empty?

        indent = line[/\A[ \t]*/]
        break if indent.length <= head_indent.length

        child_indent ||= indent
        last = i
      end
      child_indent ||= "#{head_indent}  "
      lines[last] = "#{lines[last]}\n" unless lines[last].end_with?("\n")
      lines.insert(last + 1, "#{child_indent}= #{TAG}\n")
      lines.join
    end

    # Adds each host to `var.directive`, or — when the directive is absent
    # but default_src is set — adds the directive as default_src's sources
    # plus the hosts, which is what the browser was falling back to. Leaves
    # a policy that restricts neither alone.
    def allow(text, var, directive, hosts, changes)
      lines = text.lines
      range = statement(lines, var, directive)
      if range
        hosts.each do |host|
          stmt = lines[range].join
          next if stmt.include?(host)

          lines[range.last] = append_source(lines[range.last], host)
          changes << "added #{host} to #{directive}"
        end
        return lines.join
      end

      # Neither: the policy does not restrict this kind of request at all.
      default = statement(lines, var, 'default_src')
      return text if default.nil?

      first = lines[default.first]
      indent = first[/\A[ \t]*/]
      sources = lines[default].map { |l| strip_comment(l.chomp) }.join(' ')
                              .sub(/\A[ \t]*#{var}\.default_src/, '').gsub(/\s+/, ' ').strip
      new_line = "#{indent}#{var}.#{directive} #{[sources, *hosts.map { |h| source_code(h) }].reject(&:empty?).join(', ')}\n"
      lines.insert(default.last + 1, new_line)
      changes << "added #{directive} (default_src's sources + #{hosts.join(', ')})"
      lines.join
    end

    # The line range of an uncommented `var.directive ...` statement,
    # following continuation lines (a trailing comma or backslash).
    def statement(lines, var, directive)
      start = lines.index { |l| l =~ /\A[ \t]*#{Regexp.escape(var)}\.#{directive}\b/ }
      return nil if start.nil?

      last = start
      last += 1 while last + 1 < lines.length && strip_comment(lines[last]).rstrip.end_with?(',', '\\')
      start..last
    end

    def append_source(line, host)
      body = line.chomp
      newline = line.end_with?("\n") ? "\n" : ''
      code = strip_comment(body).rstrip
      comment = body[code.length..]
      "#{code}, #{source_code(host)}#{comment}#{newline}"
    end

    # A trailing `# comment`, not counting '#' inside a string or #{}.
    def strip_comment(line)
      quote = nil
      line.each_char.with_index do |ch, i|
        if quote
          quote = nil if ch == quote && line[i - 1] != '\\'
        elsif ch == '"' || ch == "'"
          quote = ch
        elsif ch == '#' && line[i + 1] != '{'
          return line[0...i]
        end
      end
      line
    end

    # A host is written as a string literal; a runtime splat as code.
    def source_code(source)
      source.start_with?('*') ? source : source.inspect
    end
  end
end
