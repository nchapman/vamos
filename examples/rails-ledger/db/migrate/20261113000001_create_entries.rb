class CreateEntries < ActiveRecord::Migration[8.1]
  def change
    create_table :entries do |t|
      t.string :description, limit: 200, null: false
      t.integer :amount, null: false
      t.string :normalized_description, limit: 600
      t.datetime :processed_at
      t.string :worker_marker, limit: 64
      t.timestamps
    end
    add_check_constraint :entries, "amount BETWEEN -1000000000 AND 1000000000", name: "entries_bounded_amount"
  end
end
