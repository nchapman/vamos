Rails.application.routes.draw do
  get "readyz", to: "readiness#show"
  resources :entries, only: %i[create show]
end
