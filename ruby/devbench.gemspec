# frozen_string_literal: true

require_relative 'lib/devbench/version'

Gem::Specification.new do |spec|
  spec.name     = 'devbench'
  spec.version  = Devbench::VERSION
  spec.authors  = ['Dev Bench']
  spec.summary  = 'Exceptions, handled failures and who they hit, from a Rails app to Dev Bench.'
  spec.description = <<~TEXT
    Reports unhandled exceptions, Rails.error reports, failed ActiveJob and
    Sidekiq jobs, and failures the application handled, with who was
    affected, to Dev Bench. Fingerprints and counts in-process and sends one
    small request a minute; detail is uploaded only when Dev Bench asks for
    it, scrubbed first. Set DEVBENCH_DSN and it hooks itself into Rails.

    Adds no dependencies beyond the standard library, never alters a response
    body, and never raises: a diagnostics gem that can fail a request is worse
    than no diagnostics.
  TEXT
  spec.homepage = 'https://github.com/pasperry/devbench-sdk'
  spec.license  = 'LicenseRef-Proprietary'

  spec.required_ruby_version = '>= 3.0'

  # Deliberately no runtime dependencies. This loads inside a customer's
  # application, and every dependency is a version conflict we could hand
  # them.
  spec.files = Dir['lib/**/*.rb'] + ['README.md']
  spec.require_paths = ['lib']

  spec.metadata = {
    'rubygems_mfa_required' => 'true',
    'source_code_uri'       => 'https://github.com/pasperry/devbench-sdk',
  }
end
