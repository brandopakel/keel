# Rails' cache store against the server at REDIS_URL: the operations an
# application's Rails.cache uses.
require 'active_support'
require 'active_support/cache'
require 'active_support/cache/redis_cache_store'

# The store swallows Redis errors by default and answers as a miss; raising
# them is what shows a server failing a command.
store = ActiveSupport::Cache::RedisCacheStore.new(
  url: ENV.fetch('REDIS_URL'), namespace: 'check',
  error_handler: ->(method:, returning:, exception:) { raise exception }
)

def check(what, got, want)
  raise "#{what}: got #{got.inspect}, want #{want.inspect}" unless got == want
end

store.clear
store.write('a', { 'n' => 1 }, expires_in: 60)
check('read', store.read('a'), { 'n' => 1 })
check('exist?', store.exist?('a'), true)
check('fetch computes once', store.fetch('b') { 2 }, 2)
check('fetch reads', store.fetch('b') { 3 }, 2)
check('increment', store.increment('counter', 2, expires_in: 60), 2)
check('decrement', store.decrement('counter'), 1)
store.write_multi({ 'x' => 1, 'y' => 2 })
check('read_multi', store.read_multi('x', 'y', 'z'), { 'x' => 1, 'y' => 2 })
check('fetch_multi', store.fetch_multi('x', 'w') { |key| "made #{key}" }, { 'x' => 1, 'w' => 'made w' })
store.write('expiring', 1, expires_in: 1)
sleep 1.2
check('expiry', store.read('expiring'), nil)
check('delete', store.delete('a'), true)
store.write('match:1', 1)
store.write('match:2', 2)
store.delete_matched('match:*')
check('delete_matched', store.read_multi('match:1', 'match:2'), {})
store.clear
check('clear', store.read('b'), nil)
puts 'ok: read, write, fetch, counters, multi, expiry, delete_matched and clear'
