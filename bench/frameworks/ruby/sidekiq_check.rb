# Sidekiq against the server at REDIS_URL: enqueue ten jobs and a scheduled
# one, run a Sidekiq process over them, and check that every one ran.
require_relative 'sidekiq_jobs'
require 'rbconfig'

Sidekiq.redis { |r| r.call('DEL', 'check:sidekiq:sum') }
(1..10).each { |n| CheckJob.perform_async(n) }
CheckJob.perform_in(1, 100)

sidekiq = Gem.bin_path('sidekiq', 'sidekiq')
pid = spawn(RbConfig.ruby, sidekiq, '-r', File.expand_path('sidekiq_jobs.rb', __dir__), '-c', '2')
begin
  deadline = Time.now + 30
  loop do
    sum = Sidekiq.redis { |r| r.call('GET', 'check:sidekiq:sum') }.to_i
    break if sum == 155
    raise 'the Sidekiq process exited' if Process.waitpid(pid, Process::WNOHANG)
    raise "timed out with #{sum} of 155 done" if Time.now > deadline
    sleep 0.2
  end
  puts 'ok: 11 jobs ran, one of them scheduled'
ensure
  begin
    Process.kill('TERM', pid)
    Process.wait(pid)
  rescue Errno::ESRCH, Errno::ECHILD
    nil
  end
end
