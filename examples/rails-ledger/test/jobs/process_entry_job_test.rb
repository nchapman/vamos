require "test_helper"

class ProcessEntryJobTest < ActiveSupport::TestCase
  test "duplicate delivery leaves the first durable effect unchanged" do
    entry = Entry.create!(description: "  coffee   beans  ", amount: -25)
    ProcessEntryJob.perform_now(entry.id)
    original = entry.reload.attributes
    assert_equal "COFFEE BEANS", entry.normalized_description
    assert_equal -25, entry.amount
    assert_match(/\A[0-9a-f]{24}\z/, entry.worker_marker)
    assert_not_nil entry.processed_at

    ProcessEntryJob.perform_now(entry.id)
    assert_equal original, entry.reload.attributes
  end
end
