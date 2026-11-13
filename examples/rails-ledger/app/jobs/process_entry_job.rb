class ProcessEntryJob < ApplicationJob
  queue_as :default

  def perform(entry_id)
    entry = Entry.find(entry_id)
    entry.with_lock do
      return if entry.processed_at?

      entry.update!(
        normalized_description: entry.description.strip.gsub(/\s+/, " ").upcase,
        processed_at: Time.current,
        worker_marker: DemoInstance.marker
      )
    end
  end
end
