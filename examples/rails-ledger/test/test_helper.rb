ENV["RAILS_ENV"] ||= "test"
require_relative "../config/environment"
require "rails/test_help"

module ActiveSupport
  class TestCase
    # Serial: schema-failure tests deliberately modify tables inside rolled-back transactions.
    # Use ONLY a disposable DB_TEST_NAME database; never point tests at live data.
  end
end
