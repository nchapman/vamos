require "test_helper"

class SolidQueueTest < ActiveSupport::TestCase
  include ActiveJob::TestHelper

  def queue_adapter_for_test
    ActiveJob::QueueAdapters::SolidQueueAdapter.new
  end

  test "queue and application share a connection and enqueue inside the transaction" do
    assert_same Entry.connection_pool, SolidQueue::Record.connection_pool
    assert_equal false, ProcessEntryJob.enqueue_after_transaction_commit
    assert_instance_of ActiveJob::QueueAdapters::SolidQueueAdapter, ProcessEntryJob.queue_adapter

    assert_no_difference ["Entry.count", "SolidQueue::Job.count", "SolidQueue::ReadyExecution.count"] do
      Entry.transaction(requires_new: true) do
        entry = Entry.submit!(description: "rollback", amount: 12)
        job = SolidQueue::Job.order(:id).last
        assert_equal [entry.id], job.arguments["arguments"]
        assert SolidQueue::ReadyExecution.exists?(job_id: job.id)
        raise ActiveRecord::Rollback
      end
    end
  end

  test "SQL enqueue failure rolls back the entry" do
    # A real DB insertion failure, not a mocked adapter. NOT VALID spares old rows.
    ActiveRecord::Base.connection.execute("ALTER TABLE solid_queue_jobs ADD CONSTRAINT reject_test_jobs CHECK (false) NOT VALID")
    assert_no_difference ["Entry.count", "SolidQueue::Job.count"] do
      assert_raises(SolidQueue::Job::EnqueueError) do
        Entry.transaction(requires_new: true) { Entry.submit!(description: "reject", amount: 1) }
      end
    end
  end

  test "real queue claim deserializes and performs the job" do
    entry = Entry.submit!(description: "  queued work  ", amount: 7)
    process = SolidQueue::Process.create!(kind: "Worker", name: "test-worker", pid: Process.pid, last_heartbeat_at: Time.current)
    claimed = SolidQueue::ReadyExecution.claim(["default"], 1, process.id)
    assert_equal 1, claimed.size
    claimed.first.perform
    assert_equal "QUEUED WORK", entry.reload.normalized_description
    assert_equal DemoInstance.marker, entry.worker_marker
    assert_not_nil entry.processed_at
    assert_equal 0, SolidQueue::FailedExecution.count
    assert_equal 0, SolidQueue::Job.count # successful queue records are not retained
  end
end
