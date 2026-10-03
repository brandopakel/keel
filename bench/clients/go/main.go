package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	redigo "github.com/gomodule/redigo/redis"
	redis "github.com/redis/go-redis/v9"
)

type testCase struct {
	Name     string
	Args     []any
	Expected any
	Error    string
	Range    []float64
}
type fixture struct {
	Commands, Verification []testCase
	VerifyOnly             bool `json:"verify_only"`
	Prefix                 string
	MarkerKey              string   `json:"marker_key"`
	MarkerValue            any      `json:"marker_value"`
	CounterKey             string   `json:"counter_key"`
	ExpiryKey              string   `json:"expiry_key"`
	ScanKeys               []string `json:"scan_keys"`
	Transaction            txFixture
}
type txFixture struct {
	StringKey  string `json:"string_key"`
	CounterKey string `json:"counter_key"`
	AbortedKey string `json:"aborted_key"`
	Value      any
}
type client struct {
	call        func([]any) (any, error)
	pipeline    func(string) ([]int64, error)
	transaction func(txFixture)
	close       func() error
}

func connect(library string) client {
	address, password := os.Getenv("KEEL_COMPAT_ADDR"), os.Getenv("KEEL_COMPAT_PASSWORD")
	if library == "go-redis" {
		c := redis.NewClient(&redis.Options{Addr: address, Password: password, Protocol: 2, MaxRetries: -1, DialTimeout: 3 * time.Second, ReadTimeout: 3 * time.Second, WriteTimeout: 3 * time.Second})
		return client{call: func(args []any) (any, error) {
			v, e := c.Do(context.Background(), args...).Result()
			if errors.Is(e, redis.Nil) {
				return nil, nil
			}
			return v, e
		},
			pipeline: func(key string) ([]int64, error) {
				p := c.Pipeline()
				commands := make([]*redis.IntCmd, 257)
				for i := range commands {
					commands[i] = p.Incr(context.Background(), key)
				}
				if _, err := p.Exec(context.Background()); err != nil {
					return nil, err
				}
				out := make([]int64, len(commands))
				for i, cmd := range commands {
					out[i] = cmd.Val()
				}
				return out, nil
			},
			transaction: func(x txFixture) {
				ctx := context.Background()
				value := arguments([]any{x.Value})[0]
				var set *redis.StatusCmd
				var incr, push *redis.IntCmd
				var get *redis.StringCmd
				// TxPipelined's own error is not asserted: callers read each
				// command's result, and LPUSH is meant to fail.
				_, _ = c.TxPipelined(ctx, func(p redis.Pipeliner) error {
					set = p.Set(ctx, x.StringKey, value, 0)
					incr = p.Incr(ctx, x.CounterKey)
					push = p.LPush(ctx, x.StringKey, "x")
					get = p.Get(ctx, x.StringKey)
					return nil
				})
				must(set.Err())
				equal(set.Val(), "OK", "transaction SET")
				must(incr.Err())
				equal(incr.Val(), 1, "transaction INCR")
				if push.Err() == nil || !strings.Contains(push.Err().Error(), "WRONGTYPE") {
					panic(fmt.Sprintf("transaction LPUSH: %v", push.Err()))
				}
				must(get.Err())
				equal(get.Val(), x.Value, "transaction GET")
				var queued *redis.StatusCmd
				_, err := c.TxPipelined(ctx, func(p redis.Pipeliner) error {
					queued = p.Set(ctx, x.AbortedKey, "never", 0)
					p.Do(ctx, "NOSUCHCOMMAND")
					return nil
				})
				if err == nil || queued.Err() == nil || !strings.Contains(err.Error()+queued.Err().Error(), "EXECABORT") {
					panic(fmt.Sprintf("aborted transaction: %v / %v", err, queued.Err()))
				}
			}, close: c.Close}
	}
	if library != "redigo" {
		panic("unknown Go library")
	}
	options := []redigo.DialOption{redigo.DialConnectTimeout(3 * time.Second), redigo.DialReadTimeout(3 * time.Second), redigo.DialWriteTimeout(3 * time.Second)}
	if password != "" {
		options = append(options, redigo.DialPassword(password))
	}
	c, err := redigo.Dial("tcp", address, options...)
	must(err)
	return client{call: func(args []any) (any, error) { return c.Do(args[0].(string), args[1:]...) },
		pipeline: func(key string) ([]int64, error) {
			for i := 0; i < 257; i++ {
				if err := c.Send("INCR", key); err != nil {
					return nil, err
				}
			}
			if err := c.Flush(); err != nil {
				return nil, err
			}
			out := make([]int64, 257)
			for i := range out {
				v, err := redigo.Int64(c.Receive())
				if err != nil {
					return nil, err
				}
				out[i] = v
			}
			return out, nil
		},
		transaction: func(x txFixture) {
			// Redigo's transaction is MULTI and the queued commands sent, then
			// Do("EXEC"), which reads every pending reply and returns EXEC's.
			value := arguments([]any{x.Value})[0]
			must(c.Send("MULTI"))
			must(c.Send("SET", x.StringKey, value))
			must(c.Send("INCR", x.CounterKey))
			must(c.Send("LPUSH", x.StringKey, "x"))
			must(c.Send("GET", x.StringKey))
			replies, err := redigo.Values(c.Do("EXEC"))
			must(err)
			if len(replies) != 4 {
				panic(fmt.Sprintf("transaction: %d replies", len(replies)))
			}
			equal(replies[0], "OK", "transaction SET")
			equal(replies[1], 1, "transaction INCR")
			if e, ok := replies[2].(redigo.Error); !ok || !strings.Contains(string(e), "WRONGTYPE") {
				panic(fmt.Sprintf("transaction LPUSH: %v", replies[2]))
			}
			equal(replies[3], x.Value, "transaction GET")
			must(c.Send("MULTI"))
			must(c.Send("SET", x.AbortedKey, "never"))
			must(c.Send("NOSUCHCOMMAND"))
			reply, err := c.Do("EXEC")
			if e, ok := reply.(redigo.Error); !ok || !strings.HasPrefix(string(e), "EXECABORT") || err == nil {
				panic(fmt.Sprintf("aborted transaction: %v / %v", reply, err))
			}
		}, close: c.Close}
}
func must(err error) {
	if err != nil {
		panic(err)
	}
}
func arguments(raw []any) []any {
	args := make([]any, len(raw))
	for i, v := range raw {
		if m, ok := v.(map[string]any); ok {
			b, e := hex.DecodeString(m["hex"].(string))
			must(e)
			args[i] = b
		} else {
			args[i] = v
		}
	}
	return args
}
func normalize(value any) any {
	switch v := value.(type) {
	case []byte:
		return normalize(string(v))
	case string:
		if !utf8.ValidString(v) {
			return map[string]any{"hex": hex.EncodeToString([]byte(v))}
		}
		return v
	case []any:
		out := make([]any, len(v))
		for i, x := range v {
			out[i] = normalize(x)
		}
		return out
	default:
		return value
	}
}
func equal(actual, expected any, name string) {
	a, e := json.Marshal(normalize(actual))
	must(e)
	b, e := json.Marshal(expected)
	must(e)
	if string(a) != string(b) {
		panic(fmt.Sprintf("%s: got %s expected %s", name, a, b))
	}
}
func check(c client, test testCase) {
	v, err := c.call(arguments(test.Args))
	if test.Error != "" {
		if err == nil || !strings.Contains(err.Error(), test.Error) {
			panic(fmt.Sprintf("%s: expected %s, got %v", test.Name, test.Error, err))
		}
		return
	}
	must(err)
	if len(test.Range) > 0 {
		n, err := strconv.ParseFloat(fmt.Sprint(v), 64)
		must(err)
		if n < test.Range[0] || n > test.Range[1] {
			panic(test.Name + ": outside range")
		}
		return
	}
	equal(v, test.Expected, test.Name)
}
func main() {
	if len(os.Args) != 3 {
		panic("library and fixture required")
	}
	library := os.Args[1]
	raw, err := os.ReadFile(os.Args[2])
	must(err)
	var f fixture
	must(json.Unmarshal(raw, &f))
	c := connect(library)
	defer func() { _ = c.close() }()
	if !f.VerifyOnly {
		for _, test := range f.Commands {
			check(c, test)
		}
		deadline := time.Now().Add(3 * time.Second)
		for {
			v, err := c.call([]any{"GET", f.ExpiryKey})
			must(err)
			if v == nil {
				break
			}
			if time.Now().After(deadline) {
				panic("expiry did not occur")
			}
			time.Sleep(10 * time.Millisecond)
		}
		values, err := c.pipeline(f.CounterKey)
		must(err)
		for i, v := range values {
			if v != int64(i+1) {
				panic("pipeline order")
			}
		}
		seen := make(map[string]bool)
		cursor := "0"
		for {
			v, err := c.call([]any{"SCAN", cursor, "MATCH", f.Prefix + "*", "COUNT", "7", "TYPE", "STRING"})
			must(err)
			parts := normalize(v).([]any)
			cursor = fmt.Sprint(parts[0])
			for _, key := range parts[1].([]any) {
				seen[key.(string)] = true
			}
			if cursor == "0" {
				break
			}
		}
		keys := make([]string, 0, len(seen))
		for key := range seen {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		sort.Strings(f.ScanKeys)
		if !reflect.DeepEqual(keys, f.ScanKeys) {
			panic(fmt.Sprintf("SCAN: %v expected %v", keys, f.ScanKeys))
		}
		c.transaction(f.Transaction)
	}
	for _, test := range f.Verification {
		check(c, test)
	}
	must(c.close())
	c = connect(library)
	v, err := c.call([]any{"GET", f.MarkerKey})
	must(err)
	equal(v, f.MarkerValue, "reconnect")
	version := "v1.9.3"
	if library == "go-redis" {
		version = redis.Version()
	}
	must(json.NewEncoder(os.Stdout).Encode(map[string]any{"library": library, "version": version, "status": "passed", "verify_only": f.VerifyOnly}))
}
