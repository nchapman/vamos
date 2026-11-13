class ApplicationJob < ActiveJob::Base
  # Deliberate: queue rows and entry rows commit on the SAME connection.
  # Moving Solid Queue to another database requires an outbox/reconciliation design.
  self.enqueue_after_transaction_commit = false
end
