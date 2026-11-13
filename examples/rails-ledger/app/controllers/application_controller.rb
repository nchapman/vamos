class ApplicationController < ActionController::API
  before_action :identify_demo_instance

  private
    def identify_demo_instance
      response.set_header("X-Demo-Instance", DemoInstance.marker)
      response.set_header("Cache-Control", "no-store")
    end
end
