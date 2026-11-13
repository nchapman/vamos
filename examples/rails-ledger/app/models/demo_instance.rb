require "securerandom"

# Opaque, per-process marker: no hostname, environment, credential, or IP disclosure.
# PID-aware because Solid Queue forks after loading the application.
class DemoInstance
  MUTEX = Mutex.new

  def self.marker
    MUTEX.synchronize do
      if @pid != Process.pid
        @marker = SecureRandom.hex(12)
        @pid = Process.pid
      end
      @marker
    end
  end
end
