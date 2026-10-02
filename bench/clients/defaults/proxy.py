# Logging TCP proxy: records each connection's raw traffic in both directions.
import asyncio, sys

async def pipe(reader, writer, log, tag, cid):
    try:
        while True:
            data = await reader.read(65536)
            if not data:
                break
            log.write(f"{cid} {tag} {data[:600]!r}\n"); log.flush()
            writer.write(data); await writer.drain()
    except Exception as e:
        log.write(f"{cid} {tag} EXC {e!r}\n"); log.flush()
    finally:
        try:
            writer.close()
        except Exception:
            pass

async def main(lport, tport, path):
    log = open(path, "a")
    n = 0
    async def handle(cr, cw):
        nonlocal n
        n += 1
        cid = n
        log.write(f"{cid} OPEN\n"); log.flush()
        sr, sw = await asyncio.open_connection("127.0.0.1", tport)
        await asyncio.gather(pipe(cr, sw, log, "C>", cid), pipe(sr, cw, log, "S>", cid))
        log.write(f"{cid} CLOSE\n"); log.flush()
    srv = await asyncio.start_server(handle, "127.0.0.1", lport)
    async with srv:
        await srv.serve_forever()

asyncio.run(main(int(sys.argv[1]), int(sys.argv[2]), sys.argv[3]))
