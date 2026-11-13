# Standalone release control-flow checks. No Rails boot, network, or database.
# Run: bundle exec ruby test/release_check.rb
require "bundler/setup"
require "minitest/autorun"
require "tmpdir"
require "fileutils"
require "open3"

class ReleaseCheck < Minitest::Test
  def with_release(connect_failures:, attempts:)
    app = File.expand_path("..", __dir__)
    FileUtils.mkdir_p("#{app}/tmp")
    Dir.mktmpdir("release-check-", "#{app}/tmp") do |root|
      FileUtils.mkdir_p(["#{root}/bin", "#{root}/config"])
      FileUtils.cp("#{app}/bin/release", "#{root}/bin/release")
      File.write("#{root}/config/environment.rb", <<~RUBY)
        module ActiveRecord
          class ConnectionNotEstablished < StandardError; end
          class NoDatabaseError < StandardError; end
          class StatementInvalid < StandardError; end
          class Base
            def self.connection_pool = self
            def self.with_connection
              @attempt = (@attempt || 0) + 1
              File.open(ENV.fetch("TEST_TRACE"), "a") { |f| f.puts("connect") }
              raise ConnectionNotEstablished if @attempt <= Integer(ENV.fetch("TEST_FAILURES"))
              yield self
            end
            def self.select_value(*) = 1
            def self.disconnect!; end
          end
        end
      RUBY
      File.write("#{root}/bin/rails", <<~RUBY)
        #!/usr/bin/env ruby
        abort "unexpected command" unless ARGV == ["db:migrate"]
        File.open(ENV.fetch("TEST_TRACE"), "a") { |f| f.puts("migrate") }
        exit 17 # migration failed; must NOT be retried
      RUBY
      FileUtils.chmod(0o755, "#{root}/bin/rails")
      env = { "TEST_TRACE" => "#{root}/trace", "TEST_FAILURES" => connect_failures.to_s,
              "RELEASE_DB_ATTEMPTS" => attempts.to_s }
      _stdout, stderr, status = Open3.capture3(env, RbConfig.ruby, "#{root}/bin/release")
      trace = File.exist?("#{root}/trace") ? File.readlines("#{root}/trace", chomp: true) : []
      yield status.exitstatus, trace, stderr
    end
  end

  def test_failed_migration_runs_once_and_preserves_exit_status
    with_release(connect_failures: 0, attempts: 3) do |status, trace, _|
      assert_equal 17, status
      assert_equal %w[connect migrate], trace
    end
  end

  def test_only_connection_failure_is_retried
    with_release(connect_failures: 1, attempts: 2) do |status, trace, _|
      assert_equal 17, status
      assert_equal %w[connect connect migrate], trace
    end
  end

  def test_unavailable_database_exhausts_bounded_attempts_without_migration
    with_release(connect_failures: 5, attempts: 2) do |status, trace, stderr|
      assert_equal 1, status
      assert_equal %w[connect connect], trace
      assert_includes stderr, "migrations not run"
    end
  end

  def test_out_of_range_attempt_limit_is_rejected
    with_release(connect_failures: 0, attempts: 61) do |status, trace, _|
      assert_equal 1, status
      assert_empty trace
    end
  end
end
