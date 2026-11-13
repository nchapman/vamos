class ReadinessController < ApplicationController
  def show
    Readiness.check!
    render json: { status: "ready" }
  rescue ActiveRecord::ActiveRecordError, Readiness::SchemaMissing
    render json: { status: "not_ready" }, status: :service_unavailable
  end
end
