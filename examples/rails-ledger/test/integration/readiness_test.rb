require "test_helper"

class ReadinessTest < ActionDispatch::IntegrationTest
  test "schema ready even with no worker" do
    assert_equal 0, SolidQueue::Process.count
    get "/readyz"
    assert_response :ok
    assert_equal({ "status" => "ready" }, response.parsed_body)
  end

  test "missing queue table is not ready" do
    ActiveRecord::Base.connection.execute("ALTER TABLE solid_queue_ready_executions RENAME TO hidden_ready_executions")
    get "/readyz"
    assert_response :service_unavailable
    assert_equal({ "status" => "not_ready" }, response.parsed_body)
  end

  test "missing application column is not ready" do
    ActiveRecord::Base.connection.execute("ALTER TABLE entries RENAME COLUMN processed_at TO hidden_processed_at")
    get "/readyz"
    assert_response :service_unavailable
  end

  test "missing release version is not ready" do
    ActiveRecord::Base.connection.execute("DELETE FROM schema_migrations WHERE version = '20261113000001'")
    get "/readyz"
    assert_response :service_unavailable
  end
end
