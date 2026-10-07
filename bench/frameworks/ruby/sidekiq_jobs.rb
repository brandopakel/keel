# The job the Sidekiq check runs, loaded by both the client and the Sidekiq
# process it starts.
require 'sidekiq'

Sidekiq.configure_server do |config|
  config.redis = { url: ENV.fetch('REDIS_URL') }
  # Scheduled jobs are polled for; the check's one is due in a second.
  config.average_scheduled_poll_interval = 1
end
Sidekiq.configure_client { |config| config.redis = { url: ENV.fetch('REDIS_URL') } }

class CheckJob
  include Sidekiq::Job

  def perform(n)
    Sidekiq.redis { |r| r.call('INCRBY', 'check:sidekiq:sum', n) }
  end
end
