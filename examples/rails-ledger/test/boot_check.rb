# Standalone checks requiring no database. Run: bundle exec ruby test/boot_check.rb
# Intentionally NOT named *_test.rb: the normal suite boots in test, this in production.
ENV["RAILS_ENV"] = "production"
ENV["SECRET_KEY_BASE"] = "test-only-not-a-deployment-secret-" * 4
ENV["ALLOWED_HOSTS"] = "ledger.example.test"
ENV["DB_HOST"] = "127.0.0.1"
ENV["DB_PORT"] = "1" # deliberate closed local endpoint; never use a developer's DB
ENV.delete("DATABASE_URL")
require_relative "../config/environment"
require "minitest/autorun"
require "rack/mock"

class BootCheck < Minitest::Test
  def request(path, host: "ledger.example.test", https: false)
    Rack::MockRequest.new(Rails.application).get(path, "HTTP_HOST" => host,
      "HTTP_X_FORWARDED_PROTO" => (https ? "https" : "http"))
  end

  def with_ready_schema
    original = Readiness.method(:check!)
    Readiness.define_singleton_method(:check!) { true }
    yield
  ensure
    Readiness.define_singleton_method(:check!, original)
  end

  def test_unavailable_database_is_503_on_internal_http_not_redirect_or_host_rejection
    response = request("/readyz", host: "172.20.0.12:3000")
    assert_equal 503, response.status
    assert_equal({ "status" => "not_ready" }, JSON.parse(response.body))
    assert_nil response["location"]
  end

  def test_ready_schema_allows_internal_http_and_public_https
    with_ready_schema do
      assert_equal 200, request("/readyz", host: "172.20.0.12:3000").status
      assert_equal 200, request("/readyz", https: true).status
    end
  end

  def test_other_paths_retain_host_and_ssl_protection
    assert_equal 403, request("/entries/1", host: "attacker.invalid", https: true).status
    response = request("/entries/1")
    assert_equal 301, response.status
    assert_equal "https://ledger.example.test/entries/1", response["location"]
    assert_equal 404, request("/no-such-route", https: true).status
  end

  def test_transaction_configuration_and_runtime_pins
    assert_same Entry.connection_pool, SolidQueue::Record.connection_pool
    assert_equal false, ProcessEntryJob.enqueue_after_transaction_commit
    assert_instance_of ActiveJob::QueueAdapters::SolidQueueAdapter, ProcessEntryJob.queue_adapter
    assert_equal "3.4.10", RUBY_VERSION
    assert_equal "8.1.4.1", Rails.version
    assert_equal "1.7.2", Gem.loaded_specs.fetch("solid_queue").version.to_s
  end

  def test_demo_marker_is_opaque_stable_and_changes_after_fork
    marker = DemoInstance.marker
    assert_match(/\A[0-9a-f]{24}\z/, marker)
    assert_equal marker, DemoInstance.marker
    reader, writer = IO.pipe
    pid = fork do
      reader.close
      writer.write(DemoInstance.marker)
      writer.close
      exit! 0
    end
    writer.close
    child_marker = reader.read
    reader.close
    Process.wait(pid)
    refute_equal marker, child_marker
  end

  def test_queue_migration_matches_pinned_gem_schema
    template = File.read(Gem.loaded_specs.fetch("solid_queue").full_gem_path + "/lib/generators/solid_queue/install/templates/db/queue_schema.rb")
    migration = File.read(Rails.root.join("db/migrate/20261113000000_create_solid_queue_tables.rb"))
    normalize = ->(text) { text.lines.map(&:strip).reject { |line| line.empty? || line.start_with?("#") }.join("\n") }
    expected = template.lines[1...-1].join.gsub(", force: :cascade", "")
    actual = migration.split("  def change\n", 2).last.lines[0...-2].join
    assert_equal normalize.call(expected), normalize.call(actual)
  end
end
