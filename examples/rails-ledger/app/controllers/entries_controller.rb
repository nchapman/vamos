class EntriesController < ApplicationController
  rescue_from ActiveRecord::RecordNotFound do
    render json: { error: "entry not found" }, status: :not_found
  end

  rescue_from ActiveRecord::RecordInvalid do |error|
    render json: { errors: error.record.errors.to_hash }, status: :unprocessable_entity
  end

  def create
    # Deliberately flat JSON; no client-supplied processing fields or arbitrary job payloads.
    entry = Entry.submit!(params.permit(:description, :amount).to_h)
    render json: entry.api_attributes, status: :accepted, location: entry_path(entry)
  end

  def show
    render json: Entry.find(params[:id]).api_attributes
  end
end
