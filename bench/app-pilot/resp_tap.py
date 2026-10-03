#!/usr/bin/env python3
"""Transparent RESP proxy that records every command an application sends.

The pilot needs the command mix an unmodified application really uses,
including the handshake its client library sends on each connection, and every
error the server returns. Neither server can report that by itself: Keel has no
MONITOR and an unknown command never reaches its logs. So both arms put this
proxy between the application and the server, and the same bytes reach either.

Bytes are forwarded unchanged. One JSON line is written per command once its
reply has been parsed: connection, sequence number, command name (with the
subcommand for container commands such as CLIENT), option keywords, and the
reply type or error text. Replies are parsed as RESP2 or RESP3, because a
client that negotiates RESP3 with Redis would otherwise break reply pairing.

When the upstream server is down the client connection is closed at once,
which is what an application sees when the real server dies.
"""
import argparse
import asyncio
import collections
import json
import time

CONTAINER_COMMANDS = {"CLIENT", "CONFIG", "COMMAND", "MEMORY", "INFO", "HELLO",
                      "SCRIPT", "FUNCTION", "CLUSTER", "ACL", "OBJECT", "XINFO",
                      "MODULE", "DEBUG", "LATENCY", "SLOWLOG", "PUBSUB"}

# Where a command's option keywords start. Arguments before that position are
# keys and values, which can legitimately be the word "count" or "ex".
OPTION_START = {"SET": 3, "EXPIRE": 3, "PEXPIRE": 3, "EXPIREAT": 3, "PEXPIREAT": 3,
                "SCAN": 2, "SSCAN": 3, "HSCAN": 3, "ZSCAN": 3, "ZADD": 2,
                "ZRANGE": 4, "ZRANGEBYSCORE": 4, "ZREVRANGEBYSCORE": 4,
                "GETEX": 2, "LPOS": 3, "HELLO": 2, "CLIENT": 2}
OPTION_WORDS = {"EX", "PX", "EXAT", "PXAT", "NX", "XX", "GT", "LT", "KEEPTTL", "GET",
                "MATCH", "COUNT", "TYPE", "WITHSCORES", "LIMIT", "CH", "INCR", "REV",
                "BYSCORE", "BYLEX", "PERSIST", "AUTH", "SETNAME", "LIB-NAME", "LIB-VER",
                "ON", "OFF", "RANK", "MAXLEN"}


class Incomplete(Exception):
    pass


def parse_value(buf, pos):
    """Return (kind, value, next_pos) for the RESP value starting at pos."""
    if pos >= len(buf):
        raise Incomplete
    marker = chr(buf[pos])
    end = buf.find(b"\r\n", pos)
    if end < 0:
        raise Incomplete
    line = bytes(buf[pos + 1:end])
    nxt = end + 2
    if marker in "+-:_,#(":
        return marker, line, nxt
    if marker in "$!=":
        n = int(line)
        if n < 0:
            return marker, None, nxt
        if len(buf) < nxt + n + 2:
            raise Incomplete
        return marker, bytes(buf[nxt:nxt + n]), nxt + n + 2
    if marker in "*~>%|":
        n = int(line)
        if n < 0:
            return marker, None, nxt
        items = []
        for _ in range(n * 2 if marker in "%|" else n):
            kind, value, nxt = parse_value(buf, nxt)
            items.append((kind, value))
        if marker == "|":  # an attribute precedes the reply it describes
            return parse_value(buf, nxt)
        return marker, items, nxt
    # An inline command: a plain line of words, which only clients send. It
    # has no type marker, so its first byte belongs to the command name.
    return "inline", bytes(buf[pos:end]).split(), nxt


def command_record(kind, value):
    """Describe one client request, or return None if it is not a command."""
    if kind == "inline":
        args = [bytes(a) for a in value]
    elif kind == "*" and value:
        args = [v if isinstance(v, bytes) else b"" for _, v in value]
    else:
        return None
    if not args:
        return None
    name = args[0].decode("utf-8", "replace").upper()
    rec = {"cmd": name, "nargs": len(args) - 1}
    if name in CONTAINER_COMMANDS and len(args) > 1:
        rec["sub"] = args[1].decode("utf-8", "replace").upper()
    start = OPTION_START.get(name)
    if start is not None:
        opts = {a.decode("utf-8", "replace").upper() for a in args[start:] if len(a) <= 12}
        opts &= OPTION_WORDS
        if opts:
            rec["opts"] = sorted(opts)
    return rec


class Stream:
    """One direction of a connection: buffers bytes and hands out whole values.

    A frame the parser cannot read stops the logging for this direction only;
    the bytes keep flowing, so a parser bug can cost evidence but never change
    what the application sees.
    """

    def __init__(self, on_value, on_error):
        self.buf = bytearray()
        self.on_value, self.on_error = on_value, on_error
        self.failed = None

    def feed(self, data):
        if self.failed:
            return
        self.buf.extend(data)
        pos = 0
        try:
            while True:
                try:
                    kind, value, next_pos = parse_value(self.buf, pos)
                except Incomplete:
                    break
                pos = next_pos
                self.on_value(kind, value)
        except (ValueError, IndexError) as exc:
            self.failed = repr(exc)
            self.buf.clear()
            self.on_error(self.failed)
            return
        del self.buf[:pos]


async def pipe(reader, writer, on_data):
    try:
        while data := await reader.read(65536):
            on_data(data)
            writer.write(data)
            await writer.drain()
    except (ConnectionError, asyncio.CancelledError):
        pass
    finally:
        try:
            writer.close()
        except Exception:
            pass


class Tap:
    def __init__(self, upstream, log_path):
        self.upstream = upstream
        self.log = open(log_path, "a", buffering=1)
        self.next_conn = 0

    def emit(self, rec):
        self.log.write(json.dumps(rec, sort_keys=True) + "\n")

    async def handle(self, creader, cwriter):
        self.next_conn += 1
        conn = self.next_conn
        try:
            ureader, uwriter = await asyncio.open_connection("127.0.0.1", self.upstream)
        except OSError as exc:
            self.emit({"conn": conn, "event": "upstream_unavailable", "error": repr(exc), "t": time.time()})
            cwriter.close()
            return
        self.emit({"conn": conn, "event": "open", "t": time.time()})
        pending = collections.deque()
        seq = 0

        def on_command(kind, value):
            nonlocal seq
            rec = command_record(kind, value)
            if rec is not None:
                seq += 1
                rec.update(conn=conn, seq=seq)
                pending.append(rec)

        def on_reply(kind, value):
            if kind == ">":  # a RESP3 push is not the reply to any request
                self.emit({"conn": conn, "event": "push"})
                return
            rec = pending.popleft() if pending else {"conn": conn, "cmd": "?unpaired"}
            rec["reply"] = kind
            if kind in "-!":
                rec["error"] = (value or b"").decode("utf-8", "replace")[:200]
            self.emit(rec)

        def broken(direction, error):
            self.emit({"conn": conn, "event": "parse_error", "direction": direction, "error": error})

        requests = Stream(on_command, lambda e: broken("request", e))
        replies = Stream(on_reply, lambda e: broken("reply", e))
        await asyncio.gather(pipe(creader, uwriter, requests.feed),
                             pipe(ureader, cwriter, replies.feed))
        for rec in pending:
            rec["reply"] = "none: connection closed"
            self.emit(rec)
        self.emit({"conn": conn, "event": "close", "t": time.time()})


async def serve(listen, upstream, log):
    tap = Tap(upstream, log)
    server = await asyncio.start_server(tap.handle, "127.0.0.1", listen)
    async with server:
        await server.serve_forever()


def main():
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--listen", type=int, required=True)
    ap.add_argument("--upstream", type=int, required=True)
    ap.add_argument("--log", required=True)
    args = ap.parse_args()
    asyncio.run(serve(args.listen, args.upstream, args.log))


if __name__ == "__main__":
    main()
