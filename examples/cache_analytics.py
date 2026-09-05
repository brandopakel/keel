"""Cache results and keep a time-limited frequency sketch using redis-py.
Install: python -m pip install 'redis==5.2.1'
Run: KEEL_PORT=8081 python examples/cache_analytics.py
"""
import os
import redis

client = redis.Redis(
    host=os.getenv("KEEL_HOST", "127.0.0.1"),
    port=int(os.getenv("KEEL_PORT", "8081")),
    password=os.getenv("KEEL_PASSWORD"),
    protocol=2,
    decode_responses=True,
    socket_connect_timeout=2,
    socket_timeout=2,
)
# Dedicated example keys: rerunning does not accumulate old analytics.
client.delete("example:result", "example:views", "example:profile")
assert client.set("example:result", "cached answer", nx=True, ex=60)
assert client.get("example:result") == "cached answer"
assert not client.set("example:result", "replacement", nx=True)
client.execute_command("CMS.INITBYDIM", "example:views", 1000, 5)
# redis-py pipelines default to MULTI/EXEC, which Keel does not support.
with client.pipeline(transaction=False) as pipeline:
    pipeline.execute_command("CMS.INCRBY", "example:views", "article:42", 1)
    pipeline.expire("example:views", 60)
    pipeline.hset("example:profile", mapping={"name": "Ada"})
    pipeline.expire("example:profile", 60)
    pipeline.execute()
assert client.ttl("example:views") > 0
assert client.ttl("example:profile") > 0
assert client.memory_usage("example:profile") > 0
print("cache:", client.get("example:result"))
print("estimated views:", client.execute_command("CMS.QUERY", "example:views", "article:42"))
client.close()
