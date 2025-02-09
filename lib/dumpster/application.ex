defmodule Dumpster.Application do
  # See https://hexdocs.pm/elixir/Application.html
  # for more information on OTP Applications
  @moduledoc false

  use Application

  @impl true
  def start(_type, _args) do
    children = [
      DumpsterWeb.Telemetry,
      Dumpster.Repo,
      {DNSCluster, query: Application.get_env(:dumpster, :dns_cluster_query) || :ignore},
      {Phoenix.PubSub, name: Dumpster.PubSub},
      # Start the Finch HTTP client for sending emails
      {Finch, name: Dumpster.Finch},
      # Start a worker by calling: Dumpster.Worker.start_link(arg)
      # {Dumpster.Worker, arg},
      # Start to serve requests, typically the last entry
      DumpsterWeb.Endpoint
    ]

    # See https://hexdocs.pm/elixir/Supervisor.html
    # for other strategies and supported options
    opts = [strategy: :one_for_one, name: Dumpster.Supervisor]
    Supervisor.start_link(children, opts)
  end

  # Tell Phoenix to update the endpoint configuration
  # whenever the application is updated.
  @impl true
  def config_change(changed, _new, removed) do
    DumpsterWeb.Endpoint.config_change(changed, removed)
    :ok
  end
end
