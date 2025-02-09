defmodule Dumpster.Repo do
  use Ecto.Repo,
    otp_app: :dumpster,
    adapter: Ecto.Adapters.Postgres
end
