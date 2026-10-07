# ActionCable's Redis adapter against the server at REDIS_URL: subscribe to a
# channel and receive a broadcast on it, as an application's cable server does.
# A Rails application loads ActiveSupport's extensions, which ActionCable uses.
require 'active_support/all'
require 'action_cable'
require 'logger'
require 'timeout'

server = ActionCable::Server::Base.new
server.config.cable = { 'adapter' => 'redis', 'url' => ENV.fetch('REDIS_URL') }
server.config.logger = Logger.new($stderr, level: Logger::WARN)

received = Queue.new
subscribed = Queue.new
server.pubsub.subscribe('check', ->(message) { received << message }, -> { subscribed << true })
Timeout.timeout(10, RuntimeError, 'the subscription was never confirmed') { subscribed.pop }
server.pubsub.broadcast('check', 'hello')
message = Timeout.timeout(10, RuntimeError, 'the broadcast never arrived') { received.pop }
raise "got #{message.inspect}" unless message == 'hello'
server.pubsub.shutdown
puts 'ok: a broadcast reached its subscriber'
