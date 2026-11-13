class Readiness
  class SchemaMissing < StandardError; end

  # Query live schema (not the schema cache). Worker liveness is deliberately irrelevant.
  REQUIRED_COLUMNS = {
    "entries" => %w[id description amount normalized_description processed_at worker_marker created_at updated_at],
    "solid_queue_jobs" => %w[id queue_name class_name arguments priority active_job_id scheduled_at finished_at concurrency_key created_at updated_at batch_id],
    "solid_queue_ready_executions" => %w[id job_id queue_name priority created_at],
    "solid_queue_scheduled_executions" => %w[id job_id queue_name priority scheduled_at created_at],
    "solid_queue_claimed_executions" => %w[id job_id process_id created_at],
    "solid_queue_failed_executions" => %w[id job_id error created_at],
    "solid_queue_blocked_executions" => %w[id job_id queue_name priority concurrency_key expires_at created_at],
    "solid_queue_processes" => %w[id kind last_heartbeat_at supervisor_id pid hostname metadata created_at name],
    "solid_queue_pauses" => %w[id queue_name created_at],
    "solid_queue_semaphores" => %w[id key value expires_at created_at updated_at],
    "solid_queue_recurring_executions" => %w[id job_id task_key run_at created_at],
    "solid_queue_recurring_tasks" => %w[id key schedule command class_name arguments queue_name priority static description created_at updated_at],
    "solid_queue_batches" => %w[id active_job_batch_id description on_finish on_success on_failure metadata total_jobs completed_jobs failed_jobs enqueued_at finished_at failed_at created_at updated_at],
    "solid_queue_batch_executions" => %w[id job_id batch_id created_at]
  }.freeze
  MIGRATIONS = %w[20261113000000 20261113000001].freeze

  def self.check!
    ActiveRecord::Base.connection_pool.with_connection do |connection|
      connection.select_value("SELECT 1")
      versions = connection.select_values("SELECT version FROM schema_migrations")
      raise SchemaMissing unless (MIGRATIONS - versions).empty?

      REQUIRED_COLUMNS.each do |table, columns|
        projection = columns.map { |column| connection.quote_column_name(column) }.join(", ")
        connection.execute("SELECT #{projection} FROM #{connection.quote_table_name(table)} LIMIT 0")
      end
    end
    true
  end
end
