# frozen_string_literal: true

# Dev Bench server SDK for Ruby/Rails (formerly "ADT"; DECISIONS #160).
#
#   bundle add devbench
#   bin/rails generate devbench
#   DEVBENCH_DSN=https://<public>:<secret>@<ingest host>   (from `adt dsn create`)
#
# What it does:
#   * accepts and forwards the correlation id  (Devbench::Middleware, Devbench::HTTP)
#   * reports handled failures                 (Devbench.report_handled)
#   * records who was affected                 (Devbench.set_user)
#   * reports unhandled exceptions             (Devbench::Middleware, Devbench::Railtie,
#                                               Devbench::SidekiqHooks,
#                                               Devbench.capture_exception)
#   * keeps recent log lines per trace         (Devbench::Logs; direct mode)
#   * renders the browser sensor's tag         (devbench_script_tag)
#
# The old name is kept: `require 'adt'` and every `ADT.` / `ADT::` call in an
# existing install resolve to this module.
require_relative 'devbench/version'
require_relative 'devbench/trace'
require_relative 'devbench/current'
require_relative 'devbench/backtrace'
require_relative 'devbench/identity'
require_relative 'devbench/middleware'
require_relative 'devbench/http'
require_relative 'devbench/reporter'
require_relative 'devbench/session'
require_relative 'devbench/session_middleware'
require_relative 'devbench/rails_hooks'
require_relative 'devbench/sidekiq_hooks'
require_relative 'devbench/logs'
require_relative 'devbench/view_helper'

# The pre-0.5 name. The same module object, not a copy: ADT::Middleware *is*
# Devbench::Middleware, so a stack holding either holds the one class.
ADT = Devbench unless defined?(::ADT)

# Only under Rails. Bundler requires gems after `require 'rails'` in
# config/application.rb, so Rails::Railtie is defined by the time this runs.
require_relative 'devbench/railtie' if defined?(::Rails::Railtie)
