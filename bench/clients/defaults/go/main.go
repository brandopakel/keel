// Default-configuration probe for go-redis and Redigo: only the address and password are set.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	redigo "github.com/gomodule/redigo/redis"
	redis "github.com/redis/go-redis/v9"
)

func emit(lib, server, scen string, err error) {
	r := map[string]any{"library": lib, "server": server, "scenario": scen, "ok": err == nil, "error": nil}
	if err != nil {
		r["error"] = err.Error()
	}
	b, _ := json.Marshal(r)
	fmt.Println(string(b))
}

func main() {
	addr, server, pass := os.Args[1], os.Args[2], os.Getenv("PROBE_PASSWORD")
	ctx := context.Background()
	for _, scen := range []string{"default", "named", "pipeline", "tx"} {
		opt := &redis.Options{Addr: addr, Password: pass, MaxRetries: -1, DialTimeout: 3 * time.Second, ReadTimeout: 3 * time.Second}
		if scen == "named" {
			opt.ClientName = "probe"
		}
		c := redis.NewClient(opt)
		key := "probe:go-redis:" + scen
		var err error
		switch scen {
		case "default", "named":
			if err = c.Set(ctx, key, "v", 0).Err(); err == nil {
				var v string
				if v, err = c.Get(ctx, key).Result(); err == nil && v != "v" {
					err = fmt.Errorf("GET mismatch %q", v)
				}
			}
		case "pipeline":
			_, err = c.Pipelined(ctx, func(p redis.Pipeliner) error { p.Set(ctx, key, "v", 0); p.Get(ctx, key); return nil })
		case "tx":
			_, err = c.TxPipelined(ctx, func(p redis.Pipeliner) error { p.Set(ctx, key, "v", 0); p.Get(ctx, key); return nil })
		}
		c.Close()
		emit("go-redis v9.22.0", server, scen, err)
	}
	for _, scen := range []string{"default", "named", "pipeline", "tx"} {
		opts := []redigo.DialOption{redigo.DialConnectTimeout(3 * time.Second), redigo.DialReadTimeout(3 * time.Second)}
		if pass != "" {
			opts = append(opts, redigo.DialPassword(pass))
		}
		if scen == "named" {
			opts = append(opts, redigo.DialClientName("probe"))
		}
		key := "probe:redigo:" + scen
		c, err := redigo.Dial("tcp", addr, opts...)
		if err == nil {
			switch scen {
			case "default", "named":
				if _, err = c.Do("SET", key, "v"); err == nil {
					var v string
					if v, err = redigo.String(c.Do("GET", key)); err == nil && v != "v" {
						err = fmt.Errorf("GET mismatch %q", v)
					}
				}
			case "pipeline":
				c.Send("SET", key, "v")
				c.Send("GET", key)
				if err = c.Flush(); err == nil {
					if _, err = c.Receive(); err == nil {
						_, err = c.Receive()
					}
				}
			case "tx":
				c.Send("MULTI")
				c.Send("SET", key, "v")
				c.Send("GET", key)
				_, err = c.Do("EXEC")
			}
			c.Close()
		}
		emit("redigo v1.9.3", server, scen, err)
	}
}
