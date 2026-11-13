require "test_helper"

class EntryTest < ActiveSupport::TestCase
  test "bounded strict input" do
    assert Entry.new(description: "demo", amount: 0).valid?
    assert Entry.new(description: "x" * 200, amount: -Entry::MAX_AMOUNT).valid?
    assert Entry.new(description: "demo", amount: Entry::MAX_AMOUNT).valid?
    [nil, "", "   ", "x" * 201, 12].each do |description|
      assert_not Entry.new(description: description, amount: 1).valid?, description.inspect
    end
    [nil, "12", "1.5", 1.5, true, Entry::MAX_AMOUNT + 1, -Entry::MAX_AMOUNT - 1].each do |amount|
      assert_not Entry.new(description: "demo", amount: amount).valid?, amount.inspect
    end
  end
end
