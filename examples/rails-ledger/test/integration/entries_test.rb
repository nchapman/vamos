require "test_helper"

class EntriesTest < ActionDispatch::IntegrationTest
  # Override Rails test-helper's test adapter: these requests must really enqueue SQL rows.
  def queue_adapter_for_test
    ActiveJob::QueueAdapters::SolidQueueAdapter.new
  end

  test "create and retrieve durable pending entry without a worker" do
    assert_difference ["Entry.count", "SolidQueue::Job.count", "SolidQueue::ReadyExecution.count"], 1 do
      post "/entries", params: { description: "  demo entry ", amount: 42, worker_marker: "forged" }, as: :json
      assert_response :accepted
    end
    payload = response.parsed_body
    assert_equal "pending", payload["status"]
    assert_nil payload["worker_marker"]
    assert_match(/\A[0-9a-f]{24}\z/, response.headers["X-Demo-Instance"])
    marker = response.headers["X-Demo-Instance"]
    get response.headers["Location"]
    assert_response :ok
    assert_equal payload, response.parsed_body
    assert_equal marker, response.headers["X-Demo-Instance"]
    assert_equal "no-store", response.headers["Cache-Control"]
  end

  test "invalid input creates neither an entry nor a job" do
    [{ description: "x", amount: "42" }, { description: "x", amount: 1.2 },
     { description: "x" * 201, amount: 1 }, { description: "x", amount: 1_000_000_001 },
     { description: " ", amount: 1 }, { amount: 1 }].each do |payload|
      assert_no_difference ["Entry.count", "SolidQueue::Job.count"] do
        post "/entries", params: payload, as: :json
        assert_response :unprocessable_entity
      end
    end
  end

  test "unknown entry is JSON 404" do
    get "/entries/0"
    assert_response :not_found
    assert_equal({ "error" => "entry not found" }, response.parsed_body)
  end
end
