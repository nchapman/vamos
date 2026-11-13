class Entry < ApplicationRecord
  MAX_AMOUNT = 1_000_000_000

  validates :description, presence: true, length: { maximum: 200 }
  validates :amount, numericality: { only_integer: true, in: -MAX_AMOUNT..MAX_AMOUNT }
  validate :strict_input_types

  # No after_commit callback: failure to persist the queue row must roll back the entry.
  def self.submit!(attributes)
    transaction do
      entry = create!(attributes)
      job = ProcessEntryJob.perform_later(entry.id)
      raise ActiveJob::EnqueueError, "Entry job was not enqueued" unless job&.successfully_enqueued?
      entry
    end
  end

  def api_attributes
    attributes.slice("id", "description", "amount", "normalized_description", "processed_at", "worker_marker", "created_at")
      .merge("status" => processed_at ? "processed" : "pending")
  end

  private
    def strict_input_types
      errors.add(:description, "must be a string") unless description_before_type_cast.is_a?(String)
      errors.add(:amount, "must be a JSON integer") unless amount_before_type_cast.is_a?(Integer)
    end
end
